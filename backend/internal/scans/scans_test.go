package scans

import (
	"context"
	"database/sql"
	"errors"
	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/dbmigrate"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
	"github.com/yichenfchai/AegisFlow/backend/internal/tenants"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
	"strings"
	"testing"
)

// Actors covering the global roles plus one project outsider.
var (
	adminActor    = auth.Actor{TenantID: tenants.DefaultID, ID: "admin-0000000000000001", Username: "root", Role: "admin", RequestID: "req-admin"}
	maintainActor = auth.Actor{TenantID: tenants.DefaultID, ID: "maint-0000000000000001", Username: "mira", Role: "member", RequestID: "req-maint"}
	viewerActor   = auth.Actor{TenantID: tenants.DefaultID, ID: "viewer-000000000000001", Username: "vic", Role: "viewer", RequestID: "req-viewer"}
	outsiderActor = auth.Actor{TenantID: tenants.DefaultID, ID: "outsider-0000000000001", Username: "otto", Role: "member", RequestID: "req-outsider"}
)

func testService(t *testing.T) (*Service, *projects.Service, *sql.DB) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	if err := dbmigrate.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return New(db), projects.New(db), db
}

func seedUser(t *testing.T, db *sql.DB, actor auth.Actor) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO auth_users(id,username,password_hash,display_name,role,tenant_id)
	 VALUES($1,$2,'unused',$2,'member',$3)`, actor.ID, actor.Username, tenants.DefaultID); err != nil {
		t.Fatal(err)
	}
}

func createProject(t *testing.T, registry *projects.Service, name, visibility string) *projects.Project {
	t.Helper()
	project, err := registry.Create(context.Background(), adminActor, projects.Input{Name: name, Visibility: visibility})
	if err != nil {
		t.Fatalf("create project %q: %v", name, err)
	}
	return project
}

func addMaintainer(t *testing.T, registry *projects.Service, projectID string, actor auth.Actor) {
	t.Helper()
	if _, err := registry.AddMember(context.Background(), adminActor, projectID, actor.ID, projects.RoleMaintainer); err != nil {
		t.Fatalf("add maintainer %s: %v", actor.Username, err)
	}
}

func ingestScan(t *testing.T, s *Service, projectID, deliveryID string, input Input) (*Scan, bool) {
	t.Helper()
	scan, created, err := s.Ingest(context.Background(), projectID, deliveryID, input)
	if err != nil {
		t.Fatalf("ingest %s: %v", deliveryID, err)
	}
	return scan, created
}

func countScans(t *testing.T, s *Service, projectID string) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM scans WHERE project_id=$1`, projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestIngestScanAndDeliveryIdempotency(t *testing.T) {
	s, registry, db := testService(t)
	ctx := context.Background()
	seedUser(t, db, maintainActor)
	project := createProject(t, registry, "scan-ingest", projects.VisibilityPublic)

	scan, created := ingestScan(t, s, project.ID, "delivery-0001", Input{
		Source: "scanner", Tool: "trivy", Version: "0.55.0", Verdict: VerdictPassed,
		Summary: "no high severity findings", ReportURL: "https://example.com/scans/1",
	})
	if !created || scan.ID == "" || scan.ProjectID != project.ID || scan.ProjectName != project.Name ||
		scan.Source != "scanner" || scan.Tool != "trivy" || scan.Version != "0.55.0" ||
		scan.Verdict != "passed" || scan.Summary != "no high severity findings" ||
		scan.ReportURL != "https://example.com/scans/1" || scan.ReceivedAt == 0 {
		t.Fatalf("ingested scan = %+v created=%v", scan, created)
	}

	// A retried delivery is idempotent: same stored row, no second insert, and a
	// different payload cannot rewrite the evidence.
	retried, createdAgain := ingestScan(t, s, project.ID, "delivery-0001", Input{
		Source: "other-tool", Verdict: VerdictFailed, Summary: "changed",
	})
	if createdAgain || retried.ID != scan.ID || retried.Source != "scanner" || retried.Verdict != VerdictPassed {
		t.Fatalf("retried scan = %+v created=%v", retried, createdAgain)
	}
	if stored := countScans(t, s, project.ID); stored != 1 {
		t.Fatalf("stored rows = %d, want 1", stored)
	}

	// A delivery id is globally unique: reusing it for another project returns
	// the original row instead of duplicating the evidence under a new project.
	other := createProject(t, registry, "scan-ingest-other", projects.VisibilityPublic)
	reused, createdAgain := ingestScan(t, s, other.ID, "delivery-0001", Input{Source: "scanner", Verdict: VerdictWarning})
	if createdAgain || reused.ID != scan.ID || reused.ProjectID != project.ID {
		t.Fatalf("reused delivery scan = %+v created=%v", reused, createdAgain)
	}

	// Unknown project is not found; malformed ids are validation errors.
	if _, _, err := s.Ingest(ctx, "missing-0000000000001", "delivery-0002", Input{Source: "scanner", Verdict: VerdictWarning}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project error = %v", err)
	}
	if _, _, err := s.Ingest(ctx, "not-a-valid-id", "delivery-0003", Input{Source: "scanner", Verdict: VerdictWarning}); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid project id error = %v", err)
	}
	if _, _, err := s.Ingest(ctx, project.ID, "short", Input{Source: "scanner", Verdict: VerdictWarning}); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid delivery error = %v", err)
	}
}

func TestIngestScanValidation(t *testing.T) {
	s, registry, _ := testService(t)
	project := createProject(t, registry, "scan-validation", projects.VisibilityPublic)
	for _, tc := range []struct {
		name     string
		delivery string
		input    Input
	}{
		{"source blank", "delivery-1001", Input{Source: "   ", Verdict: VerdictPassed}},
		{"source too long", "delivery-1002", Input{Source: strings.Repeat("s", 65), Verdict: VerdictPassed}},
		{"source newline", "delivery-1003", Input{Source: "scanner\nline", Verdict: VerdictPassed}},
		{"tool too long", "delivery-1004", Input{Source: "scanner", Tool: strings.Repeat("t", 65), Verdict: VerdictPassed}},
		{"tool newline", "delivery-1005", Input{Source: "scanner", Tool: "tri\nvy", Verdict: VerdictPassed}},
		{"version too long", "delivery-1006", Input{Source: "scanner", Version: strings.Repeat("v", 65), Verdict: VerdictPassed}},
		{"version newline", "delivery-1007", Input{Source: "scanner", Version: "1.0\n1", Verdict: VerdictPassed}},
		{"verdict unknown", "delivery-1008", Input{Source: "scanner", Verdict: "unknown"}},
		{"verdict uppercase", "delivery-1009", Input{Source: "scanner", Verdict: "PASSED"}},
		{"summary too long", "delivery-1010", Input{Source: "scanner", Verdict: VerdictPassed, Summary: strings.Repeat("x", 1001)}},
		{"summary invalid utf8", "delivery-1011", Input{Source: "scanner", Verdict: VerdictPassed, Summary: string([]byte{0xff, 0xfe})}},
		{"report url scheme", "delivery-1012", Input{Source: "scanner", Verdict: VerdictPassed, ReportURL: "ftp://example.com/x"}},
		{"report url userinfo", "delivery-1013", Input{Source: "scanner", Verdict: VerdictPassed, ReportURL: "https://user:pass@example.com/x"}},
		{"report url fragment", "delivery-1014", Input{Source: "scanner", Verdict: VerdictPassed, ReportURL: "https://example.com/x#frag"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := s.Ingest(context.Background(), project.ID, tc.delivery, tc.input); !errors.Is(err, ErrValidation) {
				t.Fatalf("error = %v, want ErrValidation", err)
			}
		})
	}
	if stored := countScans(t, s, project.ID); stored != 0 {
		t.Fatalf("invalid deliveries stored %d rows", stored)
	}
}

func TestIngestScanNormalizesOptionalFields(t *testing.T) {
	s, registry, db := testService(t)
	project := createProject(t, registry, "scan-normalize", projects.VisibilityPublic)

	// Trimming, control-character removal (newlines preserved) and the empty
	// report URL represented as NULL.
	scan, created := ingestScan(t, s, project.ID, "delivery-2001", Input{
		Source:  "  scanner  ",
		Tool:    " trivy ",
		Version: " 0.55.0 ",
		Verdict: VerdictWarning,
		Summary: "line one\r\nline\u0000two\n\n",
	})
	if !created || scan.Source != "scanner" || scan.Tool != "trivy" || scan.Version != "0.55.0" ||
		scan.Summary != "line one\nlinetwo" || scan.ReportURL != "" {
		t.Fatalf("normalized scan = %+v", scan)
	}
	var reportURLIsNull bool
	if err := db.QueryRow(`SELECT report_url IS NULL FROM scans WHERE id=$1`, scan.ID).Scan(&reportURLIsNull); err != nil {
		t.Fatal(err)
	}
	if !reportURLIsNull {
		t.Fatal("empty reportUrl must be stored as NULL")
	}

	// Optional tool/version may be omitted entirely.
	minimal, created := ingestScan(t, s, project.ID, "delivery-2002", Input{Source: "scanner", Verdict: VerdictFailed})
	if !created || minimal.Tool != "" || minimal.Version != "" || minimal.Summary != "" {
		t.Fatalf("minimal scan = %+v", minimal)
	}
}

func TestListAndGetScanVisibilityFilters(t *testing.T) {
	s, registry, db := testService(t)
	ctx := context.Background()
	for _, actor := range []auth.Actor{maintainActor, viewerActor, outsiderActor} {
		seedUser(t, db, actor)
	}
	public := createProject(t, registry, "scan-public", projects.VisibilityPublic)
	private := createProject(t, registry, "scan-private", projects.VisibilityPrivate)
	addMaintainer(t, registry, private.ID, maintainActor)

	// Three public scans with distinct received_at values (the column has second
	// resolution), one failed and one private-project scan.
	passedOne, _ := ingestScan(t, s, public.ID, "delivery-3001", Input{Source: "scanner", Verdict: VerdictPassed, Summary: "first"})
	passedTwo, _ := ingestScan(t, s, public.ID, "delivery-3002", Input{Source: "scanner", Verdict: VerdictPassed, Summary: "second"})
	failed, _ := ingestScan(t, s, public.ID, "delivery-3003", Input{Source: "scanner", Verdict: VerdictFailed, Summary: "third"})
	privateScan, _ := ingestScan(t, s, private.ID, "delivery-3004", Input{Source: "scanner", Verdict: VerdictWarning, Summary: "private"})
	for i, scan := range []*Scan{passedOne, passedTwo, failed, privateScan} {
		if _, err := db.Exec(`UPDATE scans SET received_at=$2 WHERE id=$1`, scan.ID, 1_700_000_000+int64(i)); err != nil {
			t.Fatal(err)
		}
	}

	// Project list: newest first, verdict filter, pagination and total.
	list, total, err := s.ListProject(ctx, viewerActor, public.ID, "", 2, 0)
	if err != nil || total != 3 || len(list) != 2 || list[0].ID != failed.ID || list[1].ID != passedTwo.ID {
		t.Fatalf("first page = %+v total=%d err=%v", list, total, err)
	}
	if list[0].ProjectName != public.Name {
		t.Fatalf("project name = %q", list[0].ProjectName)
	}
	list, total, err = s.ListProject(ctx, viewerActor, public.ID, "", 2, 2)
	if err != nil || total != 3 || len(list) != 1 || list[0].ID != passedOne.ID {
		t.Fatalf("second page = %+v total=%d err=%v", list, total, err)
	}
	list, total, err = s.ListProject(ctx, viewerActor, public.ID, VerdictPassed, 20, 0)
	if err != nil || total != 2 || len(list) != 2 {
		t.Fatalf("verdict filter = %+v total=%d err=%v", list, total, err)
	}
	list, total, err = s.ListProject(ctx, viewerActor, public.ID, VerdictWarning, 20, 0)
	if err != nil || total != 0 || len(list) != 0 {
		t.Fatalf("empty verdict filter = %+v total=%d err=%v", list, total, err)
	}

	// A private project's scans follow project visibility: outsider 404,
	// maintainer allowed.
	if _, _, err = s.ListProject(ctx, outsiderActor, private.ID, "", 20, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider project list error = %v", err)
	}
	if _, _, err = s.ListProject(ctx, maintainActor, private.ID, "", 20, 0); err != nil {
		t.Fatalf("maintainer project list: %v", err)
	}

	// Get follows the same predicate, and ids stay non-enumerable.
	if scan, err := s.Get(ctx, viewerActor, passedOne.ID); err != nil || scan.ID != passedOne.ID {
		t.Fatalf("viewer get = %+v err=%v", scan, err)
	}
	if _, err := s.Get(ctx, outsiderActor, privateScan.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider get error = %v", err)
	}
	if _, err := s.Get(ctx, viewerActor, "missing-0000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown get error = %v", err)
	}
	if _, err := s.Get(ctx, viewerActor, "not-a-valid-id"); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid get id error = %v", err)
	}

	// Global list: admin sees everything; a viewer sees public scans only and an
	// invisible projectId filter yields an empty page (never an error).
	if _, total, err = s.ListVisible(ctx, adminActor, ListFilter{Limit: 20}); err != nil || total != 4 {
		t.Fatalf("admin global total = %d err=%v", total, err)
	}
	if list, total, err = s.ListVisible(ctx, viewerActor, ListFilter{Limit: 20}); err != nil || total != 3 || list[0].ID != failed.ID {
		t.Fatalf("viewer global = %+v total=%d err=%v", list, total, err)
	}
	if _, total, err = s.ListVisible(ctx, viewerActor, ListFilter{ProjectID: private.ID, Limit: 20}); err != nil || total != 0 {
		t.Fatalf("invisible projectId filter total = %d err=%v", total, err)
	}
	if list, total, err = s.ListVisible(ctx, maintainActor, ListFilter{ProjectID: private.ID, Limit: 20}); err != nil || total != 1 || list[0].ID != privateScan.ID {
		t.Fatalf("maintainer private filter = %+v total=%d err=%v", list, total, err)
	}
	if _, total, err = s.ListVisible(ctx, viewerActor, ListFilter{Verdict: VerdictFailed, Limit: 20}); err != nil || total != 1 {
		t.Fatalf("global verdict filter total = %d err=%v", total, err)
	}

	// Filter validation.
	for _, filter := range []ListFilter{
		{Verdict: "unknown", Limit: 20},
		{ProjectID: "not-a-valid-id", Limit: 20},
		{Limit: 0},
		{Limit: 101},
		{Limit: 20, Offset: -1},
	} {
		if _, _, err := s.ListVisible(ctx, adminActor, filter); !errors.Is(err, ErrValidation) {
			t.Fatalf("ListVisible(%+v) error = %v", filter, err)
		}
	}
	if _, _, err := s.ListProject(ctx, adminActor, public.ID, "unknown", 20, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid verdict filter error = %v", err)
	}
}
