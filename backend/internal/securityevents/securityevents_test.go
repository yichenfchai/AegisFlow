package securityevents

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/dbmigrate"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

var (
	adminActor  = auth.Actor{ID: "admin-0000000000000001", Username: "root", Role: "admin", RequestID: "req-admin"}
	memberActor = auth.Actor{ID: "member-000000000000001", Username: "mia", Role: "member", RequestID: "req-member"}
	viewerActor = auth.Actor{ID: "viewer-000000000000001", Username: "vic", Role: "viewer", RequestID: "req-viewer"}
)

func testService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	if err := dbmigrate.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return New(db), db
}

func seedUser(t *testing.T, db *sql.DB, actor auth.Actor) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO auth_users(id,username,password_hash,display_name,role)
	 VALUES($1,$2,'unused',$2,'member')`, actor.ID, actor.Username); err != nil {
		t.Fatal(err)
	}
}

func ingestEvent(t *testing.T, s *Service, deliveryID string, input IngestInput) (*Event, bool) {
	t.Helper()
	event, created, err := s.Ingest(context.Background(), deliveryID, input)
	if err != nil {
		t.Fatalf("ingest %s: %v", deliveryID, err)
	}
	return event, created
}

func countEvents(t *testing.T, s *Service) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM security_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func countAudit(t *testing.T, s *Service, action string) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM auth_audit WHERE action=$1 AND object_type='security_event'`, action).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestIngestSecurityEventAndDeliveryIdempotency(t *testing.T) {
	s, _ := testService(t)
	event, created := ingestEvent(t, s, "delivery-0001", IngestInput{
		Source: "waf", Severity: SeverityHigh, EventType: "sql_injection",
		Summary: "blocked payload on /login\nrule 942100", DetailsURL: "https://example.com/events/1",
	})
	if !created || event.ID == "" || event.Source != "waf" || event.Severity != "high" ||
		event.EventType != "sql_injection" || event.Summary != "blocked payload on /login\nrule 942100" ||
		event.Status != StatusOpen || event.DetailsURL != "https://example.com/events/1" ||
		event.OccurredAt == 0 || event.ReceivedAt == 0 {
		t.Fatalf("ingested event = %+v created=%v", event, created)
	}
	if event.AcknowledgedBy != nil || event.AcknowledgedByName != nil || event.AcknowledgedAt != nil {
		t.Fatalf("open event must omit acknowledgement fields: %+v", event)
	}

	// A retried delivery is idempotent: same stored row, no second insert, and a
	// different payload cannot rewrite the evidence.
	retried, createdAgain := ingestEvent(t, s, "delivery-0001", IngestInput{
		Source: "other", Severity: SeverityLow, EventType: "other", Summary: "changed",
	})
	if createdAgain || retried.ID != event.ID || retried.Source != "waf" || retried.Severity != SeverityHigh {
		t.Fatalf("retried event = %+v created=%v", retried, createdAgain)
	}
	if stored := countEvents(t, s); stored != 1 {
		t.Fatalf("stored rows = %d, want 1", stored)
	}

	// A default occurredAt is "now"; an explicit one is preserved.
	custom, _ := ingestEvent(t, s, "delivery-0002", IngestInput{Source: "waf", Severity: SeverityLow, EventType: "probe", Summary: "x", OccurredAt: 1_700_000_000})
	if custom.OccurredAt != 1_700_000_000 {
		t.Fatalf("occurredAt = %d, want the explicit value", custom.OccurredAt)
	}

	if _, _, err := s.Ingest(context.Background(), "short", IngestInput{Source: "waf", Severity: SeverityLow, EventType: "probe", Summary: "x"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid delivery error = %v", err)
	}
}

func TestIngestSecurityEventValidation(t *testing.T) {
	s, _ := testService(t)
	future := IngestInput{Source: "waf", Severity: SeverityLow, EventType: "probe", Summary: "x", OccurredAt: 9_999_999_999}
	for _, tc := range []struct {
		name     string
		delivery string
		input    IngestInput
	}{
		{"source blank", "delivery-1001", IngestInput{Source: "  ", Severity: SeverityHigh, EventType: "x", Summary: "s"}},
		{"source too long", "delivery-1002", IngestInput{Source: strings.Repeat("s", 65), Severity: SeverityHigh, EventType: "x", Summary: "s"}},
		{"source newline", "delivery-1003", IngestInput{Source: "w\naf", Severity: SeverityHigh, EventType: "x", Summary: "s"}},
		{"event type blank", "delivery-1004", IngestInput{Source: "waf", Severity: SeverityHigh, EventType: " ", Summary: "s"}},
		{"event type too long", "delivery-1005", IngestInput{Source: "waf", Severity: SeverityHigh, EventType: strings.Repeat("e", 65), Summary: "s"}},
		{"event type newline", "delivery-1006", IngestInput{Source: "waf", Severity: SeverityHigh, EventType: "a\nb", Summary: "s"}},
		{"severity unknown", "delivery-1007", IngestInput{Source: "waf", Severity: "severe", EventType: "x", Summary: "s"}},
		{"severity uppercase", "delivery-1008", IngestInput{Source: "waf", Severity: "HIGH", EventType: "x", Summary: "s"}},
		{"summary blank", "delivery-1009", IngestInput{Source: "waf", Severity: SeverityHigh, EventType: "x", Summary: "  \n "}},
		{"summary too long", "delivery-1010", IngestInput{Source: "waf", Severity: SeverityHigh, EventType: "x", Summary: strings.Repeat("x", 1001)}},
		{"summary invalid utf8", "delivery-1011", IngestInput{Source: "waf", Severity: SeverityHigh, EventType: "x", Summary: string([]byte{0xff})}},
		{"details url scheme", "delivery-1012", IngestInput{Source: "waf", Severity: SeverityHigh, EventType: "x", Summary: "s", DetailsURL: "ftp://example.com/x"}},
		{"details url userinfo", "delivery-1013", IngestInput{Source: "waf", Severity: SeverityHigh, EventType: "x", Summary: "s", DetailsURL: "https://user:pass@example.com/x"}},
		{"future occurredAt", "delivery-1014", future},
		{"negative occurredAt", "delivery-1015", IngestInput{Source: "waf", Severity: SeverityHigh, EventType: "x", Summary: "s", OccurredAt: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := s.Ingest(context.Background(), tc.delivery, tc.input); !errors.Is(err, ErrValidation) {
				t.Fatalf("error = %v, want ErrValidation", err)
			}
		})
	}
	if stored := countEvents(t, s); stored != 0 {
		t.Fatalf("invalid deliveries stored %d rows", stored)
	}

	// The full documented payload is accepted, trimmed where applicable.
	event, created := ingestEvent(t, s, "delivery-1100", IngestInput{
		Source: "  waf  ", Severity: SeverityCritical, EventType: "  rce  ",
		Summary: "  command injection attempt  ", DetailsURL: " https://example.com/e/2?filter=critical ",
	})
	if !created || event.Source != "waf" || event.EventType != "rce" || event.Summary != "command injection attempt" ||
		event.DetailsURL != "https://example.com/e/2?filter=critical" {
		t.Fatalf("normalized event = %+v", event)
	}
}

func TestAckStateMachineIdempotencyAndAudit(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	seedUser(t, db, adminActor)
	event, _ := ingestEvent(t, s, "delivery-2001", IngestInput{Source: "waf", Severity: SeverityHigh, EventType: "sql_injection", Summary: "secret summary text"})

	acked, err := s.Ack(ctx, adminActor, event.ID)
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if acked.Status != StatusAcknowledged || acked.AcknowledgedBy == nil || *acked.AcknowledgedBy != adminActor.ID ||
		acked.AcknowledgedByName == nil || *acked.AcknowledgedByName != adminActor.Username || acked.AcknowledgedAt == nil || *acked.AcknowledgedAt == 0 {
		t.Fatalf("acknowledged event = %+v", acked)
	}
	if count := countAudit(t, s, ActionAck); count != 1 {
		t.Fatalf("ack audit rows = %d, want 1", count)
	}
	var detail string
	if err := db.QueryRow(`SELECT detail FROM auth_audit WHERE action=$1 AND object_id=$2`, ActionAck, event.ID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	wantDetail := "id=" + event.ID + " severity=high eventType=sql_injection"
	if detail != wantDetail {
		t.Fatalf("audit detail = %q, want %q", detail, wantDetail)
	}
	if strings.Contains(detail, "secret summary text") {
		t.Fatal("audit detail must not contain the event summary")
	}

	// A repeated acknowledgement is idempotent: same event, no second audit row.
	again, err := s.Ack(ctx, adminActor, event.ID)
	if err != nil {
		t.Fatalf("repeat ack: %v", err)
	}
	if again.ID != event.ID || again.Status != StatusAcknowledged {
		t.Fatalf("repeated ack = %+v", again)
	}
	if count := countAudit(t, s, ActionAck); count != 1 {
		t.Fatalf("audit rows after repeat = %d, want 1", count)
	}

	// Unknown and malformed ids.
	if _, err := s.Ack(ctx, adminActor, "missing-0000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ack error = %v", err)
	}
	if _, err := s.Ack(ctx, adminActor, "not-a-valid-id"); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid ack id error = %v", err)
	}
}

func TestListGetAndAdminOnly(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	seedUser(t, db, adminActor)
	seedUser(t, db, memberActor)

	low, _ := ingestEvent(t, s, "delivery-3001", IngestInput{Source: "waf", Severity: SeverityLow, EventType: "probe", Summary: "low", OccurredAt: 1_700_000_000})
	high, _ := ingestEvent(t, s, "delivery-3002", IngestInput{Source: "waf", Severity: SeverityHigh, EventType: "xss", Summary: "high", OccurredAt: 1_700_000_100})
	critical, _ := ingestEvent(t, s, "delivery-3003", IngestInput{Source: "ids", Severity: SeverityCritical, EventType: "rce", Summary: "critical", OccurredAt: 1_700_000_200})

	// Newest occurrence first.
	list, total, err := s.List(ctx, adminActor, ListFilter{Limit: 20})
	if err != nil || total != 3 || len(list) != 3 || list[0].ID != critical.ID || list[1].ID != high.ID || list[2].ID != low.ID {
		t.Fatalf("list = %+v total=%d err=%v", list, total, err)
	}

	// Filter severity/status/source and pagination.
	list, total, err = s.List(ctx, adminActor, ListFilter{Severity: SeverityHigh, Limit: 20})
	if err != nil || total != 1 || len(list) != 1 || list[0].ID != high.ID {
		t.Fatalf("severity filter = %+v total=%d err=%v", list, total, err)
	}
	list, total, err = s.List(ctx, adminActor, ListFilter{Source: "ids", Limit: 20})
	if err != nil || total != 1 || list[0].ID != critical.ID {
		t.Fatalf("source filter = %+v total=%d err=%v", list, total, err)
	}
	list, total, err = s.List(ctx, adminActor, ListFilter{Limit: 1, Offset: 1})
	if err != nil || total != 3 || len(list) != 1 || list[0].ID != high.ID {
		t.Fatalf("pagination = %+v total=%d err=%v", list, total, err)
	}
	if _, err = s.Ack(ctx, adminActor, low.ID); err != nil {
		t.Fatalf("ack low: %v", err)
	}
	list, total, err = s.List(ctx, adminActor, ListFilter{Status: StatusAcknowledged, Limit: 20})
	if err != nil || total != 1 || list[0].ID != low.ID {
		t.Fatalf("status filter = %+v total=%d err=%v", list, total, err)
	}

	// Get returns the event; unknown/malformed ids are errors.
	if event, err := s.Get(ctx, adminActor, high.ID); err != nil || event.ID != high.ID {
		t.Fatalf("get = %+v err=%v", event, err)
	}
	if _, err := s.Get(ctx, adminActor, "missing-0000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown get error = %v", err)
	}
	if _, err := s.Get(ctx, adminActor, "not-a-valid-id"); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid get id error = %v", err)
	}

	// Defense data is admin-only at the service boundary too; filter validation
	// still applies to admins.
	for _, filter := range []ListFilter{
		{Severity: "severe", Limit: 20},
		{Status: "closed", Limit: 20},
		{Source: strings.Repeat("s", 65), Limit: 20},
		{Limit: 0},
		{Limit: 20, Offset: -1},
	} {
		if _, _, err := s.List(ctx, adminActor, filter); !errors.Is(err, ErrValidation) {
			t.Fatalf("List(%+v) error = %v", filter, err)
		}
	}
	for _, actor := range []auth.Actor{memberActor, viewerActor} {
		if _, _, err := s.List(ctx, actor, ListFilter{Limit: 20}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("non-admin list error = %v", err)
		}
		if _, err := s.Get(ctx, actor, high.ID); !errors.Is(err, ErrForbidden) {
			t.Fatalf("non-admin get error = %v", err)
		}
		if _, err := s.Ack(ctx, actor, high.ID); !errors.Is(err, ErrForbidden) {
			t.Fatalf("non-admin ack error = %v", err)
		}
	}
}
