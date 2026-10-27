package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/builds"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// waitForHTTPBuild polls until the build reaches one of the wanted statuses.
func waitForHTTPBuild(t *testing.T, h http.Handler, token, id string, want ...string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		w := request(h, "GET", "/api/v1/builds/"+id, "", token)
		if w.Code != 200 {
			t.Fatalf("get build: %d %s", w.Code, w.Body.String())
		}
		var payload struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		status, _ := payload.Data["status"].(string)
		for _, candidate := range want {
			if status == candidate {
				return payload.Data
			}
		}
		if status == builds.StatusSucceeded || status == builds.StatusFailed || status == builds.StatusCancelled || status == builds.StatusInterrupted {
			t.Fatalf("build settled in %s, want %v", status, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %v, status=%s", want, status)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func createBuildableProject(t *testing.T, h http.Handler, adminToken, name, command string) string {
	t.Helper()
	w := request(h, "POST", "/api/v1/projects", `{"name":"`+name+`","buildCommand":"`+command+`"}`, adminToken)
	if w.Code != 201 {
		t.Fatalf("create project: %d %s", w.Code, w.Body.String())
	}
	var project struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &project); err != nil {
		t.Fatal(err)
	}
	return project.Data.ID
}

func triggerBuild(t *testing.T, h http.Handler, adminToken, projectID, body string) string {
	t.Helper()
	w := request(h, "POST", "/api/v1/projects/"+projectID+"/builds", body, adminToken)
	if w.Code != 201 {
		t.Fatalf("trigger: %d %s", w.Code, w.Body.String())
	}
	var build struct {
		Data struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &build); err != nil || build.Data.ID == "" || build.Data.Status != builds.StatusQueued {
		t.Fatalf("trigger payload: %s", w.Body.String())
	}
	return build.Data.ID
}

func TestBuildRoutesLifecycle(t *testing.T) {
	h, _ := testServer(t)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	projectID := createBuildableProject(t, h, adminToken, "buildable", "echo built-by-http")
	commandlessID := createBuildableProject(t, h, adminToken, "commandless", "")

	// Typed validation errors.
	if w := request(h, "POST", "/api/v1/projects/"+commandlessID+"/builds", `{}`, adminToken); w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("no command: %d %s", w.Code, w.Body.String())
	}
	if w := request(h, "POST", "/api/v1/projects/missing-0000000000000001/builds", `{}`, adminToken); w.Code != 404 {
		t.Fatalf("unknown project: %d", w.Code)
	}

	buildID := triggerBuild(t, h, adminToken, projectID, `{}`)
	done := waitForHTTPBuild(t, h, adminToken, buildID, builds.StatusSucceeded)
	if done["finishedAt"].(float64) == 0 {
		t.Fatal("finishedAt not set")
	}
	// The worker commits the terminal status before it flushes its final log
	// batch (documented ordering), so poll instead of reading once.
	waitForLogs(t, h, adminToken, buildID, "built-by-http", "build succeeded")
	if w := request(h, "GET", "/api/v1/builds/"+buildID+"/logs?after=-1", "", adminToken); w.Code != 400 {
		t.Fatalf("bad cursor: %d", w.Code)
	}
	w := request(h, "GET", "/api/v1/builds/missing-0000000000000001/logs", "", adminToken)
	if w.Code != 404 {
		t.Fatalf("unknown build logs: %d", w.Code)
	}

	// List filters by project and is paginated.
	if w = request(h, "GET", "/api/v1/builds?projectId="+projectID+"&limit=10&offset=0", "", adminToken); w.Code != 200 || !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatalf("build list: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/builds?projectId=bogus-id", "", adminToken); w.Code != 400 {
		t.Fatalf("bad project filter: %d", w.Code)
	}

	// Cancel flow on a long-running build.
	if w = request(h, "PATCH", "/api/v1/projects/"+projectID, `{"buildCommand":"ping -n 30 127.0.0.1 >nul"}`, adminToken); w.Code != 200 {
		t.Fatalf("set long command: %d %s", w.Code, w.Body.String())
	}
	longID := triggerBuild(t, h, adminToken, projectID, `{"branch":"feature/x"}`)
	waitForHTTPBuild(t, h, adminToken, longID, builds.StatusRunning)
	if w = request(h, "POST", "/api/v1/builds/"+longID+"/cancel", `{}`, adminToken); w.Code != 200 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	waitForHTTPBuild(t, h, adminToken, longID, builds.StatusCancelled)
	if w = request(h, "POST", "/api/v1/builds/"+longID+"/cancel", `{}`, adminToken); w.Code != 200 {
		t.Fatalf("idempotent cancel: %d", w.Code)
	}
	if w = request(h, "POST", "/api/v1/builds/"+buildID+"/cancel", `{}`, adminToken); w.Code != 409 {
		t.Fatalf("cancel finished: %d", w.Code)
	}
}

func TestBuildRBACAndFailClosed(t *testing.T) {
	h, _ := testServer(t)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	createUserHTTP(t, h, adminToken, "build-viewer", "viewer-password-1", "viewer")
	viewerToken, _, _ := loginHTTP(t, h, "build-viewer", "viewer-password-1")
	projectID := createBuildableProject(t, h, adminToken, "rbac-build", "echo x")

	if w := request(h, "POST", "/api/v1/projects/"+projectID+"/builds", `{}`, viewerToken); w.Code != 403 {
		t.Fatalf("viewer trigger: %d", w.Code)
	}
	if w := request(h, "POST", "/api/v1/builds/whatever-0000000000000001/cancel", `{}`, viewerToken); w.Code != 403 {
		t.Fatalf("viewer cancel: %d", w.Code)
	}
	if w := request(h, "GET", "/api/v1/builds?limit=10&offset=0", "", viewerToken); w.Code != 200 {
		t.Fatalf("viewer list: %d", w.Code)
	}

	// A server without the wired pipeline fails closed like other dependencies.
	// It needs its own migrated database and a login issued by that database.
	db := testutil.OpenPostgres(t)
	registry := projects.New(db)
	bareAuth := auth.New(db, strings.Repeat("b", 32), time.Minute, time.Hour)
	if err := bareAuth.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := bareAuth.Bootstrap(context.Background(), "bare-admin", "a-strong-password"); err != nil {
		t.Fatal(err)
	}
	bare := New(bareAuth, slog.New(slog.NewTextHandler(io.Discard, nil)), db.PingContext, []string{"http://localhost:5173"}, Options{Limiter: newTestLimiter(), Projects: registry})
	bareLogin := request(bare, "POST", "/api/v1/auth/login", `{"username":"bare-admin","password":"a-strong-password"}`, "")
	if bareLogin.Code != 200 {
		t.Fatalf("bare login: %d", bareLogin.Code)
	}
	var tokens struct {
		Data struct {
			AccessToken string `json:"accessToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(bareLogin.Body.Bytes(), &tokens); err != nil {
		t.Fatal(err)
	}
	if w := request(bare, "GET", "/api/v1/builds", "", tokens.Data.AccessToken); w.Code != 503 {
		t.Fatalf("missing pipeline: %d", w.Code)
	}
}

// waitForLogs polls the log page until every wanted substring is present. The
// worker commits the terminal status before flushing its final batch, so a
// single read taken right after a terminal status can legitimately be empty.
func waitForLogs(t *testing.T, h http.Handler, token, buildID string, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	body := ""
	for {
		w := request(h, "GET", "/api/v1/builds/"+buildID+"/logs?after=0&limit=200", "", token)
		if w.Code != 200 {
			t.Fatalf("logs status: %d %s", w.Code, w.Body.String())
		}
		body = w.Body.String()
		missing := false
		for _, text := range want {
			if !strings.Contains(body, text) {
				missing = true
				break
			}
		}
		if !missing {
			return body
		}
		if time.Now().After(deadline) {
			t.Fatalf("logs missing %v after 10s: %s", want, body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
