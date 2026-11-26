package reviews

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Review notification events (P5-A E4). They share the existing approval
// webhook channel: one receiver is expected to handle approval.* and review.*
// events, and each event is also sent as the X-AegisFlow-Event header so a
// receiver can route without parsing the body first. The notification is an
// outbox row written in the same transaction as the review change; delivery is
// at-least-once and never changes review state.
const (
	EventRequested = "review.requested"
	EventApproved  = "review.approved"
	EventRejected  = "review.rejected"
	EventCancelled = "review.cancelled"
)

const (
	notificationPending   = "pending"
	notificationDelivered = "delivered"
	notificationFailed    = "failed"
)

const (
	// defaultNotificationScanInterval is how often the notifier polls the
	// outbox when nothing is due. The worker is independent of the build and
	// deployment workers, so a slow webhook never delays either pipeline.
	defaultNotificationScanInterval = time.Second
	maxNotificationAttempts         = 8
	// minNotificationClaimLease floors the claim lease. New derives the real
	// lease from the configured webhook timeout (timeout+30s), so a delivery
	// that is still running can never be re-claimed by another replica.
	minNotificationClaimLease = 15 * time.Second
	notificationResponseLimit = 4 << 10
	notificationErrorLimit    = 200
	// defaultNotificationWebhookTimeout is used when no explicit timeout is
	// configured; cmd/server always passes AEGIS_APPROVAL_WEBHOOK_TIMEOUT.
	defaultNotificationWebhookTimeout = 5 * time.Second
)

// ReviewNotification is the webhook JSON body for a review event. It contains
// the review snapshot at event time plus the project name; it never contains a
// webhook secret, an Authorization header, a session token or request headers.
// description is the review body itself (the same text project viewers can
// already read); everything else comes from the review row.
type ReviewNotification struct {
	DeliveryID string              `json:"deliveryId"`
	Event      string              `json:"event"`
	OccurredAt int64               `json:"occurredAt"`
	Project    NotificationProject `json:"project"`
	Review     NotificationReview  `json:"review"`
}

// NotificationProject identifies the project a review belongs to.
type NotificationProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// NotificationReview is the review snapshot included with an event. Decision
// fields are omitted while the review is pending; reason is omitted when the
// decision carried none (approve/cancel).
type NotificationReview struct {
	ID              string `json:"id"`
	ProjectID       string `json:"projectId"`
	Number          int64  `json:"number"`
	Subject         string `json:"subject"`
	Description     string `json:"description"`
	Version         string `json:"version"`
	Status          string `json:"status"`
	RequestedBy     string `json:"requestedBy"`
	RequestedByName string `json:"requestedByName"`
	DecidedBy       string `json:"decidedBy,omitempty"`
	DecidedByName   string `json:"decidedByName,omitempty"`
	Reason          string `json:"reason,omitempty"`
	ExternalURL     string `json:"externalUrl,omitempty"`
}

type pendingNotification struct {
	ID       string
	ReviewID string
	Event    string
	Payload  []byte
	Attempts int
}

// RunNotifier delivers review notifications until ctx is cancelled. It takes no
// worker lease: every replica may claim distinct outbox rows with
// FOR UPDATE SKIP LOCKED, so webhook latency stays out of the single build and
// deployment workers while delivery remains at-least-once. An empty webhook URL
// disables the notifier and enqueueing.
func (s *Service) RunNotifier(ctx context.Context, logger *slog.Logger) {
	if s.webhookURL == "" {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("review webhook notifications enabled")
	ticker := time.NewTicker(defaultNotificationScanInterval)
	defer ticker.Stop()
	for {
		s.deliverNotifications(ctx, logger)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// deliverNotifications drains a bounded batch. A failure is recorded on the row
// and does not block the next row; delivery failures never change review state.
func (s *Service) deliverNotifications(ctx context.Context, logger *slog.Logger) {
	for delivered := 0; delivered < 20; delivered++ {
		notification, err := s.claimNotification(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("review notification claim failed", "error", err)
			}
			return
		}
		s.deliverNotification(ctx, logger, notification)
	}
}

// claimNotification reserves one due outbox row. The lease prevents a crash
// between claiming and recording an outcome from spinning on the same row
// immediately, and it is derived from the webhook timeout (New) so it outlives
// even the slowest allowed delivery: another replica cannot re-claim a row
// while its first delivery is still running. The row remains retryable after
// the lease expires.
func (s *Service) claimNotification(ctx context.Context) (pendingNotification, error) {
	now := time.Now().Unix()
	var notification pendingNotification
	err := s.db.QueryRowContext(ctx, `
		WITH next AS (
			SELECT id FROM review_notifications
			WHERE status=$1 AND next_attempt_at<=$2 AND attempts<$3
			ORDER BY next_attempt_at,id LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		UPDATE review_notifications n
		SET next_attempt_at=$4
		FROM next WHERE n.id=next.id
		RETURNING n.id,n.review_id,n.event,n.payload,n.attempts`,
		notificationPending, now, maxNotificationAttempts, now+int64(s.notificationClaimLease/time.Second)).
		Scan(&notification.ID, &notification.ReviewID, &notification.Event, &notification.Payload, &notification.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return pendingNotification{}, sql.ErrNoRows
	}
	return notification, err
}

func (s *Service) deliverNotification(ctx context.Context, logger *slog.Logger, notification pendingNotification) {
	if err := s.postNotification(ctx, notification); err != nil {
		if ctx.Err() != nil {
			// Leave the row pending; the next process/attempt can retry it.
			return
		}
		s.recordNotificationFailure(ctx, logger, notification, err)
		return
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE review_notifications
		SET attempts=attempts+1,status=$2,delivered_at=$3,last_error='' WHERE id=$1`,
		notification.ID, notificationDelivered, time.Now().Unix()); err != nil {
		if ctx.Err() == nil {
			logger.Error("review notification delivery state update failed", "error", err)
		}
	}
}

// postNotification signs the exact payload bytes, matching the approval channel:
// X-AegisFlow-Signature is sha256=<hex HMAC-SHA256(secret, payload)> and
// X-AegisFlow-Delivery is both the header and the payload deliveryId. The
// receiver is expected to de-duplicate on that id.
func (s *Service) postNotification(ctx context.Context, notification pendingNotification) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(notification.Payload))
	if err != nil {
		return errors.New("request build failed")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "AegisFlow-review-webhook")
	request.Header.Set("X-AegisFlow-Event", notification.Event)
	request.Header.Set("X-AegisFlow-Delivery", notification.ID)
	if len(s.webhookSecret) > 0 {
		mac := hmac.New(sha256.New, s.webhookSecret)
		_, _ = mac.Write(notification.Payload)
		request.Header.Set("X-AegisFlow-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	response, err := s.webhookClient.Do(request)
	if err != nil {
		return errors.New("network error")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, notificationResponseLimit))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("http %d", response.StatusCode)
	}
	return nil
}

func (s *Service) recordNotificationFailure(ctx context.Context, logger *slog.Logger, notification pendingNotification, deliveryErr error) {
	detail := notificationError(deliveryErr)
	attempts := notification.Attempts + 1
	status := notificationPending
	nextAttempt := time.Now().Add(notificationBackoff(attempts)).Unix()
	if attempts >= maxNotificationAttempts {
		status = notificationFailed
		nextAttempt = 0
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE review_notifications
		SET attempts=$2,status=$3,next_attempt_at=$4,last_error=$5 WHERE id=$1`,
		notification.ID, attempts, status, nextAttempt, detail); err != nil {
		if ctx.Err() == nil {
			logger.Error("review notification failure state update failed", "error", err)
		}
		return
	}
	if ctx.Err() == nil {
		logger.Warn("review notification delivery failed",
			"review", notification.ReviewID, "event", notification.Event,
			"attempt", attempts, "status", status, "error", detail)
	}
}

// notificationBackoff is exponential from 10s and capped at one hour. attempts
// is the number of the attempt that is about to be scheduled.
func notificationBackoff(attempts int) time.Duration {
	delay := 10 * time.Second
	for attempt := 1; attempt < attempts; attempt++ {
		delay *= 2
		if delay >= time.Hour {
			return time.Hour
		}
	}
	return delay
}

// notificationError bounds and sanitizes a delivery error for last_error so the
// outbox never accumulates multi-line or control-character garbage.
func notificationError(err error) string {
	if err == nil {
		return ""
	}
	detail := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, err.Error())
	if len(detail) > notificationErrorLimit {
		detail = detail[:notificationErrorLimit]
	}
	return detail
}

// enqueueReviewNotification writes the outbox row in the caller's transaction,
// so the review change and its notification commit together (or not at all). It
// is a no-op when the webhook channel is disabled. The unique (review_id,event)
// key makes repeated idempotent transitions harmless, and the payload is built
// before commit so a delivered message always describes the state that
// committed.
func (s *Service) enqueueReviewNotification(ctx context.Context, tx *sql.Tx, review *Review, event string) error {
	if s.webhookURL == "" {
		return nil
	}
	deliveryID, err := newID()
	if err != nil {
		return err
	}
	var projectName string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM projects WHERE id=$1`, review.ProjectID).Scan(&projectName); err != nil {
		return err
	}
	occurredAt := review.CreatedAt
	if review.DecidedAt != nil && *review.DecidedAt > 0 {
		occurredAt = *review.DecidedAt
	}
	notification := ReviewNotification{
		DeliveryID: deliveryID,
		Event:      event,
		OccurredAt: occurredAt,
		Project:    NotificationProject{ID: review.ProjectID, Name: projectName},
		Review: NotificationReview{
			ID:              review.ID,
			ProjectID:       review.ProjectID,
			Number:          review.Number,
			Subject:         review.Subject,
			Description:     review.Description,
			Version:         review.Version,
			Status:          review.Status,
			RequestedBy:     review.RequestedBy,
			RequestedByName: review.RequestedByName,
		},
	}
	if review.DecidedBy != nil {
		notification.Review.DecidedBy = *review.DecidedBy
	}
	if review.DecidedByName != nil {
		notification.Review.DecidedByName = *review.DecidedByName
	}
	if review.Reason != nil {
		notification.Review.Reason = *review.Reason
	}
	notification.Review.ExternalURL = review.ExternalURL
	body, err := json.Marshal(notification)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO review_notifications
		(id,review_id,event,payload,status,attempts,next_attempt_at,last_error,created_at)
		VALUES($1,$2,$3,$4::jsonb,$5,0,$6,'',$6)
		ON CONFLICT (review_id,event) DO NOTHING`,
		deliveryID, review.ID, event, string(body), notificationPending, time.Now().Unix())
	return err
}
