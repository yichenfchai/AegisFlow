package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/deployments"
)

type deploymentEnvelope struct {
	Data deployments.Deployment `json:"data"`
}

func parseDeployment(t *testing.T, w *httptest.ResponseRecorder) deployments.Deployment {
	t.Helper()
	var envelope deploymentEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("deployment payload: %s", w.Body.String())
	}
	return envelope.Data
}

func waitHTTPDeployment(t *testing.T, h http.Handler, token, id string, want ...string) deployments.Deployment {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		w := request(h, "GET", "/api/v1/deployments/"+id, "", token)
		if w.Code != 200 {
			t.Fatalf("get deployment: %d %s", w.Code, w.Body.String())
		}
		deployment := parseDeployment(t, w)
		for _, status := range want {
			if deployment.Status == status {
				return deployment
			}
		}
		if deployment.Status == "succeeded" || deployment.Status == "failed" || deployment.Status == "cancelled" || deployment.Status == "interrupted" || deployment.Status == "rejected" {
			t.Fatalf("deployment settled in %s, want %v", deployment.Status, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %v, status=%s", want, deployment.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func deploymentEventItems(t *testing.T, h http.Handler, token, id, query string) []deployments.DeploymentEvent {
	t.Helper()
	w := request(h, "GET", "/api/v1/deployments/"+id+"/events"+query, "", token)
	if w.Code != 200 {
		t.Fatalf("events: %d %s", w.Code, w.Body.String())
	}
	var page struct {
		Data struct {
			Items []deployments.DeploymentEvent `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("events payload: %s", w.Body.String())
	}
	return page.Data.Items
}

func TestDeploymentTaskRoutesAndRBAC(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/rollout" {
			t.Errorf("unexpected deploy request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "deployed ok\n")
	}))
	defer upstream.Close()

	h, _, service := deploymentTestServer(t, nil)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	createUserHTTP(t, h, adminToken, "task-member", "member-password-01", "member")
	createUserHTTP(t, h, adminToken, "task-viewer", "viewer-password-1", "viewer")
	memberToken, _, _ := loginHTTP(t, h, "task-member", "member-password-01")
	viewerToken, _, _ := loginHTTP(t, h, "task-viewer", "viewer-password-1")

	// Project and target through the real endpoints.
	w := request(h, "POST", "/api/v1/projects", `{"name":"deploy-http"}`, adminToken)
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

	w = request(h, "POST", "/api/v1/deployment-targets", `{"name":"http-target","environment":"production","endpoint":"`+upstream.URL+`","deployPath":"/rollout"}`, adminToken)
	if w.Code != 201 {
		t.Fatalf("create target: %d %s", w.Code, w.Body.String())
	}
	var target struct {
		Data struct {
			ID         string `json:"id"`
			DeployPath string `json:"deployPath"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &target); err != nil || target.Data.ID == "" || target.Data.DeployPath != "/rollout" {
		t.Fatalf("target payload: %s", w.Body.String())
	}
	targetID := target.Data.ID

	// RBAC: every deployment endpoint is admin-only.
	if w = request(h, "POST", "/api/v1/projects/"+projectID+"/deployments", `{"targetId":"`+targetID+`","version":"v1"}`, memberToken); w.Code != 403 || errorCode(t, w) != "forbidden" {
		t.Fatalf("member create: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/projects/"+projectID+"/deployments", "", viewerToken); w.Code != 403 {
		t.Fatalf("viewer list: %d", w.Code)
	}
	if w = request(h, "GET", "/api/v1/deployments/missing-0000000000000001", "", memberToken); w.Code != 403 {
		t.Fatalf("member get: %d", w.Code)
	}
	if w = request(h, "GET", "/api/v1/deployments/missing-0000000000000001/events", "", viewerToken); w.Code != 403 {
		t.Fatalf("viewer events: %d", w.Code)
	}
	if w = request(h, "POST", "/api/v1/deployments/missing-0000000000000001/cancel", `{}`, memberToken); w.Code != 403 {
		t.Fatalf("member cancel: %d", w.Code)
	}
	if w = request(h, "POST", "/api/v1/deployments/missing-0000000000000001/rollback", `{}`, viewerToken); w.Code != 403 {
		t.Fatalf("viewer rollback: %d", w.Code)
	}

	// Validation.
	if w = request(h, "POST", "/api/v1/projects/"+projectID+"/deployments", `{"targetId":"`+targetID+`","version":"bad version"}`, adminToken); w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("bad version: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "POST", "/api/v1/projects/"+projectID+"/deployments", `{"targetId":"missing-0000000000000001","version":"v1"}`, adminToken); w.Code != 400 {
		t.Fatalf("unknown target: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "POST", "/api/v1/projects/missing-0000000000000001/deployments", `{"targetId":"`+targetID+`","version":"v1"}`, adminToken); w.Code != 404 {
		t.Fatalf("unknown project: %d %s", w.Code, w.Body.String())
	}

	// Cancellation before any worker exists: queued -> cancelled, idempotent.
	w = request(h, "POST", "/api/v1/projects/"+projectID+"/deployments", `{"targetId":"`+targetID+`","version":"v0"}`, adminToken)
	if w.Code != 201 {
		t.Fatalf("create cancelled: %d %s", w.Code, w.Body.String())
	}
	queued := parseDeployment(t, w)
	if queued.Status != "queued" || queued.DeploymentNumber != 1 {
		t.Fatalf("queued deployment = %+v", queued)
	}
	if w = request(h, "POST", "/api/v1/deployments/"+queued.ID+"/cancel", `{}`, adminToken); w.Code != 200 {
		t.Fatalf("cancel queued: %d %s", w.Code, w.Body.String())
	}
	if got := parseDeployment(t, w); got.Status != "cancelled" || got.FinishedAt == 0 {
		t.Fatalf("cancelled deployment = %+v", got)
	}
	if w = request(h, "POST", "/api/v1/deployments/"+queued.ID+"/cancel", `{}`, adminToken); w.Code != 200 {
		t.Fatalf("repeat cancel: %d", w.Code)
	}

	// Start the single leased worker for the execution assertions.
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	go service.RunWorker(workerCtx, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	w = request(h, "POST", "/api/v1/projects/"+projectID+"/deployments", `{"targetId":"`+targetID+`","version":"v1.0.0"}`, adminToken)
	if w.Code != 201 {
		t.Fatalf("create v1: %d %s", w.Code, w.Body.String())
	}
	first := parseDeployment(t, w)
	first = waitHTTPDeployment(t, h, adminToken, first.ID, "succeeded")
	if first.StartedAt == 0 || first.FinishedAt == 0 || first.TargetName != "http-target" || first.TargetEnvironment != "production" || first.Detail != "" {
		t.Fatalf("succeeded deployment = %+v", first)
	}

	w = request(h, "POST", "/api/v1/projects/"+projectID+"/deployments", `{"targetId":"`+targetID+`","version":"v2.0.0"}`, adminToken)
	if w.Code != 201 {
		t.Fatalf("create v2: %d %s", w.Code, w.Body.String())
	}
	second := parseDeployment(t, w)
	second = waitHTTPDeployment(t, h, adminToken, second.ID, "succeeded")
	if second.DeploymentNumber != first.DeploymentNumber+1 {
		t.Fatalf("numbering = %d then %d", first.DeploymentNumber, second.DeploymentNumber)
	}

	// Project list, status filter and paging validation.
	w = request(h, "GET", "/api/v1/projects/"+projectID+"/deployments?limit=10&offset=0", "", adminToken)
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var page struct {
		Data struct {
			Items []deployments.Deployment `json:"items"`
			Total int                      `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.Data.Total != 3 || len(page.Data.Items) != 3 {
		t.Fatalf("list payload: %s", w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/projects/"+projectID+"/deployments?status=succeeded", "", adminToken); w.Code != 200 {
		t.Fatalf("filtered list: %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.Data.Total != 2 {
		t.Fatalf("filtered payload: %s", w.Body.String())
	}
	// The admin-wide list mirrors the build list: optional projectId/status
	// filters, and it is admin-only like every other deployment endpoint.
	if w = request(h, "GET", "/api/v1/deployments?limit=10&offset=0", "", adminToken); w.Code != 200 {
		t.Fatalf("global list: %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.Data.Total != 3 {
		t.Fatalf("global list payload: %s", w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/deployments?projectId="+projectID+"&status=cancelled", "", adminToken); w.Code != 200 {
		t.Fatalf("global filtered list: %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.Data.Total != 1 {
		t.Fatalf("global filtered payload: %s", w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/deployments?status=bogus", "", adminToken); w.Code != 400 {
		t.Fatalf("global invalid status: %d", w.Code)
	}
	if w = request(h, "GET", "/api/v1/deployments", "", memberToken); w.Code != 403 {
		t.Fatalf("member global list: %d", w.Code)
	}
	if w = request(h, "GET", "/api/v1/deployments", "", viewerToken); w.Code != 403 {
		t.Fatalf("viewer global list: %d", w.Code)
	}
	if w = request(h, "GET", "/api/v1/projects/"+projectID+"/deployments?status=bogus", "", adminToken); w.Code != 400 {
		t.Fatalf("bad status: %d", w.Code)
	}
	if w = request(h, "GET", "/api/v1/deployments/short", "", adminToken); w.Code != 400 {
		t.Fatalf("malformed id: %d", w.Code)
	}
	if w = request(h, "GET", "/api/v1/deployments/missing-0000000000000001", "", adminToken); w.Code != 404 {
		t.Fatalf("unknown deployment: %d", w.Code)
	}

	// Events cursor paging. Wait for the tail flush first.
	var events []deployments.DeploymentEvent
	deadline := time.Now().Add(10 * time.Second)
	for {
		events = deploymentEventItems(t, h, adminToken, first.ID, "")
		if len(events) >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(events) < 3 {
		t.Fatalf("expected lifecycle and response events, got %+v", events)
	}
	after := deploymentEventItems(t, h, adminToken, first.ID, "?after="+strconv.FormatInt(events[0].ID, 10))
	if len(after) != len(events)-1 {
		t.Fatalf("after cursor = %d events, want %d", len(after), len(events)-1)
	}
	if w = request(h, "GET", "/api/v1/deployments/"+first.ID+"/events?after=-1", "", adminToken); w.Code != 400 {
		t.Fatalf("negative cursor: %d", w.Code)
	}
	if w = request(h, "GET", "/api/v1/deployments/"+first.ID+"/events?limit=0", "", adminToken); w.Code != 400 {
		t.Fatalf("bad limit: %d", w.Code)
	}
	if w = request(h, "GET", "/api/v1/deployments/missing-0000000000000001/events", "", adminToken); w.Code != 404 {
		t.Fatalf("unknown events: %d", w.Code)
	}

	// Rollback v2 -> the earlier succeeded v1.0.0.
	w = request(h, "POST", "/api/v1/deployments/"+second.ID+"/rollback", `{}`, adminToken)
	if w.Code != 201 {
		t.Fatalf("rollback: %d %s", w.Code, w.Body.String())
	}
	rollback := parseDeployment(t, w)
	if rollback.Version != "v1.0.0" || rollback.RollbackOf != second.ID || rollback.Status != "queued" {
		t.Fatalf("rollback deployment = %+v", rollback)
	}
	rollback = waitHTTPDeployment(t, h, adminToken, rollback.ID, "succeeded")

	// The earliest deployment has no earlier success; success is not cancellable.
	if w = request(h, "POST", "/api/v1/deployments/"+first.ID+"/rollback", `{}`, adminToken); w.Code != 400 {
		t.Fatalf("rollback earliest: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "POST", "/api/v1/deployments/"+first.ID+"/cancel", `{}`, adminToken); w.Code != 409 {
		t.Fatalf("cancel succeeded: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "POST", "/api/v1/deployments/missing-0000000000000001/rollback", `{}`, adminToken); w.Code != 404 {
		t.Fatalf("rollback unknown: %d", w.Code)
	}
	if w = request(h, "GET", "/api/v1/deployments/"+rollback.ID, "", adminToken); w.Code != 200 {
		t.Fatalf("get rollback: %d", w.Code)
	}
}
