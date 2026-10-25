package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
)

func TestProjectRoutesAndRBAC(t *testing.T) {
	h, s := testServer(t)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	createUserHTTP(t, h, adminToken, "proj-member", "member-password-01", "member")
	createUserHTTP(t, h, adminToken, "proj-viewer", "viewer-password-1", "viewer")
	viewerToken, _, _ := loginHTTP(t, h, "proj-viewer", "viewer-password-1")
	memberToken, _, _ := loginHTTP(t, h, "proj-member", "member-password-01")

	// Viewers read but never write the registry.
	if w := request(h, "POST", "/api/v1/projects", `{"name":"viewer-denied"}`, viewerToken); w.Code != 403 {
		t.Fatalf("viewer create: %d %s", w.Code, w.Body.String())
	}
	if w := request(h, "PATCH", "/api/v1/projects/some-00000000000000001", `{"name":"x"}`, viewerToken); w.Code != 403 {
		t.Fatalf("viewer patch: %d", w.Code)
	}
	if w := request(h, "POST", "/api/v1/projects", `{"name":"ab"}`, adminToken); w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("invalid name: %d %s", w.Code, w.Body.String())
	}
	w := request(h, "POST", "/api/v1/projects", `{"name":"web-api","description":"Main service","repoUrl":"https://github.com/example/web-api.git","defaultBranch":"main"}`, adminToken)
	if w.Code != 201 {
		t.Fatalf("create project: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.Data.ID == "" || created.Data.Status != "active" {
		t.Fatalf("created project payload: %s", w.Body.String())
	}
	if w = request(h, "POST", "/api/v1/projects", `{"name":"WEB-API"}`, adminToken); w.Code != 409 {
		t.Fatalf("duplicate project: %d", w.Code)
	}
	if w = request(h, "POST", "/api/v1/projects", `{"name":"cli-tool","repoUrl":"D:\\repos\\cli"}`, memberToken); w.Code != 201 {
		t.Fatalf("member create: %d %s", w.Code, w.Body.String())
	}

	// List with pagination and status filter.
	type projectPage struct {
		Data struct {
			Items []struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"items"`
			Total int `json:"total"`
		} `json:"data"`
	}
	w = request(h, "GET", "/api/v1/projects?limit=1&offset=0", "", viewerToken)
	var page projectPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.Data.Total != 2 || len(page.Data.Items) != 1 || page.Data.Items[0].Name != "cli-tool" {
		t.Fatalf("project page: %s", w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/projects?status=bogus", "", viewerToken); w.Code != 400 {
		t.Fatalf("bad status filter: %d", w.Code)
	}

	if w = request(h, "GET", "/api/v1/projects/"+created.Data.ID, "", viewerToken); w.Code != 200 {
		t.Fatalf("get project: %d", w.Code)
	}
	if w = request(h, "GET", "/api/v1/projects/missing-0000000000000001", "", adminToken); w.Code != 404 {
		t.Fatalf("unknown project: %d", w.Code)
	}
	if w = request(h, "GET", "/api/v1/projects/short", "", adminToken); w.Code != 400 {
		t.Fatalf("malformed project id: %d", w.Code)
	}

	// Archive semantics through the member role.
	w = request(h, "PATCH", "/api/v1/projects/"+created.Data.ID, `{"archived":true}`, memberToken)
	if w.Code != 200 {
		t.Fatalf("archive: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "PATCH", "/api/v1/projects/"+created.Data.ID, `{"description":"nope"}`, adminToken); w.Code != 409 {
		t.Fatalf("archived edit: %d", w.Code)
	}
	if w = request(h, "PATCH", "/api/v1/projects/"+created.Data.ID, `{}`, adminToken); w.Code != 400 {
		t.Fatalf("empty patch: %d", w.Code)
	}
	w = request(h, "GET", "/api/v1/projects?status=archived", "", viewerToken)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.Data.Total != 1 || page.Data.Items[0].Name != "web-api" {
		t.Fatalf("archived filter: %s", w.Body.String())
	}

	// Project operations land in the shared audit trail.
	w = request(h, "GET", "/api/v1/audit?limit=100&offset=0", "", adminToken)
	var audit struct {
		Data struct {
			Items []struct {
				Action     string `json:"action"`
				ObjectType string `json:"objectType"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &audit); err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, entry := range audit.Data.Items {
		actions[entry.Action+"|"+entry.ObjectType] = true
	}
	if !actions["project.create|project"] || !actions["project.archive|project"] {
		t.Fatalf("audit missing project actions: %v", actions)
	}

	// Without the registry wired the routes fail closed like other dependencies.
	bare := New(s, slog.New(slog.NewTextHandler(io.Discard, nil)), func(context.Context) error { return nil }, []string{"http://localhost:5173"}, Options{Limiter: newTestLimiter()})
	if w = request(bare, "GET", "/api/v1/projects", "", adminToken); w.Code != 503 {
		t.Fatalf("missing registry: %d", w.Code)
	}
}
