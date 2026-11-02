package deployments

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
)

type capturedWebhook struct {
	body    []byte
	headers http.Header
}

func webhookServer(t *testing.T, status int) (*httptest.Server, <-chan capturedWebhook) {
	t.Helper()
	received := make(chan capturedWebhook, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read webhook body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		received <- capturedWebhook{body: body, headers: r.Header.Clone()}
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server, received
}

func testNotifierLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func notificationRow(t *testing.T, s *Service, approvalID, event string) (status string, attempts int, lastError string) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT status,attempts,last_error FROM approval_notifications
		WHERE approval_id=$1 AND event=$2`, approvalID, event).Scan(&status, &attempts, &lastError); err != nil {
		t.Fatal(err)
	}
	return status, attempts, lastError
}

func countNotifications(t *testing.T, s *Service, approvalID string) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM approval_notifications WHERE approval_id=$1`, approvalID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestApprovalWebhookRequestedDeliveredAndSigned(t *testing.T) {
	secret := "0123456789abcdef"
	server, received := webhookServer(t, http.StatusNoContent)
	s, _, registry := taskService(t, Options{
		ApprovalEnvironments:   []string{EnvironmentProduction},
		ApprovalWebhookURL:     server.URL,
		ApprovalWebhookSecret:  []byte(secret),
		ApprovalWebhookTimeout: 2 * time.Second,
	})
	ctx := context.Background()
	project := taskProject(t, registry, "webhook-request")
	target := approvalTarget(t, s, "webhook-target", server.URL, EnvironmentProduction)
	deployment, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	s.deliverNotifications(ctx, testNotifierLogger())

	var captured capturedWebhook
	select {
	case captured = <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("webhook was not delivered")
	}
	if got := captured.headers.Get("X-AegisFlow-Event"); got != notificationRequested {
		t.Fatalf("event header = %q", got)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(captured.body)
	wantSignature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := captured.headers.Get("X-AegisFlow-Signature"); got != wantSignature {
		t.Fatalf("signature = %q want %q", got, wantSignature)
	}
	var notification ApprovalNotification
	if err := json.Unmarshal(captured.body, &notification); err != nil {
		t.Fatal(err)
	}
	if notification.Event != notificationRequested || notification.Approval.ID != deployment.ApprovalID ||
		notification.Approval.Status != ApprovalPending || notification.Deployment.ID != deployment.ID ||
		notification.Deployment.ProjectName != project.Name || notification.Deployment.TargetName != target.Name ||
		notification.Deployment.Status != DeployAwaitingApproval || notification.Deployment.DeploymentNumber != 1 {
		t.Fatalf("notification = %+v", notification)
	}
	if got := captured.headers.Get("X-AegisFlow-Delivery"); got != notification.DeliveryID || got == "" {
		t.Fatalf("delivery header = %q body id = %q", got, notification.DeliveryID)
	}
	if bytes.Contains(captured.body, []byte(testSecretKey)) {
		t.Fatal("webhook body leaked a deployment secret")
	}
	if status, attempts, lastError := notificationRow(t, s, deployment.ApprovalID, notificationRequested); status != notificationDelivered || attempts != 1 || lastError != "" {
		t.Fatalf("outbox row = status %q attempts %d error %q", status, attempts, lastError)
	}
}

func TestApprovalWebhookDecisionAndExpiryEvents(t *testing.T) {
	server, received := webhookServer(t, http.StatusOK)
	s, _, registry := taskService(t, Options{
		ApprovalEnvironments: []string{EnvironmentProduction},
		ApprovalWebhookURL:   server.URL,
	})
	ctx := context.Background()
	project := taskProject(t, registry, "webhook-events")
	target := approvalTarget(t, s, "events-target", server.URL, EnvironmentProduction)

	approve, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Approve(ctx, testActor, approve.ApprovalID); err != nil {
		t.Fatal(err)
	}
	// Repeating an idempotent decision must not enqueue a second event.
	if _, err = s.Approve(ctx, testActor, approve.ApprovalID); err != nil {
		t.Fatal(err)
	}
	if count := countNotifications(t, s, approve.ApprovalID); count != 2 {
		t.Fatalf("approve notifications = %d want 2", count)
	}

	reject, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Reject(ctx, testActor, reject.ApprovalID, "window"); err != nil {
		t.Fatal(err)
	}

	expire, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v3"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE approvals SET expires_at=1 WHERE id=$1`, expire.ApprovalID); err != nil {
		t.Fatal(err)
	}
	if expired, err := s.ExpireApprovals(ctx, time.Now().Unix()); err != nil || expired != 1 {
		t.Fatalf("expire = %d err=%v", expired, err)
	}

	s.deliverNotifications(ctx, testNotifierLogger())
	events := map[string]int{}
	for index := 0; index < 6; index++ {
		select {
		case captured := <-received:
			var notification ApprovalNotification
			if err := json.Unmarshal(captured.body, &notification); err != nil {
				t.Fatal(err)
			}
			events[notification.Event]++
			switch notification.Event {
			case notificationApproved:
				if notification.Deployment.Status != DeployQueued {
					t.Fatalf("approved payload = %+v", notification)
				}
			case notificationRejected:
				if notification.Deployment.Status != DeployRejected || notification.Approval.Reason != "window" {
					t.Fatalf("rejected payload = %+v", notification)
				}
			case notificationExpired:
				if notification.Deployment.Status != DeployRejected || notification.Approval.Reason != "approval expired" {
					t.Fatalf("expired payload = %+v", notification)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("only received %d webhook events: %v", index, events)
		}
	}
	if events[notificationRequested] != 3 || events[notificationApproved] != 1 ||
		events[notificationRejected] != 1 || events[notificationExpired] != 1 {
		t.Fatalf("events = %v", events)
	}
}

func TestApprovalWebhookCancelEnqueuesExpiry(t *testing.T) {
	server, received := webhookServer(t, http.StatusOK)
	s, _, registry := taskService(t, Options{
		ApprovalEnvironments: []string{EnvironmentProduction},
		ApprovalWebhookURL:   server.URL,
	})
	ctx := context.Background()
	project := taskProject(t, registry, "webhook-cancel")
	target := approvalTarget(t, s, "cancel-webhook-target", server.URL, EnvironmentProduction)
	deployment, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CancelDeployment(ctx, testActor, deployment.ID); err != nil {
		t.Fatal(err)
	}
	s.deliverNotifications(ctx, testNotifierLogger())
	events := map[string]int{}
	for index := 0; index < 2; index++ {
		select {
		case captured := <-received:
			var notification ApprovalNotification
			if err := json.Unmarshal(captured.body, &notification); err != nil {
				t.Fatal(err)
			}
			events[notification.Event]++
			if notification.Event == notificationExpired && notification.Approval.Reason != "deployment cancelled" {
				t.Fatalf("cancel payload = %+v", notification)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("only received %d cancel webhook events: %v", index, events)
		}
	}
	if events[notificationRequested] != 1 || events[notificationExpired] != 1 {
		t.Fatalf("cancel events = %v", events)
	}
}

func TestApprovalWebhookRetriesThenFails(t *testing.T) {
	server, received := webhookServer(t, http.StatusInternalServerError)
	s, _, registry := taskService(t, Options{
		ApprovalEnvironments: []string{EnvironmentProduction},
		ApprovalWebhookURL:   server.URL,
	})
	ctx := context.Background()
	project := taskProject(t, registry, "webhook-retry")
	target := approvalTarget(t, s, "retry-target", server.URL, EnvironmentProduction)
	deployment, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= maxNotificationAttempts; attempt++ {
		s.deliverNotifications(ctx, testNotifierLogger())
		select {
		case <-received:
		case <-time.After(5 * time.Second):
			t.Fatalf("attempt %d was not delivered", attempt)
		}
		if attempt < maxNotificationAttempts {
			if _, err := s.db.Exec(`UPDATE approval_notifications SET next_attempt_at=0 WHERE approval_id=$1`, deployment.ApprovalID); err != nil {
				t.Fatal(err)
			}
		}
	}
	status, attempts, lastError := notificationRow(t, s, deployment.ApprovalID, notificationRequested)
	if status != notificationFailed || attempts != maxNotificationAttempts || lastError != "http 500" {
		t.Fatalf("failed row = status %q attempts %d error %q", status, attempts, lastError)
	}
}

func TestApprovalWebhookDisabledDoesNotEnqueue(t *testing.T) {
	s, _, registry := taskService(t, Options{ApprovalEnvironments: []string{EnvironmentProduction}})
	ctx := context.Background()
	project := taskProject(t, registry, "webhook-disabled")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	target := approvalTarget(t, s, "disabled-target", upstream.URL, EnvironmentProduction)
	deployment, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if count := countNotifications(t, s, deployment.ApprovalID); count != 0 {
		t.Fatalf("disabled notifier enqueued %d rows", count)
	}
}
