package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// auditExportTestServer wires the real auth service (the only dependency of the
// audit export) and returns the database so a test can seed rows directly.
func auditExportTestServer(t *testing.T) (http.Handler, *sql.DB) {
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
	return New(s, logger, db.PingContext, []string{"http://localhost:5173"}, Options{Limiter: newTestLimiter()}), db
}

// seedAuditEntry inserts one audit row directly so the export tests pin exact
// field content (commas, quotes, newlines) without depending on which operation
// produced it.
func seedAuditEntry(t *testing.T, db *sql.DB, id string, createdAt int64, actorID, actorUsername, action, objectType, objectID, result, detail string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO auth_audit(id,created_at,actor_id,actor_username,action,object_type,object_id,result,detail,request_id)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'req-export')`,
		id, createdAt, actorID, actorUsername, action, objectType, objectID, result, detail); err != nil {
		t.Fatal(err)
	}
}

func seedExportAuditRows(t *testing.T, db *sql.DB) {
	t.Helper()
	seedAuditEntry(t, db, "audit-1", 1700000000, "admin-id", "root", "user.create", "user", "user-1", "success", "note=alpha,beta")
	seedAuditEntry(t, db, "audit-2", 1700000100, "maint-id", "mira", "project.update", "project", "project-1", "failure", `note=he said "hi"`)
	seedAuditEntry(t, db, "audit-3", 1700000200, "sys-id", "", "policy.update", "policy", "project-2", "success", "note=line1\nline2")
}

// auditCSVRecords parses an export body with the standard CSV reader, which
// handles CRLF and quoted fields exactly like a spreadsheet does.
func auditCSVRecords(t *testing.T, body []byte) [][]string {
	t.Helper()
	records, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV export: %v\nbody:\n%s", err, body)
	}
	return records
}

// TestAuditCSVEscaping pins the RFC 4180 encoding of the pure renderer,
// including the field types a report is most likely to break on.
func TestAuditCSVEscaping(t *testing.T) {
	entries := []auth.AuditEntry{
		{ID: "id-1", CreatedAt: 1700000000, ActorID: "a", ActorUsername: "root", Action: "user.create", ObjectType: "user", ObjectID: "u1", Result: "success", Detail: `comma,value`},
		{ID: "id-2", CreatedAt: 1700000100, ActorID: "a", ActorUsername: "root", Action: "user.create", ObjectType: "user", ObjectID: "u1", Result: "success", Detail: `quote "inside"`},
		{ID: "id-3", CreatedAt: 1700000200, ActorID: "a", ActorUsername: "root", Action: "user.create", ObjectType: "user", ObjectID: "u1", Result: "success", Detail: "line1\nline2"},
		{ID: "id-4", CreatedAt: 1700000300, ActorID: "a", ActorUsername: "root", Action: "user.create", ObjectType: "user", ObjectID: "u1", Result: "success", Detail: "plain"},
	}
	body := auditCSV(entries)
	// Records end with CRLF; a plain value stays unquoted while the three
	// special fields are quoted with embedded quotes doubled.
	text := string(body)
	for _, want := range []string{
		"id,createdAt,action,objectType,objectId,result,actor,detail\r\n",
		`"comma,value"`,
		`"quote ""inside"""`,
		"\"line1\nline2\"",
		"plain\r\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("CSV body missing %q:\n%s", want, text)
		}
	}
	records := auditCSVRecords(t, body)
	if len(records) != 5 {
		t.Fatalf("CSV records = %d, want header + 4 rows", len(records))
	}
	for i, entry := range entries {
		row := records[i+1]
		if len(row) != 8 {
			t.Fatalf("row %d fields = %d, want 8: %v", i, len(row), row)
		}
		if row[0] != entry.ID || row[6] != "root" || row[7] != entry.Detail {
			t.Fatalf("row %d = %v, want id=%s actor=root detail=%q", i, row, entry.ID, entry.Detail)
		}
	}
}

// TestAuditExportCSVHTTP covers the endpoint contract: admin-only access, exact
// header row, RFC 4180 round-trip, filters identical to the audit list, the
// truncation header, and the no-match case that still returns a header row.
func TestAuditExportCSVHTTP(t *testing.T) {
	h, db := auditExportTestServer(t)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	// The admin login itself writes an auth.login audit row; remove it (and any
	// bootstrap row) so the export assertions pin exactly the seeded rows.
	if _, err := db.Exec(`DELETE FROM auth_audit`); err != nil {
		t.Fatal(err)
	}
	seedExportAuditRows(t, db)

	w := request(h, "GET", "/api/v1/audit/export.csv", "", adminToken)
	if w.Code != 200 {
		t.Fatalf("export: %d %s", w.Code, w.Body.String())
	}
	if contentType := w.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/csv; charset=utf-8") {
		t.Fatalf("Content-Type = %q", contentType)
	}
	disposition := w.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disposition, `attachment; filename="audit-`) || !strings.HasSuffix(disposition, `.csv"`) {
		t.Fatalf("Content-Disposition = %q", disposition)
	}
	if w.Header().Get("X-Audit-Truncated") != "" {
		t.Fatalf("unexpected truncation header: %q", w.Header().Get("X-Audit-Truncated"))
	}
	records := auditCSVRecords(t, w.Body.Bytes())
	if len(records) != 4 {
		t.Fatalf("export rows = %d, want header + 3", len(records))
	}
	wantHeader := []string{"id", "createdAt", "action", "objectType", "objectId", "result", "actor", "detail"}
	if fmt.Sprint(records[0]) != fmt.Sprint(wantHeader) {
		t.Fatalf("header = %v, want %v", records[0], wantHeader)
	}
	// Newest first, RFC3339 UTC timestamps, actor username with an id fallback,
	// and every detail round-trips through the escaping.
	wantRows := [][]string{
		{"audit-3", "2023-11-14T22:16:40Z", "policy.update", "policy", "project-2", "success", "sys-id", "note=line1\nline2"},
		{"audit-2", "2023-11-14T22:15:00Z", "project.update", "project", "project-1", "failure", "mira", `note=he said "hi"`},
		{"audit-1", "2023-11-14T22:13:20Z", "user.create", "user", "user-1", "success", "root", "note=alpha,beta"},
	}
	for i, want := range wantRows {
		if fmt.Sprint(records[i+1]) != fmt.Sprint(want) {
			t.Fatalf("row %d = %v, want %v", i, records[i+1], want)
		}
	}
	// The export must not grow secret-bearing or request-echo columns.
	body := w.Body.String()
	for _, forbidden := range []string{"requestId", "password", "token", "cookie"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("export leaked %q: %s", forbidden, body)
		}
	}

	// Filters use the audit list semantics and narrow the exported rows.
	for name, query := range map[string]string{
		"action":     "action=user.create",
		"objectType": "objectType=project",
		"result":     "result=failure",
		"actor":      "actor=MIRA",
		"from":       "from=2023-11-14T22:13:30Z",
	} {
		w = request(h, "GET", "/api/v1/audit/export.csv?"+query, "", adminToken)
		if w.Code != 200 {
			t.Fatalf("%s filter: %d %s", name, w.Code, w.Body.String())
		}
		rows := auditCSVRecords(t, w.Body.Bytes())
		if name == "from" {
			if len(rows) != 3 {
				t.Fatalf("%s filter rows = %v", name, rows)
			}
			continue
		}
		if len(rows) != 2 {
			t.Fatalf("%s filter rows = %v", name, rows)
		}
	}
	// Date-only to includes the whole UTC day; an invalid value is rejected with
	// the same error as the list endpoint.
	if w = request(h, "GET", "/api/v1/audit/export.csv?to=2023-11-14", "", adminToken); w.Code != 200 || len(auditCSVRecords(t, w.Body.Bytes())) != 4 {
		t.Fatalf("date-only to: %d %s", w.Code, w.Body.String())
	}
	for name, query := range map[string]string{"from": "from=not-a-date", "result": "result=maybe"} {
		if w = request(h, "GET", "/api/v1/audit/export.csv?"+query, "", adminToken); w.Code != 400 {
			t.Fatalf("%s rejection: %d %s", name, w.Code, w.Body.String())
		}
	}

	// No match: a valid CSV document with only the header row.
	w = request(h, "GET", "/api/v1/audit/export.csv?action=missing.action", "", adminToken)
	if w.Code != 200 {
		t.Fatalf("no-match export: %d %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); body != auditCSVHeader+"\r\n" {
		t.Fatalf("no-match body = %q", body)
	}

	// Over the cap: the endpoint keeps the newest rows and reports truncation.
	previousLimit := auditExportLimit
	auditExportLimit = 2
	t.Cleanup(func() { auditExportLimit = previousLimit })
	w = request(h, "GET", "/api/v1/audit/export.csv", "", adminToken)
	if w.Code != 200 || w.Header().Get("X-Audit-Truncated") != "true" {
		t.Fatalf("truncated export: %d header=%q", w.Code, w.Header().Get("X-Audit-Truncated"))
	}
	if rows := auditCSVRecords(t, w.Body.Bytes()); len(rows) != 3 {
		t.Fatalf("truncated export rows = %d, want header + 2", len(rows))
	}

	// Admin only: 401 without a session and 403 for any other role.
	if w = request(h, "GET", "/api/v1/audit/export.csv", "", ""); w.Code != 401 {
		t.Fatalf("anonymous export: %d", w.Code)
	}
	createUserHTTP(t, h, adminToken, "export-viewer", "export-viewer-pass-1", "viewer")
	createUserHTTP(t, h, adminToken, "export-member", "export-member-pass-1", "member")
	for name, credentials := range map[string][2]string{
		"viewer": {"export-viewer", "export-viewer-pass-1"},
		"member": {"export-member", "export-member-pass-1"},
	} {
		token, _, _ := loginHTTP(t, h, credentials[0], credentials[1])
		if w = request(h, "GET", "/api/v1/audit/export.csv", "", token); w.Code != 403 || errorCode(t, w) != "forbidden" {
			t.Fatalf("%s export: %d %s", name, w.Code, w.Body.String())
		}
	}
}

// TestAuditExportLimitBounds pins the service-side bound so a future caller
// cannot raise the export past the documented cap.
func TestAuditExportLimitBounds(t *testing.T) {
	h, _ := auditExportTestServer(t)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	// The HTTP layer always passes the cap; this test documents that the cap is
	// exposed and that the endpoint remains a GET-only route.
	if auditExportLimit != auth.AuditExportMaxRows {
		t.Fatalf("auditExportLimit = %d, want %d", auditExportLimit, auth.AuditExportMaxRows)
	}
	if w := request(h, "POST", "/api/v1/audit/export.csv", "{}", adminToken); w.Code != 404 {
		t.Fatalf("POST export: %d, want 404", w.Code)
	}
}
