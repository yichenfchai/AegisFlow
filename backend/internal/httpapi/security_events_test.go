package httpapi

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/securityevents"
)

type securityEventEnvelope struct {
	Data securityevents.Event `json:"data"`
}

func parseSecurityEventHTTP(t *testing.T, body []byte) securityevents.Event {
	t.Helper()
	var envelope securityEventEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("security event payload: %s", string(body))
	}
	return envelope.Data
}

type securityEventListEnvelope struct {
	Data struct {
		Items  []securityevents.Event `json:"items"`
		Total  int                    `json:"total"`
		Limit  int                    `json:"limit"`
		Offset int                    `json:"offset"`
	} `json:"data"`
}

func parseSecurityEventListHTTP(t *testing.T, body []byte) securityEventListEnvelope {
	t.Helper()
	var envelope securityEventListEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("security event list payload: %s", string(body))
	}
	return envelope
}

func countSecurityEventAudit(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_audit WHERE action='security_event.ack' AND object_type='security_event'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestSecurityEventIngestAckAndAudit(t *testing.T) {
	h, db := ingestTestServer(t, []byte(ingestWebhookTestSecret))
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")

	body := `{"source":"waf","severity":"high","eventType":"sql_injection","summary":"blocked payload on /login","detailsUrl":"https://example.com/events/1"}`
	w := serveSignedExternalResult(t, h, "/api/v1/security-events", []byte(body), []byte(ingestWebhookTestSecret),
		time.Now().Unix(), "delivery-event-0001", nil)
	if w.Code != 201 {
		t.Fatalf("ingest: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "signature") || strings.Contains(w.Body.String(), ingestWebhookTestSecret) {
		t.Fatalf("event response leaked signature material: %s", w.Body.String())
	}
	event := parseSecurityEventHTTP(t, w.Body.Bytes())
	if event.ID == "" || event.Source != "waf" || event.Severity != "high" || event.EventType != "sql_injection" ||
		event.Status != "open" || event.DetailsURL != "https://example.com/events/1" || event.OccurredAt == 0 {
		t.Fatalf("event = %+v", event)
	}
	if event.AcknowledgedBy != nil || event.AcknowledgedAt != nil || event.AcknowledgedByName != nil {
		t.Fatalf("open event must omit acknowledgement fields: %+v", event)
	}

	// A retried delivery is idempotent: 200 + the original row.
	w = serveSignedExternalResult(t, h, "/api/v1/security-events",
		[]byte(`{"source":"waf","severity":"low","eventType":"probe","summary":"changed"}`),
		[]byte(ingestWebhookTestSecret), time.Now().Unix(), "delivery-event-0001", nil)
	if w.Code != 200 {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	if retried := parseSecurityEventHTTP(t, w.Body.Bytes()); retried.ID != event.ID || retried.Severity != "high" {
		t.Fatalf("retried event = %+v", retried)
	}
	var stored int
	if err := db.QueryRow(`SELECT COUNT(*) FROM security_events`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("stored events = %d, want 1", stored)
	}

	// Admin list and detail.
	if w = request(h, "GET", "/api/v1/security-events", "", adminToken); w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	} else if page := parseSecurityEventListHTTP(t, w.Body.Bytes()); page.Data.Total != 1 || page.Data.Items[0].ID != event.ID {
		t.Fatalf("list = %+v", page.Data)
	}
	if w = request(h, "GET", "/api/v1/security-events/"+event.ID, "", adminToken); w.Code != 200 {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/security-events?severity=high", "", adminToken); w.Code != 200 {
		t.Fatalf("severity filter: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/security-events?status=open&source=waf", "", adminToken); w.Code != 200 {
		t.Fatalf("status/source filter: %d %s", w.Code, w.Body.String())
	}
	for _, query := range []string{"severity=severe", "status=closed", "source=" + strings.Repeat("s", 65)} {
		if w = request(h, "GET", "/api/v1/security-events?"+query, "", adminToken); w.Code != 400 || errorCode(t, w) != "validation_failed" {
			t.Fatalf("invalid filter %q: %d %s", query, w.Code, w.Body.String())
		}
	}

	// Acknowledging records the admin and exactly one audit row; detail must not
	// contain the event summary.
	w = request(h, "POST", "/api/v1/security-events/"+event.ID+"/ack", `{}`, adminToken)
	if w.Code != 200 {
		t.Fatalf("ack: %d %s", w.Code, w.Body.String())
	}
	acked := parseSecurityEventHTTP(t, w.Body.Bytes())
	if acked.Status != "acknowledged" || acked.AcknowledgedBy == nil || acked.AcknowledgedByName == nil ||
		acked.AcknowledgedAt == nil || *acked.AcknowledgedByName != "admin" {
		t.Fatalf("acknowledged event = %+v", acked)
	}
	if count := countSecurityEventAudit(t, db); count != 1 {
		t.Fatalf("ack audit rows = %d, want 1", count)
	}
	var detail string
	if err := db.QueryRow(`SELECT detail FROM auth_audit WHERE action='security_event.ack'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "id="+event.ID) || !strings.Contains(detail, "severity=high") || !strings.Contains(detail, "eventType=sql_injection") {
		t.Fatalf("audit detail = %q", detail)
	}
	if strings.Contains(detail, "blocked payload") {
		t.Fatalf("audit detail leaked the summary: %q", detail)
	}

	// Repeated acknowledgement is idempotent: 200, same event, no second audit.
	w = request(h, "POST", "/api/v1/security-events/"+event.ID+"/ack", `{}`, adminToken)
	if w.Code != 200 {
		t.Fatalf("repeat ack: %d %s", w.Code, w.Body.String())
	}
	if again := parseSecurityEventHTTP(t, w.Body.Bytes()); again.ID != event.ID || again.Status != "acknowledged" {
		t.Fatalf("repeated ack = %+v", again)
	}
	if count := countSecurityEventAudit(t, db); count != 1 {
		t.Fatalf("audit rows after repeat = %d, want 1", count)
	}
	if w = request(h, "GET", "/api/v1/security-events?status=acknowledged", "", adminToken); w.Code != 200 {
		t.Fatalf("acknowledged filter: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "POST", "/api/v1/security-events/missing-0000000000001/ack", `{}`, adminToken); w.Code != 404 {
		t.Fatalf("unknown ack: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "POST", "/api/v1/security-events/not-a-valid-id/ack", `{}`, adminToken); w.Code != 400 {
		t.Fatalf("invalid ack id: %d %s", w.Code, w.Body.String())
	}
}

func TestSecurityEventIngestConflictsAndValidation(t *testing.T) {
	h, db := ingestTestServer(t, []byte(ingestWebhookTestSecret))
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	_ = adminToken

	for i, tc := range []struct {
		name string
		body string
	}{
		{"unknown field", `{"source":"waf","severity":"high","eventType":"x","summary":"s","extra":1}`},
		{"null field", `{"source":null,"severity":"high","eventType":"x","summary":"s"}`},
		{"array", `[]`},
		{"two documents", `{"source":"waf","severity":"high","eventType":"x","summary":"s"} {}`},
		{"empty source", `{"source":"","severity":"high","eventType":"x","summary":"s"}`},
		{"source multiline", `{"source":"w\nf","severity":"high","eventType":"x","summary":"s"}`},
		{"eventType empty", `{"source":"waf","severity":"high","eventType":" ","summary":"s"}`},
		{"eventType too long", `{"source":"waf","severity":"high","eventType":"` + strings.Repeat("e", 65) + `","summary":"s"}`},
		{"severity unknown", `{"source":"waf","severity":"severe","eventType":"x","summary":"s"}`},
		{"summary empty", `{"source":"waf","severity":"high","eventType":"x","summary":"  "}`},
		{"summary too long", `{"source":"waf","severity":"high","eventType":"x","summary":"` + strings.Repeat("x", 1001) + `"}`},
		{"future occurredAt", `{"source":"waf","severity":"high","eventType":"x","summary":"s","occurredAt":99999999999}`},
		{"detailsUrl scheme", `{"source":"waf","severity":"high","eventType":"x","summary":"s","detailsUrl":"ftp://example.com/x"}`},
		{"detailsUrl fragment", `{"source":"waf","severity":"high","eventType":"x","summary":"s","detailsUrl":"https://example.com/x#f"}`},
		{"malformed json", `{"source":`},
		{"oversized", `{"source":"waf","severity":"high","eventType":"x","summary":"` + strings.Repeat("x", 17<<10) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Each delivery id stays unique: 400 for the body, never a conflict.
			w := serveSignedExternalResult(t, h, "/api/v1/security-events", []byte(tc.body), []byte(ingestWebhookTestSecret),
				time.Now().Unix(), "delivery-event-bad-"+strconv.Itoa(i), nil)
			if w.Code != 400 {
				t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
			}
		})
	}
	var stored int
	if err := db.QueryRow(`SELECT COUNT(*) FROM security_events`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatalf("invalid deliveries stored %d rows", stored)
	}

	// Unconfigured secret closes the ingest endpoint with the documented message.
	unconfigured, _ := ingestTestServer(t, nil)
	w := serveSignedExternalResult(t, unconfigured, "/api/v1/security-events",
		[]byte(`{"source":"waf","severity":"high","eventType":"x","summary":"s"}`), nil, time.Now().Unix(), "", nil)
	if w.Code != 503 || errorCode(t, w) != "dependencies_unavailable" || !strings.Contains(w.Body.String(), "ingest is not configured") {
		t.Fatalf("unconfigured: %d %s", w.Code, w.Body.String())
	}

	// Authentication failures are the shared 401.
	body := []byte(`{"source":"waf","severity":"high","eventType":"x","summary":"s"}`)
	fixed := time.Now().Unix()
	for _, tc := range []struct {
		name      string
		secret    []byte
		timestamp int64
		delivery  string
	}{
		{"missing headers", nil, 0, ""},
		{"wrong secret", []byte("fedcba9876543210"), fixed, "delivery-event-auth-01"},
		{"stale timestamp", []byte(ingestWebhookTestSecret), fixed - 301, "delivery-event-auth-02"},
		{"bad delivery", []byte(ingestWebhookTestSecret), fixed, "short"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := serveSignedExternalResult(t, h, "/api/v1/security-events", body, tc.secret, tc.timestamp, tc.delivery, nil)
			if w.Code != 401 || errorCode(t, w) != "unauthorized" {
				t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
			}
		})
	}
}

func TestSecurityEventAdminOnly(t *testing.T) {
	h, _ := ingestTestServer(t, []byte(ingestWebhookTestSecret))
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	createUserHTTP(t, h, adminToken, "event-member", "member-password-01", "member")
	memberToken, _, _ := loginHTTP(t, h, "event-member", "member-password-01")
	createUserHTTP(t, h, adminToken, "event-viewer", "viewer-password-01", "viewer")
	viewerToken, _, _ := loginHTTP(t, h, "event-viewer", "viewer-password-01")
	created := serveSignedExternalResult(t, h, "/api/v1/security-events",
		[]byte(`{"source":"waf","severity":"critical","eventType":"rce","summary":"s"}`),
		[]byte(ingestWebhookTestSecret), time.Now().Unix(), "delivery-event-admin-01", nil)
	if created.Code != 201 {
		t.Fatalf("ingest: %d %s", created.Code, created.Body.String())
	}
	event := parseSecurityEventHTTP(t, created.Body.Bytes())

	for _, token := range []string{memberToken, viewerToken} {
		for _, route := range []struct{ method, path, body string }{
			{"GET", "/api/v1/security-events", ""},
			{"GET", "/api/v1/security-events/" + event.ID, ""},
			{"POST", "/api/v1/security-events/" + event.ID + "/ack", `{}`},
		} {
			if w := request(h, route.method, route.path, route.body, token); w.Code != 403 || errorCode(t, w) != "forbidden" {
				t.Fatalf("non-admin %s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
			}
		}
	}
	for _, token := range []string{""} {
		if w := request(h, "GET", "/api/v1/security-events", "", token); w.Code != 401 {
			t.Fatalf("unauthenticated list: %d %s", w.Code, w.Body.String())
		}
		if w := request(h, "POST", "/api/v1/security-events/"+event.ID+"/ack", `{}`, token); w.Code != 401 {
			t.Fatalf("unauthenticated ack: %d %s", w.Code, w.Body.String())
		}
	}
}
