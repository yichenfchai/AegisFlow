package reviews

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
)

type capturedReviewWebhook struct {
	body    []byte
	headers http.Header
}

func reviewWebhookServer(t *testing.T, status int) (*httptest.Server, <-chan capturedReviewWebhook) {
	t.Helper()
	received := make(chan capturedReviewWebhook, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read webhook body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		received <- capturedReviewWebhook{body: body, headers: r.Header.Clone()}
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server, received
}

func testReviewNotifierLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func reviewNotificationRows(t *testing.T, s *Service, reviewID string) map[string]int {
	t.Helper()
	rows, err := s.db.Query(`SELECT event FROM review_notifications WHERE review_id=$1`, reviewID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var event string
		if err := rows.Scan(&event); err != nil {
			t.Fatal(err)
		}
		counts[event]++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return counts
}

func reviewNotificationRow(t *testing.T, s *Service, reviewID, event string) (status string, attempts int, lastError string) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT status,attempts,last_error FROM review_notifications
		WHERE review_id=$1 AND event=$2`, reviewID, event).Scan(&status, &attempts, &lastError); err != nil {
		t.Fatal(err)
	}
	return status, attempts, lastError
}

// TestReviewNotificationsEnqueuedForEveryEvent covers the four events and their
// same-transaction outbox rows: requested at creation plus one terminal event
// per decision, and idempotent repeats never enqueue a duplicate.
func TestReviewNotificationsEnqueuedForEveryEvent(t *testing.T) {
	const secret = "review-webhook-secret-0123456789"
	s, registry, db := testNotificationService(t, Options{WebhookURL: "https://hooks.example.com/aegis", WebhookSecret: []byte(secret)})
	ctx := context.Background()
	for _, actor := range []auth.Actor{adminActor, maintainActor, memberActor, viewerActor, outsiderActor} {
		seedUser(t, db, actor)
	}
	project := createProject(t, registry, adminActor, "notify-events", projects.VisibilityPublic)
	addMaintainer(t, registry, project.ID, maintainActor)

	approvedReview := createReview(t, s, adminActor, project.ID)
	review, err := s.Approve(ctx, maintainActor, approvedReview.ID, "ship it")
	if err != nil {
		t.Fatal(err)
	}
	// Repeating an idempotent decision must not enqueue a second event.
	if _, err = s.Approve(ctx, maintainActor, approvedReview.ID, "ship it"); err != nil {
		t.Fatal(err)
	}
	if rows := reviewNotificationRows(t, s, approvedReview.ID); rows[EventRequested] != 1 || rows[EventApproved] != 1 || len(rows) != 2 {
		t.Fatalf("approved review outbox = %v", rows)
	}

	rejectedReview := createReview(t, s, adminActor, project.ID)
	if _, err = s.Reject(ctx, maintainActor, rejectedReview.ID, "needs tests"); err != nil {
		t.Fatal(err)
	}
	cancelledReview := createReview(t, s, adminActor, project.ID)
	if _, err = s.Cancel(ctx, adminActor, cancelledReview.ID); err != nil {
		t.Fatal(err)
	}
	if rows := reviewNotificationRows(t, s, rejectedReview.ID); rows[EventRequested] != 1 || rows[EventRejected] != 1 || len(rows) != 2 {
		t.Fatalf("rejected review outbox = %v", rows)
	}
	if rows := reviewNotificationRows(t, s, cancelledReview.ID); rows[EventRequested] != 1 || rows[EventCancelled] != 1 || len(rows) != 2 {
		t.Fatalf("cancelled review outbox = %v", rows)
	}

	// The approved payload describes the committed decision and the project, and
	// never contains the signing secret or header-like credentials.
	var payload []byte
	if err = s.db.QueryRow(`SELECT payload FROM review_notifications WHERE review_id=$1 AND event=$2`,
		approvedReview.ID, EventApproved).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var notification ReviewNotification
	if err = json.Unmarshal(payload, &notification); err != nil {
		t.Fatal(err)
	}
	if notification.Event != EventApproved || notification.DeliveryID == "" || notification.OccurredAt != *review.DecidedAt ||
		notification.Project.ID != project.ID || notification.Project.Name != project.Name {
		t.Fatalf("approved payload = %+v", notification)
	}
	snapshot := notification.Review
	if snapshot.ID != review.ID || snapshot.ProjectID != project.ID || snapshot.Number != review.Number ||
		snapshot.Status != StatusApproved || snapshot.RequestedBy != adminActor.ID || snapshot.RequestedByName != adminActor.Username ||
		snapshot.DecidedBy != maintainActor.ID || snapshot.DecidedByName != maintainActor.Username ||
		snapshot.Reason != "ship it" || snapshot.Subject != review.Subject || snapshot.Description != review.Description ||
		snapshot.ExternalURL != review.ExternalURL {
		t.Fatalf("approved snapshot = %+v", snapshot)
	}
	if bytes.Contains(payload, []byte(secret)) || bytes.Contains(payload, []byte("Authorization")) {
		t.Fatalf("payload leaked sensitive material: %s", payload)
	}

	// An unconfigured webhook never writes outbox rows.
	disabled, disabledRegistry, _ := testNotificationService(t, Options{})
	disabledProject := createProject(t, disabledRegistry, adminActor, "notify-disabled", projects.VisibilityPublic)
	disabledReview := createReview(t, disabled, adminActor, disabledProject.ID)
	if rows := reviewNotificationRows(t, disabled, disabledReview.ID); len(rows) != 0 {
		t.Fatalf("disabled notifier enqueued %v", rows)
	}
}

// TestReviewNotifierDeliversSignedEvents exercises the real HTTP path: headers,
// HMAC over the exact body, payload snapshot, and the delivered outbox state.
func TestReviewNotifierDeliversSignedEvents(t *testing.T) {
	const secret = "0123456789abcdef"
	server, received := reviewWebhookServer(t, http.StatusNoContent)
	s, registry, db := testNotificationService(t, Options{
		WebhookURL: server.URL, WebhookSecret: []byte(secret), WebhookTimeout: 2 * time.Second,
	})
	seedUser(t, db, adminActor)
	ctx := context.Background()
	project := createProject(t, registry, adminActor, "notify-delivery", projects.VisibilityPublic)
	review := createReview(t, s, adminActor, project.ID)
	s.deliverNotifications(ctx, testReviewNotifierLogger())

	var requested capturedReviewWebhook
	select {
	case requested = <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("review.requested webhook was not delivered")
	}
	if got := requested.headers.Get("X-AegisFlow-Event"); got != EventRequested {
		t.Fatalf("event header = %q", got)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(requested.body)
	wantSignature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := requested.headers.Get("X-AegisFlow-Signature"); got != wantSignature {
		t.Fatalf("signature = %q want %q", got, wantSignature)
	}
	var notification ReviewNotification
	if err := json.Unmarshal(requested.body, &notification); err != nil {
		t.Fatal(err)
	}
	if notification.Event != EventRequested || notification.Review.ID != review.ID ||
		notification.Review.Status != StatusPending || notification.Project.Name != project.Name ||
		notification.Review.DecidedBy != "" || notification.Review.Reason != "" {
		t.Fatalf("requested notification = %+v", notification)
	}
	if got := requested.headers.Get("X-AegisFlow-Delivery"); got != notification.DeliveryID || got == "" {
		t.Fatalf("delivery header = %q body id = %q", got, notification.DeliveryID)
	}
	if status, attempts, lastError := reviewNotificationRow(t, s, review.ID, EventRequested); status != notificationDelivered || attempts != 1 || lastError != "" {
		t.Fatalf("requested outbox row = status %q attempts %d error %q", status, attempts, lastError)
	}

	if _, err := s.Approve(ctx, adminActor, review.ID, "ok"); err != nil {
		t.Fatal(err)
	}
	s.deliverNotifications(ctx, testReviewNotifierLogger())
	var approved capturedReviewWebhook
	select {
	case approved = <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("review.approved webhook was not delivered")
	}
	if got := approved.headers.Get("X-AegisFlow-Event"); got != EventApproved {
		t.Fatalf("approved event header = %q", got)
	}
	if err := json.Unmarshal(approved.body, &notification); err != nil {
		t.Fatal(err)
	}
	if notification.Review.Status != StatusApproved || notification.Review.DecidedByName != adminActor.Username ||
		notification.Review.Reason != "ok" || notification.OccurredAt != *notificationTime(t, s, review.ID) {
		t.Fatalf("approved notification = %+v", notification)
	}
	if status, attempts, _ := reviewNotificationRow(t, s, review.ID, EventApproved); status != notificationDelivered || attempts != 1 {
		t.Fatalf("approved outbox row = status %q attempts %d", status, attempts)
	}
}

// notificationTime reads the stored decision timestamp for payload assertions.
func notificationTime(t *testing.T, s *Service, reviewID string) *int64 {
	t.Helper()
	var decidedAt int64
	if err := s.db.QueryRow(`SELECT decided_at FROM reviews WHERE id=$1`, reviewID).Scan(&decidedAt); err != nil {
		t.Fatal(err)
	}
	return &decidedAt
}

// TestReviewNotifierRetriesThenFails mirrors the approval notifier: a failing
// receiver is retried with backoff up to the attempt cap, then the row is
// terminally failed with a sanitized last_error.
func TestReviewNotifierRetriesThenFails(t *testing.T) {
	server, received := reviewWebhookServer(t, http.StatusInternalServerError)
	s, registry, _ := testNotificationService(t, Options{WebhookURL: server.URL, WebhookTimeout: 2 * time.Second})
	ctx := context.Background()
	project := createProject(t, registry, adminActor, "notify-retry", projects.VisibilityPublic)
	review := createReview(t, s, adminActor, project.ID)
	for attempt := 1; attempt <= maxNotificationAttempts; attempt++ {
		s.deliverNotifications(ctx, testReviewNotifierLogger())
		select {
		case <-received:
		case <-time.After(5 * time.Second):
			t.Fatalf("attempt %d was not delivered", attempt)
		}
		if attempt < maxNotificationAttempts {
			if _, err := s.db.Exec(`UPDATE review_notifications SET next_attempt_at=0 WHERE review_id=$1`, review.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	status, attempts, lastError := reviewNotificationRow(t, s, review.ID, EventRequested)
	if status != notificationFailed || attempts != maxNotificationAttempts || lastError != "http 500" {
		t.Fatalf("failed row = status %q attempts %d error %q", status, attempts, lastError)
	}
}

// TestReviewNotificationClaimLeaseExceedsWebhookTimeout regression: the claim
// lease is derived from the webhook timeout so another replica cannot re-claim
// a row while its first delivery is still running.
func TestReviewNotificationClaimLeaseExceedsWebhookTimeout(t *testing.T) {
	server, _ := reviewWebhookServer(t, http.StatusNoContent)
	s, registry, _ := testNotificationService(t, Options{WebhookURL: server.URL, WebhookTimeout: 30 * time.Second})
	project := createProject(t, registry, adminActor, "notify-lease", projects.VisibilityPublic)
	createReview(t, s, adminActor, project.ID)
	before := time.Now().Unix()
	claimed, err := s.claimNotification(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var nextAttempt int64
	if err := s.db.QueryRow(`SELECT next_attempt_at FROM review_notifications WHERE id=$1`, claimed.ID).Scan(&nextAttempt); err != nil {
		t.Fatal(err)
	}
	if lease := nextAttempt - before; lease < 60 {
		t.Fatalf("claim lease = %ds, want >= 60s for a 30s webhook timeout", lease)
	}
}

// TestReviewNotificationBackoff pins the documented 10s-start, 1h-cap schedule.
func TestReviewNotificationBackoff(t *testing.T) {
	for _, tc := range []struct {
		attempts int
		want     time.Duration
	}{
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{7, 640 * time.Second},
		{8, 1280 * time.Second},
		{10, time.Hour},
		{20, time.Hour},
	} {
		if got := notificationBackoff(tc.attempts); got != tc.want {
			t.Fatalf("backoff(%d) = %s, want %s", tc.attempts, got, tc.want)
		}
	}
}
