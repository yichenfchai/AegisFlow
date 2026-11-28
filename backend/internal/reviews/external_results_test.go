package reviews

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/dbmigrate"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

func ingestResult(t *testing.T, s *Service, reviewID, deliveryID string, input ExternalResultInput) (*ExternalResult, bool) {
	t.Helper()
	result, created, err := s.IngestExternalResult(context.Background(), reviewID, deliveryID, input)
	if err != nil {
		t.Fatalf("ingest %s: %v", deliveryID, err)
	}
	return result, created
}

func countExternalResults(t *testing.T, s *Service, reviewID string) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM review_external_results WHERE review_id=$1`, reviewID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestIngestExternalResultAndDeliveryIdempotency(t *testing.T) {
	s, registry, db := testService(t)
	ctx := context.Background()
	for _, actor := range []auth.Actor{maintainActor, memberActor, viewerActor, outsiderActor} {
		seedUser(t, db, actor)
	}
	project := createProject(t, registry, adminActor, "external-ingest", projects.VisibilityPublic)
	review := createReview(t, s, adminActor, project.ID)

	result, created := ingestResult(t, s, review.ID, "delivery-0001", ExternalResultInput{
		Source: "scanner", Verdict: VerdictPassed, Summary: "all checks passed", URL: "https://example.com/scans/1",
	})
	if !created || result.ID == "" || result.ReviewID != review.ID || result.DeliveryID != "delivery-0001" ||
		result.Source != "scanner" || result.Verdict != "passed" || result.Summary != "all checks passed" ||
		result.URL != "https://example.com/scans/1" || result.ReceivedAt == 0 {
		t.Fatalf("ingested result = %+v created=%v", result, created)
	}

	// A retried delivery is idempotent: same stored row, no second insert, and
	// even a different payload cannot rewrite the evidence.
	retried, createdAgain := ingestResult(t, s, review.ID, "delivery-0001", ExternalResultInput{
		Source: "other-tool", Verdict: VerdictFailed, Summary: "changed",
	})
	if createdAgain || retried.ID != result.ID || retried.Source != "scanner" || retried.Verdict != "passed" {
		t.Fatalf("retried result = %+v created=%v", retried, createdAgain)
	}
	if count := countExternalResults(t, s, review.ID); count != 1 {
		t.Fatalf("stored rows = %d, want 1", count)
	}

	// Ingesting an external result never changes the review state machine.
	stored, err := s.Get(ctx, adminActor, review.ID)
	if err != nil || stored.Status != StatusPending {
		t.Fatalf("review after ingest = %+v err=%v", stored, err)
	}

	// An unknown review is not found; a malformed review id is a validation error.
	if _, _, err = s.IngestExternalResult(ctx, "missing-0000000000001", "delivery-0002", ExternalResultInput{Source: "s", Verdict: VerdictWarning}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown review err = %v", err)
	}
	if _, _, err = s.IngestExternalResult(ctx, "not-a-valid-id", "delivery-0003", ExternalResultInput{Source: "s", Verdict: VerdictWarning}); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid review id err = %v", err)
	}
}

func TestIngestExternalResultValidation(t *testing.T) {
	s, registry, _ := testService(t)
	ctx := context.Background()
	project := createProject(t, registry, adminActor, "external-validation", projects.VisibilityPublic)
	review := createReview(t, s, adminActor, project.ID)

	valid := ExternalResultInput{Source: "scanner", Verdict: VerdictPassed}
	for _, tc := range []struct {
		name     string
		delivery string
		input    ExternalResultInput
	}{
		{"source blank", "delivery-1001", ExternalResultInput{Source: "   ", Verdict: VerdictPassed}},
		{"source too long", "delivery-1002", ExternalResultInput{Source: strings.Repeat("s", 65), Verdict: VerdictPassed}},
		{"source newline", "delivery-1003", ExternalResultInput{Source: "scanner\nline", Verdict: VerdictPassed}},
		{"verdict unknown", "delivery-1004", ExternalResultInput{Source: "scanner", Verdict: "unknown"}},
		{"verdict uppercase", "delivery-1005", ExternalResultInput{Source: "scanner", Verdict: "PASSED"}},
		{"summary too long", "delivery-1006", ExternalResultInput{Source: "scanner", Verdict: VerdictPassed, Summary: strings.Repeat("x", 501)}},
		{"summary invalid utf8", "delivery-1007", ExternalResultInput{Source: "scanner", Verdict: VerdictPassed, Summary: string([]byte{0xff, 0xfe})}},
		{"url scheme", "delivery-1008", ExternalResultInput{Source: "scanner", Verdict: VerdictPassed, URL: "ftp://example.com/x"}},
		{"url userinfo", "delivery-1009", ExternalResultInput{Source: "scanner", Verdict: VerdictPassed, URL: "https://user:pass@example.com/x"}},
		{"url fragment", "delivery-1010", ExternalResultInput{Source: "scanner", Verdict: VerdictPassed, URL: "https://example.com/x#frag"}},
		{"url relative", "delivery-1011", ExternalResultInput{Source: "scanner", Verdict: VerdictPassed, URL: "/scans/1"}},
		{"url too long", "delivery-1012", ExternalResultInput{Source: "scanner", Verdict: VerdictPassed, URL: "https://example.com/" + strings.Repeat("a", 2048)}},
		{"delivery too short", "short", valid},
		{"delivery too long", strings.Repeat("d", 129), valid},
		{"delivery bad character", "delivery 0001", valid},
		{"delivery slash", "delivery/0001", valid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := s.IngestExternalResult(ctx, review.ID, tc.delivery, tc.input); !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want validation", err)
			}
		})
	}
	if count := countExternalResults(t, s, review.ID); count != 0 {
		t.Fatalf("invalid deliveries stored %d rows", count)
	}
}

func TestExternalResultSummarySanitizedAndUrlStored(t *testing.T) {
	s, registry, db := testService(t)
	project := createProject(t, registry, adminActor, "external-summary", projects.VisibilityPublic)
	review := createReview(t, s, adminActor, project.ID)

	// Newlines survive (a multi-line summary is allowed) while other control
	// characters are removed and CR/CRLF become LF.
	result, created := ingestResult(t, s, review.ID, "delivery-2001", ExternalResultInput{
		Source:  "  scanner  ",
		Verdict: VerdictWarning,
		Summary: "line one\r\nline\u0000two\n\n",
		URL:     "https://example.com/scans/2?filter=warnings",
	})
	if !created || result.Source != "scanner" || result.Summary != "line one\nlinetwo" ||
		result.URL != "https://example.com/scans/2?filter=warnings" || result.Verdict != "warning" {
		t.Fatalf("normalized result = %+v", result)
	}

	// Omitting summary and url stores empty values, and an empty url is NULL in
	// the database rather than an empty string.
	result, created = ingestResult(t, s, review.ID, "delivery-2002", ExternalResultInput{Source: "scanner", Verdict: VerdictFailed})
	if !created || result.Summary != "" || result.URL != "" {
		t.Fatalf("minimal result = %+v", result)
	}
	var urlIsNull bool
	if err := db.QueryRow(`SELECT url IS NULL FROM review_external_results WHERE id=$1`, result.ID).Scan(&urlIsNull); err != nil {
		t.Fatal(err)
	}
	if !urlIsNull {
		t.Fatal("empty url must be stored as NULL")
	}
}

func TestListExternalResultsVisibilityOrderingAndCap(t *testing.T) {
	s, registry, db := testService(t)
	ctx := context.Background()
	for _, actor := range []auth.Actor{maintainActor, memberActor, viewerActor, outsiderActor} {
		seedUser(t, db, actor)
	}
	publicProject := createProject(t, registry, adminActor, "external-public", projects.VisibilityPublic)
	privateProject := createProject(t, registry, adminActor, "external-private", projects.VisibilityPrivate)
	addMaintainer(t, registry, privateProject.ID, maintainActor)
	publicReview := createReview(t, s, adminActor, publicProject.ID)
	privateReview := createReview(t, s, adminActor, privateProject.ID)

	// Oldest first in insertion order, then stamp distinct received_at values so
	// the ordering assertion is deterministic (the column has second resolution).
	results := make([]*ExternalResult, 0, 3)
	for i, delivery := range []string{"delivery-3001", "delivery-3002", "delivery-3003"} {
		result, _ := ingestResult(t, s, publicReview.ID, delivery, ExternalResultInput{Source: "scanner", Verdict: VerdictPassed})
		results = append(results, result)
		if _, err := db.Exec(`UPDATE review_external_results SET received_at=$2 WHERE id=$1`, result.ID, 1_700_000_000+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	// Newest first: delivery-3003, delivery-3002, delivery-3001.
	list, err := s.ListExternalResults(ctx, viewerActor, publicReview.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].ID != results[2].ID || list[1].ID != results[1].ID || list[2].ID != results[0].ID {
		t.Fatalf("list order = %+v", list)
	}

	// The read is capped at 50 rows; insert the remaining rows directly so the
	// test stays fast.
	for i := 0; i < 55; i++ {
		if _, err := db.Exec(`INSERT INTO review_external_results(id,tenant_id,review_id,delivery_id,source,verdict,summary,received_at)
			VALUES($1,(SELECT tenant_id FROM reviews WHERE id=$2),$2,$3,'bulk','passed','',$4)`, "bulk-"+string(rune('a'+i%26))+string(rune('a'+i/26)), publicReview.ID,
			"delivery-bulk-"+string(rune('a'+i%26))+string(rune('a'+i/26)), 1_700_100_000+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	list, err = s.ListExternalResults(ctx, viewerActor, publicReview.ID)
	if err != nil || len(list) != maxExternalResults {
		t.Fatalf("capped list = %d err=%v", len(list), err)
	}

	// A private project's review is invisible to an outsider; the maintainer can
	// read it. An outsider cannot reach the results through a public listing.
	ingestResult(t, s, privateReview.ID, "delivery-4001", ExternalResultInput{Source: "scanner", Verdict: VerdictWarning})
	if _, err := s.ListExternalResults(ctx, outsiderActor, privateReview.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider list err = %v", err)
	}
	if _, err := s.ListExternalResults(ctx, maintainActor, privateReview.ID); err != nil {
		t.Fatalf("maintainer list: %v", err)
	}
	if _, err := s.ListExternalResults(ctx, adminActor, "missing-0000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown review err = %v", err)
	}
	if _, err := s.ListExternalResults(ctx, adminActor, "not-a-valid-id"); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid review id err = %v", err)
	}
}

// testNotificationService builds the review service with explicit notification
// options so the outbox/notifier can be exercised independently of cmd/server.
func testNotificationService(t *testing.T, options Options) (*Service, *projects.Service, *sql.DB) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	if err := dbmigrate.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	registry := projects.New(db)
	return New(db, registry, options), registry, db
}
