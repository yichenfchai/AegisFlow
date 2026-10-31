package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/deployments"
)

func approvalList(t *testing.T, h http.Handler, token, query string) ([]deployments.Approval, int) {
	t.Helper()
	w := request(h, "GET", "/api/v1/approvals"+query, "", token)
	if w.Code != 200 {
		t.Fatalf("list approvals: %d %s", w.Code, w.Body.String())
	}
	var page struct {
		Data struct {
			Items []deployments.Approval `json:"items"`
			Total int                    `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("approval page: %s", w.Body.String())
	}
	return page.Data.Items, page.Data.Total
}

func parseApproval(t *testing.T, w *httptest.ResponseRecorder) deployments.Approval {
	t.Helper()
	var envelope struct {
		Data deployments.Approval `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("approval payload: %s", w.Body.String())
	}
	return envelope.Data
}

func TestApprovalRoutesAndRBAC(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "deployed\n")
	}))
	defer upstream.Close()

	h, _, service := deploymentTestServerWith(t, nil, deployments.Options{
		ApprovalEnvironments: []string{"production"},
		ApprovalTTL:          time.Hour,
	})
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	createUserHTTP(t, h, adminToken, "approval-member", "member-password-01", "member")
	createUserHTTP(t, h, adminToken, "approval-viewer", "viewer-password-1", "viewer")
	memberToken, _, _ := loginHTTP(t, h, "approval-member", "member-password-01")
	viewerToken, _, _ := loginHTTP(t, h, "approval-viewer", "viewer-password-1")

	w := request(h, "POST", "/api/v1/projects", `{"name":"approval-http"}`, adminToken)
	if w.Code != 201 {
		t.Fatalf("create project: %d %s", w.Code, w.Body.String())
	}
	var project struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &project); err != nil || project.Data.ID == "" {
		t.Fatalf("project payload: %s", w.Body.String())
	}
	projectID := project.Data.ID

	w = request(h, "POST", "/api/v1/deployment-targets", `{"name":"approval-target","environment":"production","endpoint":"`+upstream.URL+`"}`, adminToken)
	if w.Code != 201 {
		t.Fatalf("create target: %d %s", w.Code, w.Body.String())
	}
	var target struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &target); err != nil || target.Data.ID == "" {
		t.Fatalf("target payload: %s", w.Body.String())
	}
	targetID := target.Data.ID

	createDeployment := func(version string) deployments.Deployment {
		t.Helper()
		w := request(h, "POST", "/api/v1/projects/"+projectID+"/deployments",
			`{"targetId":"`+targetID+`","version":"`+version+`"}`, adminToken)
		if w.Code != 201 {
			t.Fatalf("create deployment %s: %d %s", version, w.Code, w.Body.String())
		}
		return parseDeployment(t, w)
	}

	deployment := createDeployment("v1")
	if deployment.Status != "awaiting_approval" || deployment.ApprovalID == "" {
		t.Fatalf("deployment = %+v", deployment)
	}

	// RBAC: every approval endpoint is admin-only.
	if w = request(h, "GET", "/api/v1/approvals", "", memberToken); w.Code != 403 || errorCode(t, w) != "forbidden" {
		t.Fatalf("member list: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/approvals", "", viewerToken); w.Code != 403 {
		t.Fatalf("viewer list: %d", w.Code)
	}
	if w = request(h, "POST", "/api/v1/approvals/"+deployment.ApprovalID+"/approve", `{}`, memberToken); w.Code != 403 {
		t.Fatalf("member approve: %d", w.Code)
	}
	if w = request(h, "POST", "/api/v1/approvals/"+deployment.ApprovalID+"/reject", `{}`, viewerToken); w.Code != 403 {
		t.Fatalf("viewer reject: %d", w.Code)
	}

	items, total := approvalList(t, h, adminToken, "?limit=10&offset=0")
	if total != 1 || len(items) != 1 || items[0].ID != deployment.ApprovalID || items[0].Status != "pending" || items[0].DeploymentID != deployment.ID {
		t.Fatalf("approval list = %+v total=%d", items, total)
	}
	if items, total = approvalList(t, h, adminToken, "?status=pending"); total != 1 || len(items) != 1 {
		t.Fatalf("pending filter = %+v total=%d", items, total)
	}
	if _, total = approvalList(t, h, adminToken, "?status=approved"); total != 0 {
		t.Fatalf("approved filter total=%d", total)
	}
	if w = request(h, "GET", "/api/v1/approvals?status=bogus", "", adminToken); w.Code != 400 {
		t.Fatalf("bad status: %d", w.Code)
	}
	if w = request(h, "GET", "/api/v1/approvals?limit=0", "", adminToken); w.Code != 400 {
		t.Fatalf("bad limit: %d", w.Code)
	}

	// Approve, then the worker executes the now-queued deployment.
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	go service.RunWorker(workerCtx, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	w = request(h, "POST", "/api/v1/approvals/"+deployment.ApprovalID+"/approve", `{}`, adminToken)
	if w.Code != 200 {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if approved := parseApproval(t, w); approved.Status != "approved" || approved.DecidedAt == 0 {
		t.Fatalf("approved = %+v", approved)
	}
	if w = request(h, "POST", "/api/v1/approvals/"+deployment.ApprovalID+"/approve", `{}`, adminToken); w.Code != 200 {
		t.Fatalf("repeat approve: %d", w.Code)
	}
	waitHTTPDeployment(t, h, adminToken, deployment.ID, "succeeded")

	// Reject with a reason (and without one).
	rejected := createDeployment("v2")
	w = request(h, "POST", "/api/v1/approvals/"+rejected.ApprovalID+"/reject", `{"reason":"ticket-1"}`, adminToken)
	if w.Code != 200 {
		t.Fatalf("reject: %d %s", w.Code, w.Body.String())
	}
	if got := parseApproval(t, w); got.Status != "rejected" || got.Reason != "ticket-1" {
		t.Fatalf("rejected approval = %+v", got)
	}
	if w = request(h, "GET", "/api/v1/deployments/"+rejected.ID, "", adminToken); w.Code != 200 {
		t.Fatalf("get rejected deployment: %d", w.Code)
	} else if got := parseDeployment(t, w); got.Status != "rejected" || got.Detail != "approval rejected" {
		t.Fatalf("rejected deployment = %+v", got)
	}
	reasonless := createDeployment("v3")
	if w = request(h, "POST", "/api/v1/approvals/"+reasonless.ApprovalID+"/reject", `{}`, adminToken); w.Code != 200 {
		t.Fatalf("reasonless reject: %d %s", w.Code, w.Body.String())
	}
	if got := parseApproval(t, w); got.Status != "rejected" || got.Reason != "" {
		t.Fatalf("reasonless approval = %+v", got)
	}

	// Cancelling an awaiting deployment expires its approval; approval decisions
	// afterwards conflict.
	cancelled := createDeployment("v4")
	if w = request(h, "POST", "/api/v1/deployments/"+cancelled.ID+"/cancel", `{}`, adminToken); w.Code != 200 {
		t.Fatalf("cancel awaiting: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "POST", "/api/v1/approvals/"+cancelled.ApprovalID+"/approve", `{}`, adminToken); w.Code != 409 || errorCode(t, w) != "conflict" {
		t.Fatalf("approve after cancel: %d %s", w.Code, w.Body.String())
	}
	if items, total = approvalList(t, h, adminToken, "?status=expired"); total != 1 || items[0].Reason != "deployment cancelled" {
		t.Fatalf("expired filter = %+v total=%d", items, total)
	}

	// Unknown, malformed and already-decided approvals map to the shared envelope.
	if w = request(h, "POST", "/api/v1/approvals/missing-0000000000000001/approve", `{}`, adminToken); w.Code != 404 {
		t.Fatalf("unknown approve: %d", w.Code)
	}
	if w = request(h, "POST", "/api/v1/approvals/short/approve", `{}`, adminToken); w.Code != 400 {
		t.Fatalf("malformed approve: %d", w.Code)
	}
	if w = request(h, "POST", "/api/v1/approvals/"+rejected.ApprovalID+"/approve", `{}`, adminToken); w.Code != 409 {
		t.Fatalf("approve rejected approval: %d", w.Code)
	}
}
