package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/knowledge"
	"github.com/yichenfchai/AegisFlow/backend/internal/secrets"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// knowledgeSecretsTestServer wires the knowledge base and the independent
// secret store next to the real auth service, like cmd/server. secretKey
// configures the store (nil = unconfigured).
func knowledgeSecretsTestServer(t *testing.T, secretKey []byte) (http.Handler, *sql.DB) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	s := auth.New(db, strings.Repeat("k", 32), time.Minute, time.Hour)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Bootstrap(context.Background(), "admin", "a-strong-password"); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	options := Options{
		Limiter:   newTestLimiter(),
		Knowledge: knowledge.New(db),
		Secrets:   secrets.New(db, secrets.Options{SecretKey: secretKey}),
	}
	return New(s, logger, db.PingContext, []string{"http://localhost:5173"}, options), db
}

type knowledgeEnvelope struct {
	Data knowledge.Document `json:"data"`
}

type knowledgePageEnvelope struct {
	Data struct {
		Items  []knowledge.Doc `json:"items"`
		Total  int             `json:"total"`
		Limit  int             `json:"limit"`
		Offset int             `json:"offset"`
	} `json:"data"`
}

func parseKnowledge(t *testing.T, body []byte) knowledge.Document {
	t.Helper()
	var envelope knowledgeEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("knowledge payload: %s", string(body))
	}
	return envelope.Data
}

func TestKnowledgeHTTPCRUDSearchAndAudit(t *testing.T) {
	h, db := knowledgeSecretsTestServer(t, nil)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	markdown := "# 发布手册\n\n步骤：**先备份**，再执行 `rollback`。\n<script>alert(1)</script>"

	w := request(h, "POST", "/api/v1/knowledge",
		`{"title":"发布手册","body":"`+strings.ReplaceAll(strings.ReplaceAll(markdown, `"`, `\"`), "\n", `\n`)+`","tags":" ops, runbook ,, security "}`,
		adminToken)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	doc := parseKnowledge(t, w.Body.Bytes())
	if doc.ID == "" || doc.Title != "发布手册" || doc.Tags != "ops,runbook,security" || doc.Body != markdown {
		t.Fatalf("created doc = %+v", doc)
	}

	// The list never carries bodies, including the embedded XSS payload.
	w = request(h, "GET", "/api/v1/knowledge?limit=20&offset=0", "", adminToken)
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	raw := w.Body.String()
	if strings.Contains(raw, `"body"`) || strings.Contains(raw, "script") {
		t.Fatalf("list leaked bodies: %s", raw)
	}
	var page knowledgePageEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Data.Total != 1 || page.Data.Items[0].ID != doc.ID || page.Data.Limit != 20 {
		t.Fatalf("page = %+v", page.Data)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM knowledge_docs WHERE body LIKE '%script%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("stored bodies containing script = %d, want the payload stored verbatim", count)
	}

	// Title search matches; a wildcard-only search stays literal.
	for _, query := range []string{"/api/v1/knowledge?q=%E5%8F%91%E5%B8%83", "/api/v1/knowledge?q=%25"} {
		w = request(h, "GET", query, "", adminToken)
		if w.Code != 200 {
			t.Fatalf("search %s: %d %s", query, w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
	}
	if page.Data.Total != 0 {
		t.Fatalf("literal %% search matched %d documents", page.Data.Total)
	}

	// Detail returns the body verbatim.
	if w = request(h, "GET", "/api/v1/knowledge/"+doc.ID, "", adminToken); w.Code != 200 {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	if fetched := parseKnowledge(t, w.Body.Bytes()); fetched.Body != markdown {
		t.Fatalf("detail body = %q", fetched.Body)
	}

	// Update replaces the content; the audit row never contains the body.
	if w = request(h, "PUT", "/api/v1/knowledge/"+doc.ID,
		`{"title":"发布手册 v2","body":"updated **body**","tags":"ops"}`, adminToken); w.Code != 200 {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	if updated := parseKnowledge(t, w.Body.Bytes()); updated.Title != "发布手册 v2" || updated.Body != "updated **body**" {
		t.Fatalf("updated = %+v", updated)
	}

	rows, err := db.Query(`SELECT action,detail FROM auth_audit WHERE object_type='knowledge_doc' ORDER BY created_at,id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var action, detail string
		if err := rows.Scan(&action, &detail); err != nil {
			t.Fatal(err)
		}
		seen[action] = true
		if !strings.Contains(detail, doc.ID) || !strings.Contains(detail, "title=") {
			t.Fatalf("audit detail = %q", detail)
		}
		if strings.Contains(detail, "script") || strings.Contains(detail, "backup") || strings.Contains(detail, "updated **body**") {
			t.Fatalf("audit detail leaked the body: %q", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !seen[knowledge.ActionCreate] || !seen[knowledge.ActionUpdate] {
		t.Fatalf("audit actions = %+v", seen)
	}

	// Validation and not-found shapes.
	for _, tc := range []struct{ path, body string }{
		{"/api/v1/knowledge", `{"title":"","body":"x"}`},
		{"/api/v1/knowledge", `{"title":"ok","body":"` + strings.Repeat("b", (100<<10)+1) + `"}`},
		{"/api/v1/knowledge?q=" + strings.Repeat("q", 201), ""},
	} {
		method := "GET"
		if tc.body != "" {
			method = "POST"
		}
		w = request(h, method, tc.path, tc.body, adminToken)
		if w.Code != 400 || errorCode(t, w) != "validation_failed" {
			t.Fatalf("%s %s: %d %s", method, tc.path, w.Code, w.Body.String())
		}
	}
	// Page bounds are rejected by the shared pagination parser.
	if w = request(h, "GET", "/api/v1/knowledge?limit=0", "", adminToken); w.Code != 400 || errorCode(t, w) != "invalid_request" {
		t.Fatalf("limit=0: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/knowledge/missing-0000000000001", "", adminToken); w.Code != 404 {
		t.Fatalf("missing detail: %d %s", w.Code, w.Body.String())
	}

	// member may read, may not write; the route guard answers 403.
	memberID := createUserHTTP(t, h, adminToken, "knowledge-member", "member-password-01", "member")
	_ = memberID
	memberToken, _, _ := loginHTTP(t, h, "knowledge-member", "member-password-01")
	if w = request(h, "GET", "/api/v1/knowledge", "", memberToken); w.Code != 200 {
		t.Fatalf("member list: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/knowledge/"+doc.ID, "", memberToken); w.Code != 200 {
		t.Fatalf("member detail: %d %s", w.Code, w.Body.String())
	}
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/v1/knowledge", `{"title":"nope","body":"x"}`},
		{"PUT", "/api/v1/knowledge/" + doc.ID, `{"title":"nope","body":"x"}`},
		{"DELETE", "/api/v1/knowledge/" + doc.ID, ""},
	} {
		if w = request(h, tc.method, tc.path, tc.body, memberToken); w.Code != 403 {
			t.Fatalf("member %s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}

	if w = request(h, "DELETE", "/api/v1/knowledge/"+doc.ID, "", adminToken); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/knowledge/"+doc.ID, "", adminToken); w.Code != 404 {
		t.Fatalf("detail after delete: %d %s", w.Code, w.Body.String())
	}
}

func TestKnowledgeHTTPUnwired(t *testing.T) {
	h, _ := testServer(t)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/v1/knowledge", ""},
		{"GET", "/api/v1/knowledge/know-0000000000001", ""},
		{"POST", "/api/v1/knowledge", `{"title":"t","body":"b"}`},
	} {
		w := request(h, tc.method, tc.path, tc.body, "")
		if w.Code != 401 {
			t.Fatalf("unauthenticated %s %s: %d", tc.method, tc.path, w.Code)
		}
	}
	h, _ = testServer(t)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/v1/knowledge", ""},
		{"POST", "/api/v1/knowledge", `{"title":"t","body":"b"}`},
	} {
		w := request(h, tc.method, tc.path, tc.body, adminToken)
		if w.Code != 503 || errorCode(t, w) != "dependencies_unavailable" {
			t.Fatalf("unwired %s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}
