package deployments

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

// Approval notification events. They are stored in the transactional outbox and
// also sent as the X-AegisFlow-Event header, so receivers can route without
// parsing the body first.
const (
	notificationRequested = "approval.requested"
	notificationApproved  = "approval.approved"
	notificationRejected  = "approval.rejected"
	notificationExpired   = "approval.expired"
)

const (
	notificationPending   = "pending"
	notificationDelivered = "delivered"
	notificationFailed    = "failed"
)

const (
	// defaultNotificationScanInterval is how often a notifier polls the outbox
	// when no delivery is due. The worker is separate from the deployment lease
	// holder, so a slow webhook never delays deployments.
	defaultNotificationScanInterval = time.Second
	maxNotificationAttempts         = 8
	notificationClaimLease          = 15 * time.Second
	notificationResponseLimit       = 4 << 10
	notificationErrorLimit          = 200
)

// ApprovalNotification is the webhook JSON body. It deliberately contains no
// target credential, deployment secret, request body or token. Usernames are
// included for human-readable receivers; the IDs remain the stable identifiers.
type ApprovalNotification struct {
	DeliveryID          string                 `json:"deliveryId"`
	Event               string                 `json:"event"`
	OccurredAt          int64                  `json:"occurredAt"`
	Approval            Approval               `json:"approval"`
	Deployment          NotificationDeployment `json:"deployment"`
	RequestedByUsername string                 `json:"requestedByUsername,omitempty"`
	DecidedByUsername   string                 `json:"decidedByUsername,omitempty"`
}

// NotificationDeployment is the deployment snapshot included with an approval
// event. It is built inside the same transaction as the approval change, so the
// message describes the state at event time rather than a later mutation.
type NotificationDeployment struct {
	ID               string `json:"id"`
	ProjectID        string `json:"projectId"`
	ProjectName      string `json:"projectName"`
	DeploymentNumber int64  `json:"deploymentNumber"`
	TargetName       string `json:"targetName"`
	Environment      string `json:"environment"`
	Version          string `json:"version"`
	Status           string `json:"status"`
	RollbackOf       string `json:"rollbackOf,omitempty"`
}

type pendingNotification struct {
	ID         string
	ApprovalID string
	Event      string
	Payload    []byte
	Attempts   int
}

// RunNotifier delivers approval notifications until ctx is cancelled. It does
// not take the deployment worker lease: every replica may claim different
// outbox rows with FOR UPDATE SKIP LOCKED, which keeps webhook latency out of
// the single deployment worker and still gives at-least-once delivery.
func (s *Service) RunNotifier(ctx context.Context, logger *slog.Logger) {
	if s.webhookURL == "" {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("approval webhook notifications enabled")
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
// and does not block the next row; delivery failures never change approval or
// deployment state.
func (s *Service) deliverNotifications(ctx context.Context, logger *slog.Logger) {
	for delivered := 0; delivered < 20; delivered++ {
		notification, err := s.claimNotification(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("approval notification claim failed", "error", err)
			}
			return
		}
		s.deliverNotification(ctx, logger, notification)
	}
}

// claimNotification reserves one due outbox row. The lease prevents a crash
// between claiming and recording an outcome from spinning on the same row
// immediately; the row remains retryable after the lease expires.
func (s *Service) claimNotification(ctx context.Context) (pendingNotification, error) {
	now := time.Now().Unix()
	var notification pendingNotification
	err := s.db.QueryRowContext(ctx, `
		WITH next AS (
			SELECT id FROM approval_notifications
			WHERE status=$1 AND next_attempt_at<=$2 AND attempts<$3
			ORDER BY next_attempt_at,id LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		UPDATE approval_notifications n
		SET next_attempt_at=$4
		FROM next WHERE n.id=next.id
		RETURNING n.id,n.approval_id,n.event,n.payload,n.attempts`,
		notificationPending, now, maxNotificationAttempts, now+int64(notificationClaimLease/time.Second)).
		Scan(&notification.ID, &notification.ApprovalID, &notification.Event, &notification.Payload, &notification.Attempts)
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
	if _, err := s.db.ExecContext(ctx, `UPDATE approval_notifications
		SET attempts=attempts+1,status=$2,delivered_at=$3,last_error='' WHERE id=$1`,
		notification.ID, notificationDelivered, time.Now().Unix()); err != nil {
		if ctx.Err() == nil {
			logger.Error("approval notification delivery state update failed", "error", err)
		}
	}
}

func (s *Service) postNotification(ctx context.Context, notification pendingNotification) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(notification.Payload))
	if err != nil {
		return errors.New("request build failed")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "AegisFlow-approval-webhook")
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
	if _, err := s.db.ExecContext(ctx, `UPDATE approval_notifications
		SET attempts=$2,status=$3,next_attempt_at=$4,last_error=$5 WHERE id=$1`,
		notification.ID, attempts, status, nextAttempt, detail); err != nil {
		if ctx.Err() == nil {
			logger.Error("approval notification failure state update failed", "error", err)
		}
		return
	}
	if ctx.Err() == nil {
		logger.Warn("approval notification delivery failed",
			"approval", notification.ApprovalID, "event", notification.Event,
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

// enqueueApprovalNotification writes the outbox row in the caller's transaction.
// It is a no-op when notifications are disabled. The unique key makes duplicate
// idempotent transitions harmless, and the payload is built before commit so a
// sent message always describes the state that committed.
func (s *Service) enqueueApprovalNotification(ctx context.Context, tx *sql.Tx, approval Approval, event string) error {
	if s.webhookURL == "" {
		return nil
	}
	deliveryID, err := newID()
	if err != nil {
		return err
	}
	occurredAt := approval.CreatedAt
	if approval.DecidedAt > 0 {
		occurredAt = approval.DecidedAt
	}
	notification := ApprovalNotification{
		DeliveryID: deliveryID,
		Event:      event,
		OccurredAt: occurredAt,
		Approval:   approval,
		Deployment: NotificationDeployment{
			ID:        approval.DeploymentID,
			ProjectID: approval.ProjectID,
		},
	}
	err = tx.QueryRowContext(ctx, `
		SELECT p.name,d.deployment_number,d.target_name,d.target_environment,d.version,d.status,d.rollback_of,
		       COALESCE(requester.username,''),COALESCE(decider.username,'')
		FROM deployments d
		JOIN projects p ON p.id=d.project_id
		LEFT JOIN auth_users requester ON requester.id=$2
		LEFT JOIN auth_users decider ON decider.id=$3
		WHERE d.id=$1`, approval.DeploymentID, approval.RequestedBy, approval.DecidedBy).
		Scan(&notification.Deployment.ProjectName, &notification.Deployment.DeploymentNumber,
			&notification.Deployment.TargetName, &notification.Deployment.Environment,
			&notification.Deployment.Version, &notification.Deployment.Status,
			&notification.Deployment.RollbackOf, &notification.RequestedByUsername, &notification.DecidedByUsername)
	if err != nil {
		return err
	}
	body, err := json.Marshal(notification)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO approval_notifications
		(id,approval_id,event,payload,status,attempts,next_attempt_at,delivered_at,last_error,created_at)
		VALUES($1,$2,$3,$4::jsonb,$5,0,$6,0,'',$6)
		ON CONFLICT (approval_id,event) DO NOTHING`,
		deliveryID, approval.ID, event, string(body), notificationPending, time.Now().Unix())
	return err
}
