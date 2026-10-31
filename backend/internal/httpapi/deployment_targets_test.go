package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/deployments"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// deploymentTestServer wires the deployment-target registry next to the real
// auth service, the way cmd/server does, and starts no build worker (these
// routes never touch it).
func deploymentTestServer(t *testing.T, client *http.Client) (http.Handler, *auth.Service) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	s := auth.New(db, strings.Repeat("k", 32), time.Minute, time.Hour)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Bootstrap(context.Background(), "admin", "a-strong-password"); err != nil {
		t.Fatal(err)
	}
	registry := deployments.New(db, deployments.Options{
		SecretKey:    bytes.Repeat([]byte{9}, 32),
		CheckTimeout: 2 * time.Second,
		HTTPClient:   client,
	})
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(s, logger, db.PingContext, []string{"http://localhost:5173"}, Options{Limiter: newTestLimiter(), Deployments: registry}), s
}

func TestDeploymentTargetRoutesAndRBAC(t *testing.T) {
	var probes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if probes.Add(1) == 1 {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	h, authService := deploymentTestServer(t, nil)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	createUserHTTP(t, h, adminToken, "deploy-member", "member-password-01", "member")
	createUserHTTP(t, h, adminToken, "deploy-viewer", "viewer-password-1", "viewer")
	memberToken, _, _ := loginHTTP(t, h, "deploy-member", "member-password-01")
	viewerToken, _, _ := loginHTTP(t, h, "deploy-viewer", "viewer-password-1")

	createBody := `{"name":"prod-web","kind":"http","environment":"production","endpoint":"` + upstream.URL + `","healthPath":"/healthz","secret":"probe-token"}`
	if w := request(h, "POST", "/api/v1/deployment-targets", createBody, memberToken); w.Code != 403 || errorCode(t, w) != "forbidden" {
		t.Fatalf("member create: %d %s", w.Code, w.Body.String())
	}
	if w := request(h, "GET", "/api/v1/deployment-targets", "", viewerToken); w.Code != 403 || errorCode(t, w) != "forbidden" {
		t.Fatalf("viewer list: %d %s", w.Code, w.Body.String())
	}
	if w := request(h, "POST", "/api/v1/deployment-targets", createBody, viewerToken); w.Code != 403 {
		t.Fatalf("viewer create: %d", w.Code)
	}

	w := request(h, "POST", "/api/v1/deployment-targets", `{"name":"ab","endpoint":"https://example.com"}`, adminToken)
	if w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("invalid create: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "POST", "/api/v1/deployment-targets", `{"name":"bad-endpoint","endpoint":"https://user:pw@example.com"}`, adminToken); w.Code != 400 {
		t.Fatalf("userinfo endpoint: %d %s", w.Code, w.Body.String())
	}

	w = request(h, "POST", "/api/v1/deployment-targets", createBody, adminToken)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data deployments.Target `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	target := created.Data
	if target.ID == "" || target.Name != "prod-web" || !target.HasSecret || target.Status != "active" || target.Environment != "production" {
		t.Fatalf("created target = %+v", target)
	}
	if strings.Contains(w.Body.String(), "probe-token") {
		t.Fatalf("create response leaked the secret: %s", w.Body.String())
	}

	if w = request(h, "POST", "/api/v1/deployment-targets", createBody, adminToken); w.Code != 409 {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
	}

	w = request(h, "GET", "/api/v1/deployment-targets?limit=10&offset=0", "", adminToken)
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var page struct {
		Data struct {
			Items []deployments.Target `json:"items"`
			Total int                  `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.Data.Total != 1 || len(page.Data.Items) != 1 || page.Data.Items[0].ID != target.ID {
		t.Fatalf("list payload: %s", w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/deployment-targets?limit=0", "", adminToken); w.Code != 400 {
		t.Fatalf("bad limit: %d", w.Code)
	}

	if w = request(h, "GET", "/api/v1/deployment-targets/"+target.ID, "", adminToken); w.Code != 200 {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/deployment-targets/missing-0000000000000001", "", adminToken); w.Code != 404 {
		t.Fatalf("unknown get: %d", w.Code)
	}
	if w = request(h, "GET", "/api/v1/deployment-targets/short", "", adminToken); w.Code != 400 {
		t.Fatalf("malformed id: %d", w.Code)
	}

	w = request(h, "PATCH", "/api/v1/deployment-targets/"+target.ID, `{"status":"disabled","healthPath":"/ready"}`, adminToken)
	if w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	var patched struct {
		Data deployments.Target `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &patched); err != nil || patched.Data.Status != "disabled" || patched.Data.HealthPath != "/ready" || !patched.Data.HasSecret {
		t.Fatalf("patch payload: %s", w.Body.String())
	}
	if w = request(h, "PATCH", "/api/v1/deployment-targets/"+target.ID, `{}`, adminToken); w.Code != 400 {
		t.Fatalf("empty patch: %d", w.Code)
	}

	// The probe hits the real upstream: 200, then 500.
	w = request(h, "POST", "/api/v1/deployment-targets/"+target.ID+"/check", `{}`, adminToken)
	if w.Code != 200 {
		t.Fatalf("check: %d %s", w.Code, w.Body.String())
	}
	var checked struct {
		Data deployments.CheckResult `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &checked); err != nil || !checked.Data.OK || checked.Data.StatusCode != 200 || checked.Data.Detail != "ok" {
		t.Fatalf("healthy check payload: %s", w.Body.String())
	}
	w = request(h, "POST", "/api/v1/deployment-targets/"+target.ID+"/check", `{}`, adminToken)
	if err := json.Unmarshal(w.Body.Bytes(), &checked); err != nil || checked.Data.OK || checked.Data.StatusCode != 500 || checked.Data.Detail != "http 500" {
		t.Fatalf("unhealthy check payload: %s", w.Body.String())
	}
	if w = request(h, "POST", "/api/v1/deployment-targets/missing-0000000000000001/check", `{}`, adminToken); w.Code != 404 {
		t.Fatalf("unknown check: %d", w.Code)
	}
	if w = request(h, "POST", "/api/v1/deployment-targets/"+target.ID+"/check", `{}`, memberToken); w.Code != 403 {
		t.Fatalf("member check: %d", w.Code)
	}

	if w = request(h, "DELETE", "/api/v1/deployment-targets/"+target.ID, "", adminToken); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/deployment-targets/"+target.ID, "", adminToken); w.Code != 404 {
		t.Fatalf("get after delete: %d", w.Code)
	}
	if w = request(h, "DELETE", "/api/v1/deployment-targets/missing-0000000000000001", "", adminToken); w.Code != 404 {
		t.Fatalf("unknown delete: %d", w.Code)
	}

	// Without the registry wired the routes fail closed like other dependencies.
	bare := New(authService, slog.New(slog.NewTextHandler(io.Discard, nil)), func(context.Context) error { return nil }, []string{"http://localhost:5173"}, Options{Limiter: newTestLimiter()})
	if w = request(bare, "GET", "/api/v1/deployment-targets", "", adminToken); w.Code != 503 {
		t.Fatalf("missing registry: %d", w.Code)
	}
}
