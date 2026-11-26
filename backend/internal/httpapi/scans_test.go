package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
	"github.com/yichenfchai/AegisFlow/backend/internal/reviews"
	"github.com/yichenfchai/AegisFlow/backend/internal/scans"
	"github.com/yichenfchai/AegisFlow/backend/internal/securityevents"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// ingestWebhookTestSecret is the fixed key the scan/defense-event webhook tests
// sign with; it never leaves the test process.
const ingestWebhookTestSecret = "0123456789abcdef"

// ingestTestServer wires the scan registry and defense-event store next to the
// real auth service, the project registry and the review center, exactly like
// cmd/server. secret configures AEGIS_INGEST_WEBHOOK_SECRET (nil = unconfigured).
func ingestTestServer(t *testing.T, secret []byte) (http.Handler, *sql.DB) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	s := auth.New(db, strings.Repeat("k", 32), time.Minute, time.Hour)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Bootstrap(context.Background(), "admin", "a-strong-password"); err != nil {
		t.Fatal(err)
	}
	registry := projects.New(db)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	options := Options{
		Limiter:             newTestLimiter(),
		Projects:            registry,
		Reviews:             reviews.New(db, registry),
		Scans:               scans.New(db),
		SecurityEvents:      securityevents.New(db),
		IngestWebhookSecret: secret,
	}
	return New(s, logger, db.PingContext, []string{"http://localhost:5173"}, options), db
}

type scanEnvelope struct {
	Data scans.Scan `json:"data"`
}

func parseScanHTTP(t *testing.T, body []byte) scans.Scan {
	t.Helper()
	var envelope scanEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("scan payload: %s", string(body))
	}
	return envelope.Data
}

type scanListEnvelope struct {
	Data struct {
		Items  []scans.Scan `json:"items"`
		Total  int          `json:"total"`
		Limit  int          `json:"limit"`
		Offset int          `json:"offset"`
	} `json:"data"`
}

func parseScanListHTTP(t *testing.T, body []byte) scanListEnvelope {
	t.Helper()
	var envelope scanListEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("scan list payload: %s", string(body))
	}
	return envelope
}

func countScanRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM scans`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestScanIngestHappyPathIdempotencyAndReads(t *testing.T) {
	h, db := ingestTestServer(t, []byte(ingestWebhookTestSecret))
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	projectID := createProjectHTTP(t, h, adminToken, "scan-http-happy", "public")
	path := "/api/v1/projects/" + projectID + "/scans"

	body := `{"source":"scanner","tool":"trivy","version":"0.55.0","verdict":"passed","summary":"no high findings","reportUrl":"https://example.com/scan/1"}`
	w := serveSignedExternalResult(t, h, path, []byte(body), []byte(ingestWebhookTestSecret), time.Now().Unix(), "delivery-scan-0001", nil)
	if w.Code != 201 {
		t.Fatalf("ingest: %d %s", w.Code, w.Body.String())
	}
	scan := parseScanHTTP(t, w.Body.Bytes())
	if scan.ID == "" || scan.ProjectID != projectID || scan.ProjectName != "scan-http-happy" ||
		scan.Source != "scanner" || scan.Tool != "trivy" || scan.Version != "0.55.0" ||
		scan.Verdict != "passed" || scan.Summary != "no high findings" ||
		scan.ReportURL != "https://example.com/scan/1" || scan.ReceivedAt == 0 {
		t.Fatalf("scan = %+v", scan)
	}
	if strings.Contains(w.Body.String(), "signature") || strings.Contains(w.Body.String(), ingestWebhookTestSecret) {
		t.Fatalf("scan response leaked signature material: %s", w.Body.String())
	}

	// A retried delivery is idempotent: 200 and the original row, no duplicate.
	retryBody := `{"source":"scanner","verdict":"failed","summary":"a later attempt"}`
	w = serveSignedExternalResult(t, h, path, []byte(retryBody), []byte(ingestWebhookTestSecret), time.Now().Unix(), "delivery-scan-0001", nil)
	if w.Code != 200 {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	if retried := parseScanHTTP(t, w.Body.Bytes()); retried.ID != scan.ID || retried.Verdict != "passed" {
		t.Fatalf("retried scan = %+v", retried)
	}
	if stored := countScanRows(t, db); stored != 1 {
		t.Fatalf("stored rows = %d, want 1", stored)
	}
	// The audit trail has no scan actions: ingestion is external evidence, not a
	// local state change.
	var auditRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_audit WHERE action LIKE 'scan.%'`).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if auditRows != 0 {
		t.Fatalf("scan audit rows = %d, want 0", auditRows)
	}

	// Project-scoped list: newest first with the paging envelope.
	if w = request(h, "GET", path+"?limit=20&offset=0", "", adminToken); w.Code != 200 {
		t.Fatalf("project list: %d %s", w.Code, w.Body.String())
	} else if page := parseScanListHTTP(t, w.Body.Bytes()); page.Data.Total != 1 || len(page.Data.Items) != 1 || page.Data.Items[0].ID != scan.ID {
		t.Fatalf("project list = %+v", page.Data)
	}
	// Global list and detail are authenticated read routes.
	if w = request(h, "GET", "/api/v1/scans?projectId="+projectID, "", adminToken); w.Code != 200 {
		t.Fatalf("global list: %d %s", w.Code, w.Body.String())
	} else if page := parseScanListHTTP(t, w.Body.Bytes()); page.Data.Total != 1 {
		t.Fatalf("global list = %+v", page.Data)
	}
	if w = request(h, "GET", "/api/v1/scans/"+scan.ID, "", adminToken); w.Code != 200 {
		t.Fatalf("get scan: %d %s", w.Code, w.Body.String())
	} else if got := parseScanHTTP(t, w.Body.Bytes()); got.ID != scan.ID {
		t.Fatalf("get scan = %+v", got)
	}
}

func TestScanIngestNotConfigured(t *testing.T) {
	h, _ := ingestTestServer(t, nil)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	projectID := createProjectHTTP(t, h, adminToken, "scan-http-disabled", "public")
	w := serveSignedExternalResult(t, h, "/api/v1/projects/"+projectID+"/scans",
		[]byte(`{"source":"scanner","verdict":"passed"}`), nil, time.Now().Unix(), "", nil)
	if w.Code != 503 || errorCode(t, w) != "dependencies_unavailable" {
		t.Fatalf("unconfigured: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ingest is not configured") {
		t.Fatalf("unconfigured message = %s", w.Body.String())
	}
	// The read routes stay available (empty page) without the secret.
	if w = request(h, "GET", "/api/v1/scans", "", adminToken); w.Code != 200 {
		t.Fatalf("global list without secret: %d %s", w.Code, w.Body.String())
	}
}

func TestScanIngestAuthenticationAndBodies(t *testing.T) {
	h, _ := ingestTestServer(t, []byte(ingestWebhookTestSecret))
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	projectID := createProjectHTTP(t, h, adminToken, "scan-http-auth", "public")
	path := "/api/v1/projects/" + projectID + "/scans"
	body := []byte(`{"source":"scanner","verdict":"passed"}`)
	fixed := time.Now().Unix()
	wrongSecret := []byte("fedcba9876543210")

	for _, tc := range []struct {
		name      string
		secret    []byte
		timestamp int64
		delivery  string
		mutate    func(*http.Request)
	}{
		{"missing all headers", nil, 0, "", nil},
		{"missing timestamp", []byte(ingestWebhookTestSecret), fixed, "delivery-scan-auth-0001", func(r *http.Request) { r.Header.Del("X-AegisFlow-Timestamp") }},
		{"missing signature", []byte(ingestWebhookTestSecret), fixed, "delivery-scan-auth-0002", func(r *http.Request) { r.Header.Del("X-AegisFlow-Signature") }},
		{"missing delivery", []byte(ingestWebhookTestSecret), fixed, "delivery-scan-auth-0003", func(r *http.Request) { r.Header.Del("X-AegisFlow-Delivery") }},
		{"wrong secret", wrongSecret, fixed, "delivery-scan-auth-0004", nil},
		{"malformed signature", []byte(ingestWebhookTestSecret), fixed, "delivery-scan-auth-0005", func(r *http.Request) {
			r.Header.Set("X-AegisFlow-Signature", "sha256=not-hex")
		}},
		{"stale timestamp", []byte(ingestWebhookTestSecret), fixed - 301, "delivery-scan-auth-0006", nil},
		{"future timestamp", []byte(ingestWebhookTestSecret), fixed + 301, "delivery-scan-auth-0007", nil},
		{"short delivery", []byte(ingestWebhookTestSecret), fixed, "short", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := serveSignedExternalResult(t, h, path, body, tc.secret, tc.timestamp, tc.delivery, tc.mutate)
			if w.Code != 401 || errorCode(t, w) != "unauthorized" {
				t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
			}
		})
	}

	// An unknown project is 404; a malformed project id is a validation error.
	w := serveSignedExternalResult(t, h, "/api/v1/projects/missing-0000000000001/scans", body,
		[]byte(ingestWebhookTestSecret), fixed, "delivery-scan-auth-0008", nil)
	if w.Code != 404 {
		t.Fatalf("unknown project: %d %s", w.Code, w.Body.String())
	}
	w = serveSignedExternalResult(t, h, "/api/v1/projects/not-a-valid-id/scans", body,
		[]byte(ingestWebhookTestSecret), fixed, "delivery-scan-auth-0009", nil)
	if w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("invalid project id: %d %s", w.Code, w.Body.String())
	}

	// Body validation: every malformed delivery is a 400 and stores nothing.
	delivery := 0
	for _, tc := range []struct {
		name string
		body string
	}{
		{"unknown field", `{"source":"scanner","verdict":"passed","extra":1}`},
		{"null field", `{"source":null,"verdict":"passed"}`},
		{"array", `[]`},
		{"null body", `null`},
		{"two documents", `{"source":"scanner","verdict":"passed"} {}`},
		{"empty source", `{"source":"","verdict":"passed"}`},
		{"source too long", `{"source":"` + strings.Repeat("s", 65) + `","verdict":"passed"}`},
		{"source multiline", `{"source":"scanner\nline","verdict":"passed"}`},
		{"tool too long", `{"source":"scanner","tool":"` + strings.Repeat("t", 65) + `","verdict":"passed"}`},
		{"version multiline", `{"source":"scanner","version":"1\n2","verdict":"passed"}`},
		{"verdict unknown", `{"source":"scanner","verdict":"unknown"}`},
		{"verdict uppercase", `{"source":"scanner","verdict":"PASSED"}`},
		{"summary too long", `{"source":"scanner","verdict":"passed","summary":"` + strings.Repeat("x", 1001) + `"}`},
		{"reportUrl scheme", `{"source":"scanner","verdict":"passed","reportUrl":"ftp://example.com/x"}`},
		{"reportUrl userinfo", `{"source":"scanner","verdict":"passed","reportUrl":"https://user:pass@example.com/x"}`},
		{"reportUrl fragment", `{"source":"scanner","verdict":"passed","reportUrl":"https://example.com/x#f"}`},
		{"malformed json", `{"source":`},
		{"oversized body", `{"source":"scanner","verdict":"passed","summary":"` + strings.Repeat("x", 17<<10) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delivery++
			w := serveSignedExternalResult(t, h, path, []byte(tc.body), []byte(ingestWebhookTestSecret),
				time.Now().Unix(), "delivery-scan-body-"+strconv.Itoa(delivery), nil)
			if w.Code != 400 {
				t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
			}
		})
	}
	// The minimal valid body only needs source and verdict; a multi-line summary
	// keeps its newlines while other control characters are removed.
	minimal := `{"source":"scanner","verdict":"warning","summary":"line one\r\nline\u0000two\n\n"}`
	w = serveSignedExternalResult(t, h, path, []byte(minimal), []byte(ingestWebhookTestSecret),
		time.Now().Unix(), "delivery-scan-body-ok", nil)
	if w.Code != 201 {
		t.Fatalf("minimal body: %d %s", w.Code, w.Body.String())
	}
	if scan := parseScanHTTP(t, w.Body.Bytes()); scan.Summary != "line one\nlinetwo" || scan.Tool != "" || scan.Version != "" {
		t.Fatalf("sanitized scan = %+v", scan)
	}
	// The Content-Type is enforced before the signature.
	w = serveSignedExternalResult(t, h, path, body, []byte(ingestWebhookTestSecret), fixed, "delivery-scan-auth-0010",
		func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") })
	if w.Code != 415 || errorCode(t, w) != "unsupported_media_type" {
		t.Fatalf("wrong content type: %d %s", w.Code, w.Body.String())
	}
}

func TestScanReadVisibility(t *testing.T) {
	h, _ := ingestTestServer(t, []byte(ingestWebhookTestSecret))
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	createUserHTTP(t, h, adminToken, "scan-outsider", "member-password-01", "member")
	outsiderToken, _, _ := loginHTTP(t, h, "scan-outsider", "member-password-01")
	createUserHTTP(t, h, adminToken, "scan-viewer", "viewer-password-01", "viewer")
	viewerToken, _, _ := loginHTTP(t, h, "scan-viewer", "viewer-password-01")
	publicID := createProjectHTTP(t, h, adminToken, "scan-read-public", "public")
	privateID := createProjectHTTP(t, h, adminToken, "scan-read-private", "private")

	// The trusted signer may report evidence for any existing project.
	for _, tc := range []struct{ projectID, delivery, verdict string }{
		{publicID, "delivery-read-0001", "passed"},
		{privateID, "delivery-read-0002", "failed"},
	} {
		w := serveSignedExternalResult(t, h, "/api/v1/projects/"+tc.projectID+"/scans",
			[]byte(`{"source":"scanner","verdict":"`+tc.verdict+`"}`), []byte(ingestWebhookTestSecret),
			time.Now().Unix(), tc.delivery, nil)
		if w.Code != 201 {
			t.Fatalf("ingest %s: %d %s", tc.delivery, w.Code, w.Body.String())
		}
	}

	// Private project reads are invisible to a plain member; anonymous callers
	// are rejected before any read.
	if w := request(h, "GET", "/api/v1/projects/"+privateID+"/scans", "", outsiderToken); w.Code != 404 || errorCode(t, w) != "not_found" {
		t.Fatalf("outsider private list: %d %s", w.Code, w.Body.String())
	}
	// An invisible projectId filter yields an empty page, never an error.
	w := request(h, "GET", "/api/v1/scans?projectId="+privateID, "", outsiderToken)
	if w.Code != 200 {
		t.Fatalf("outsider global filter: %d %s", w.Code, w.Body.String())
	}
	if page := parseScanListHTTP(t, w.Body.Bytes()); page.Data.Total != 0 || len(page.Data.Items) != 0 {
		t.Fatalf("invisible filter page = %+v", page.Data)
	}
	// A visible public project is readable by every authenticated role.
	if w = request(h, "GET", "/api/v1/projects/"+publicID+"/scans", "", viewerToken); w.Code != 200 {
		t.Fatalf("viewer public list: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/scans", "", viewerToken); w.Code != 200 {
		t.Fatalf("viewer global list: %d %s", w.Code, w.Body.String())
	} else if page := parseScanListHTTP(t, w.Body.Bytes()); page.Data.Total != 1 {
		t.Fatalf("viewer global page = %+v", page.Data)
	}
	if w = request(h, "GET", "/api/v1/scans", "", ""); w.Code != 401 {
		t.Fatalf("unauthenticated global list: %d %s", w.Code, w.Body.String())
	}
	// Filter validation and unknown ids.
	if w = request(h, "GET", "/api/v1/scans?verdict=unknown", "", adminToken); w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("invalid verdict filter: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/scans?limit=0", "", adminToken); w.Code != 400 {
		t.Fatalf("invalid limit: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/scans?limit=200", "", adminToken); w.Code != 400 {
		t.Fatalf("too large limit: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/scans/missing-0000000000001", "", adminToken); w.Code != 404 {
		t.Fatalf("unknown scan: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/scans/not-a-valid-id", "", adminToken); w.Code != 400 {
		t.Fatalf("invalid scan id: %d %s", w.Code, w.Body.String())
	}
}
