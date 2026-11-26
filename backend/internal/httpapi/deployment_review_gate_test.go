package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yichenfchai/AegisFlow/backend/internal/deployments"
)

// createDeploymentTargetHTTP registers one target through the real endpoint, so
// review-gate tests use the same target shape as production.
func createDeploymentTargetHTTP(t *testing.T, h http.Handler, token, name, environment, endpoint, deployPath string) string {
	t.Helper()
	body := `{"name":"` + name + `","environment":"` + environment + `","endpoint":"` + endpoint + `","deployPath":"` + deployPath + `"}`
	w := request(h, "POST", "/api/v1/deployment-targets", body, token)
	if w.Code != 201 {
		t.Fatalf("create target %s: %d %s", name, w.Code, w.Body.String())
	}
	var payload struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil || payload.Data.ID == "" {
		t.Fatalf("target payload: %s", w.Body.String())
	}
	return payload.Data.ID
}

// approveReviewHTTP decides a pending review as the global admin.
func approveReviewHTTP(t *testing.T, h http.Handler, token, reviewID string) {
	t.Helper()
	if w := request(h, "POST", "/api/v1/reviews/"+reviewID+"/approve", `{}`, token); w.Code != 200 {
		t.Fatalf("approve review %s: %d %s", reviewID, w.Code, w.Body.String())
	}
}

func deploymentCreateHTTP(t *testing.T, h http.Handler, token, projectID, targetID, version, reviewID string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"targetId":"` + targetID + `","version":"` + version + `"`
	if reviewID != "" {
		body += `,"reviewId":"` + reviewID + `"`
	}
	body += `}`
	return request(h, "POST", "/api/v1/projects/"+projectID+"/deployments", body, token)
}

// TestDeploymentRoutesEnforceReviewGate covers the HTTP contract of the opt-in
// review gate: missing, pending, cross-project, unknown and malformed reviews
// are 400 validation_failed, an approved same-project review is snapshotted on
// the deployment, strict JSON rejects unknown fields, the gate is per target
// environment, and rollbacks enforce the same rule.
func TestDeploymentRoutesEnforceReviewGate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "deployed\n")
	}))
	defer upstream.Close()

	h, _, service := deploymentTestServerWith(t, nil, deployments.Options{
		RequireReviewEnvironments: []string{"production"},
		ApprovalEnvironments:      []string{},
	})
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	projectID := createProjectHTTP(t, h, adminToken, "gate-http", "public")
	otherID := createProjectHTTP(t, h, adminToken, "gate-http-other", "public")
	prodTarget := createDeploymentTargetHTTP(t, h, adminToken, "gate-prod", "production", upstream.URL, "/rollout")
	devTarget := createDeploymentTargetHTTP(t, h, adminToken, "gate-dev", "development", upstream.URL, "/rollout")

	// Missing review on a gated environment is rejected before anything is
	// enqueued.
	if w := deploymentCreateHTTP(t, h, adminToken, projectID, prodTarget, "v1", ""); w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("missing review: %d %s", w.Code, w.Body.String())
	}
	pending := createReviewHTTP(t, h, adminToken, projectID, "pending change")
	if w := deploymentCreateHTTP(t, h, adminToken, projectID, prodTarget, "v1", pending.ID); w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("pending review: %d %s", w.Code, w.Body.String())
	}

	approved := createReviewHTTP(t, h, adminToken, projectID, "approved change")
	approveReviewHTTP(t, h, adminToken, approved.ID)
	w := deploymentCreateHTTP(t, h, adminToken, projectID, prodTarget, "v1", approved.ID)
	if w.Code != 201 {
		t.Fatalf("gated create: %d %s", w.Code, w.Body.String())
	}
	first := parseDeployment(t, w)
	if first.ReviewID != approved.ID || first.Status != "queued" {
		t.Fatalf("gated deployment = %+v", first)
	}

	// A review of another project, an unknown review and a malformed id are all
	// the same validation error, so review ids cannot be used to probe projects.
	otherReview := createReviewHTTP(t, h, adminToken, otherID, "other project change")
	approveReviewHTTP(t, h, adminToken, otherReview.ID)
	for name, reviewID := range map[string]string{
		"cross-project": otherReview.ID,
		"unknown":       "missing-0000000000000001",
		"malformed":     "short",
	} {
		if w := deploymentCreateHTTP(t, h, adminToken, projectID, prodTarget, "v2", reviewID); w.Code != 400 || errorCode(t, w) != "validation_failed" {
			t.Fatalf("%s review: %d %s", name, w.Code, w.Body.String())
		}
	}
	// The strict decoder still rejects unknown JSON fields alongside reviewId.
	w = request(h, "POST", "/api/v1/projects/"+projectID+"/deployments",
		`{"targetId":"`+prodTarget+`","version":"v2","reviewId":"`+approved.ID+`","extra":true}`, adminToken)
	if w.Code != 400 || errorCode(t, w) != "invalid_request" {
		t.Fatalf("unknown field: %d %s", w.Code, w.Body.String())
	}

	// The gate is per target environment: development needs no review, but a
	// review supplied there is still validated.
	if w := deploymentCreateHTTP(t, h, adminToken, projectID, devTarget, "v2", ""); w.Code != 201 {
		t.Fatalf("ungated development create: %d %s", w.Code, w.Body.String())
	} else if dev := parseDeployment(t, w); dev.ReviewID != "" {
		t.Fatalf("ungated deployment reviewId = %q, want empty", dev.ReviewID)
	}
	if w := deploymentCreateHTTP(t, h, adminToken, projectID, devTarget, "v3", pending.ID); w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("pending review on ungated environment: %d %s", w.Code, w.Body.String())
	}

	// Execute the production deployments, then exercise the rollback gate.
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	go service.RunWorker(workerCtx, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	waitHTTPDeployment(t, h, adminToken, first.ID, "succeeded")
	secondReview := createReviewHTTP(t, h, adminToken, projectID, "second change")
	approveReviewHTTP(t, h, adminToken, secondReview.ID)
	w = deploymentCreateHTTP(t, h, adminToken, projectID, prodTarget, "v1.1", secondReview.ID)
	if w.Code != 201 {
		t.Fatalf("gated second create: %d %s", w.Code, w.Body.String())
	}
	second := parseDeployment(t, w)
	waitHTTPDeployment(t, h, adminToken, second.ID, "succeeded")

	if w := request(h, "POST", "/api/v1/deployments/"+second.ID+"/rollback", `{}`, adminToken); w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("rollback without review: %d %s", w.Code, w.Body.String())
	}
	if w := request(h, "POST", "/api/v1/deployments/"+second.ID+"/rollback", `{"reviewId":"`+otherReview.ID+`"}`, adminToken); w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("rollback with cross-project review: %d %s", w.Code, w.Body.String())
	}
	rollbackReview := createReviewHTTP(t, h, adminToken, projectID, "rollback change")
	approveReviewHTTP(t, h, adminToken, rollbackReview.ID)
	w = request(h, "POST", "/api/v1/deployments/"+second.ID+"/rollback", `{"reviewId":"`+rollbackReview.ID+`"}`, adminToken)
	if w.Code != 201 {
		t.Fatalf("gated rollback: %d %s", w.Code, w.Body.String())
	}
	rollback := parseDeployment(t, w)
	if rollback.ReviewID != rollbackReview.ID || rollback.RollbackOf != second.ID || rollback.Version != "v1" {
		t.Fatalf("rollback deployment = %+v", rollback)
	}
	// Unknown fields in the rollback body are rejected as well.
	if w := request(h, "POST", "/api/v1/deployments/"+second.ID+"/rollback", `{"reviewId":"`+rollbackReview.ID+`","extra":1}`, adminToken); w.Code != 400 || errorCode(t, w) != "invalid_request" {
		t.Fatalf("rollback unknown field: %d %s", w.Code, w.Body.String())
	}
}

// TestDeploymentReviewGateComposesWithApproval pins the two-gate composition at
// the HTTP layer: review first (400 when missing), then the unchanged approval
// state machine (awaiting_approval), then execution after approval.
func TestDeploymentReviewGateComposesWithApproval(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "deployed\n")
	}))
	defer upstream.Close()

	h, _, service := deploymentTestServerWith(t, nil, deployments.Options{
		RequireReviewEnvironments: []string{"production"},
		ApprovalEnvironments:      []string{"production"},
	})
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	projectID := createProjectHTTP(t, h, adminToken, "gate-approval-http", "public")
	targetID := createDeploymentTargetHTTP(t, h, adminToken, "gate-approval-target", "production", upstream.URL, "/rollout")

	if w := deploymentCreateHTTP(t, h, adminToken, projectID, targetID, "v1", ""); w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("missing review with approval enabled: %d %s", w.Code, w.Body.String())
	}
	review := createReviewHTTP(t, h, adminToken, projectID, "release change")
	approveReviewHTTP(t, h, adminToken, review.ID)
	w := deploymentCreateHTTP(t, h, adminToken, projectID, targetID, "v1", review.ID)
	if w.Code != 201 {
		t.Fatalf("gated create: %d %s", w.Code, w.Body.String())
	}
	deployment := parseDeployment(t, w)
	if deployment.Status != "awaiting_approval" || deployment.ApprovalID == "" || deployment.ReviewID != review.ID {
		t.Fatalf("gated deployment with approval = %+v", deployment)
	}

	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	go service.RunWorker(workerCtx, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if w = request(h, "POST", "/api/v1/approvals/"+deployment.ApprovalID+"/approve", `{}`, adminToken); w.Code != 200 {
		t.Fatalf("approve deployment: %d %s", w.Code, w.Body.String())
	}
	executed := waitHTTPDeployment(t, h, adminToken, deployment.ID, "succeeded")
	if executed.ReviewID != review.ID {
		t.Fatalf("executed deployment reviewId = %q, want %q", executed.ReviewID, review.ID)
	}
}
