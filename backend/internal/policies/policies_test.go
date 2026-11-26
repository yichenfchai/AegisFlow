package policies

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/dbmigrate"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// Actors covering the global roles plus one project outsider; the maintainer is
// added to each test project through the project registry.
var (
	adminActor    = auth.Actor{ID: "admin-0000000000000001", Username: "root", Role: "admin", RequestID: "req-policies"}
	maintActor    = auth.Actor{ID: "maint-0000000000000001", Username: "mira", Role: "member", RequestID: "req-policies"}
	viewerActor   = auth.Actor{ID: "viewer-000000000000001", Username: "vic", Role: "viewer", RequestID: "req-policies"}
	outsiderActor = auth.Actor{ID: "outsider-0000000000001", Username: "otto", Role: "member", RequestID: "req-policies"}
)

func testService(t *testing.T) (*Service, *projects.Service, *sql.DB) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	if err := dbmigrate.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	registry := projects.New(db)
	return New(db, registry), registry, db
}

func seedUser(t *testing.T, db *sql.DB, actor auth.Actor) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO auth_users(id,username,password_hash,display_name,role)
	 VALUES($1,$2,'unused',$2,'member')`, actor.ID, actor.Username); err != nil {
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

// seedScan inserts one scan row directly so the gate tests pin the
// received_at/id ordering without importing the scan service.
func seedScan(t *testing.T, db *sql.DB, projectID, verdict string, receivedAt int64) string {
	t.Helper()
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO scans(id,project_id,delivery_id,source,tool,version,verdict,summary,received_at)
	 VALUES($1,$2,$3,'test-source','trivy','0.55.0',$4,'',$5)`, id, projectID, "delivery-"+id, verdict, receivedAt); err != nil {
		t.Fatal(err)
	}
	return id
}

// deploymentGate reads the gate inputs in a transaction, exactly like the
// deployment service does, and rolls the transaction back afterwards.
func deploymentGate(t *testing.T, s *Service, projectID string) (bool, string, string) {
	t.Helper()
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	requireReview, required, latest, err := s.DeploymentGate(context.Background(), tx, projectID)
	if err != nil {
		t.Fatalf("deployment gate: %v", err)
	}
	return requireReview, required, latest
}

// TestPolicyDefaultsAndVisibility pins the no-row defaults and the read
// visibility rule: an invisible private project is not found, never an empty
// default policy.
func TestPolicyDefaultsAndVisibility(t *testing.T) {
	s, registry, db := testService(t)
	ctx := context.Background()
	for _, actor := range []auth.Actor{adminActor, maintActor, viewerActor, outsiderActor} {
		seedUser(t, db, actor)
	}
	public := createProject(t, registry, "policy-defaults", projects.VisibilityPublic)
	private := createProject(t, registry, "policy-defaults-private", projects.VisibilityPrivate)
	addMaintainer(t, registry, private.ID, maintActor)

	policy, err := s.Get(ctx, viewerActor, public.ID)
	if err != nil {
		t.Fatalf("get default policy: %v", err)
	}
	want := Policy{ProjectID: public.ID, RequireReview: false, RequiredScanVerdict: RequiredScanVerdictNone}
	if policy != want {
		t.Fatalf("default policy = %+v, want %+v", policy, want)
	}
	for name, actor := range map[string]auth.Actor{"viewer": viewerActor, "member-outsider": outsiderActor} {
		if _, err = s.Get(ctx, actor, private.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s read of private project error = %v, want ErrNotFound", name, err)
		}
	}
	// The creator (admin) and a maintainer can still read the private project.
	for name, actor := range map[string]auth.Actor{"admin": adminActor, "maintainer": maintActor} {
		if _, err = s.Get(ctx, actor, private.ID); err != nil {
			t.Fatalf("%s read of visible project: %v", name, err)
		}
	}
	if _, err = s.Get(ctx, adminActor, "short"); !errors.Is(err, ErrValidation) {
		t.Fatalf("malformed project id error = %v, want ErrValidation", err)
	}
	if _, err = s.Get(ctx, adminActor, "missing-000000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project error = %v, want ErrNotFound", err)
	}
}

// TestUpsertAuthorizationAuditAndDefaults covers the write matrix (admin or
// maintainer only, invisible project 404), the strict enum, the stored row, the
// updater identity and the transactional policy.update audit row.
func TestUpsertAuthorizationAuditAndDefaults(t *testing.T) {
	s, registry, db := testService(t)
	ctx := context.Background()
	for _, actor := range []auth.Actor{adminActor, maintActor, viewerActor, outsiderActor} {
		seedUser(t, db, actor)
	}
	project := createProject(t, registry, "policy-writes", projects.VisibilityPublic)
	private := createProject(t, registry, "policy-writes-private", projects.VisibilityPrivate)
	addMaintainer(t, registry, project.ID, maintActor)

	// Visibility first: an invisible project is not found even for a valid body.
	if _, err := s.Upsert(ctx, outsiderActor, private.ID, Input{RequiredScanVerdict: RequiredScanVerdictNone}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider upsert of private project error = %v, want ErrNotFound", err)
	}
	// A viewer never writes, and a plain member of a public project is not a
	// maintainer: gate changes reuse the reviewer overlay, not the write overlay.
	for name, actor := range map[string]auth.Actor{"viewer": viewerActor, "member": outsiderActor} {
		if _, err := s.Upsert(ctx, actor, project.ID, Input{RequiredScanVerdict: RequiredScanVerdictNone}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s upsert error = %v, want ErrForbidden", name, err)
		}
	}
	// The enum is validated before anything is written.
	for _, verdict := range []string{"", "PASSED", "failed"} {
		if _, err := s.Upsert(ctx, adminActor, project.ID, Input{RequiredScanVerdict: verdict}); !errors.Is(err, ErrValidation) {
			t.Fatalf("verdict %q error = %v, want ErrValidation", verdict, err)
		}
	}
	if _, err := s.Upsert(ctx, adminActor, "short", Input{RequiredScanVerdict: RequiredScanVerdictNone}); !errors.Is(err, ErrValidation) {
		t.Fatalf("malformed project id error = %v, want ErrValidation", err)
	}
	if countPolicyAudits(t, db, project.ID) != 0 {
		t.Fatal("rejected writes must not be audited as policy.update")
	}

	// A maintainer stores the policy; the audit row commits with it.
	updated, err := s.Upsert(ctx, maintActor, project.ID, Input{RequireReview: true, RequiredScanVerdict: RequiredScanVerdictPassed})
	if err != nil {
		t.Fatalf("maintainer upsert: %v", err)
	}
	if updated.ProjectID != project.ID || !updated.RequireReview || updated.RequiredScanVerdict != RequiredScanVerdictPassed ||
		updated.UpdatedBy != maintActor.ID || updated.UpdatedByName != maintActor.Username || updated.UpdatedAt == 0 {
		t.Fatalf("stored policy = %+v", updated)
	}
	// Every visible role reads the same row, including the resolved username.
	for name, actor := range map[string]auth.Actor{"viewer": viewerActor, "member": outsiderActor, "admin": adminActor} {
		policy, err := s.Get(ctx, actor, project.ID)
		if err != nil {
			t.Fatalf("%s get: %v", name, err)
		}
		if policy != updated {
			t.Fatalf("%s policy = %+v, want %+v", name, policy, updated)
		}
	}
	detail := policyAuditDetail(t, db, project.ID)
	wantDetail := "projectId=" + project.ID + " requireReview=true requiredScanVerdict=passed"
	if detail != wantDetail {
		t.Fatalf("audit detail = %q, want %q", detail, wantDetail)
	}
	if strings.Contains(detail, "token") || strings.Contains(detail, "password") || strings.Contains(detail, "cookie") {
		t.Fatalf("audit detail leaks sensitive-looking content: %q", detail)
	}

	// The upsert path replaces the row (not a second row) and an admin may also
	// write it; the default values are stored explicitly, not deleted.
	if _, err = s.Upsert(ctx, adminActor, project.ID, Input{RequireReview: false, RequiredScanVerdict: RequiredScanVerdictNone}); err != nil {
		t.Fatalf("admin upsert: %v", err)
	}
	policy, err := s.Get(ctx, adminActor, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if policy.RequireReview || policy.RequiredScanVerdict != RequiredScanVerdictNone || policy.UpdatedBy != adminActor.ID {
		t.Fatalf("replaced policy = %+v", policy)
	}
	var rows int
	if err = db.QueryRow(`SELECT COUNT(*) FROM project_policies WHERE project_id=$1`, project.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("policy rows = %d, want 1", rows)
	}
	if countPolicyAudits(t, db, project.ID) != 2 {
		t.Fatalf("policy.update audit rows = %d, want 2", countPolicyAudits(t, db, project.ID))
	}
}

func countPolicyAudits(t *testing.T, db *sql.DB, projectID string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_audit WHERE action='policy.update' AND object_type='policy' AND object_id=$1`, projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func policyAuditDetail(t *testing.T, db *sql.DB, projectID string) string {
	t.Helper()
	var detail string
	if err := db.QueryRow(`SELECT detail FROM auth_audit WHERE action='policy.update' AND object_type='policy' AND object_id=$1 ORDER BY created_at,id DESC LIMIT 1`, projectID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	return detail
}

// TestDeploymentGateInputs pins the deployment-facing projection: defaults for a
// project without a policy or scans, the latest scan by received_at then id, and
// the stored gate values.
func TestDeploymentGateInputs(t *testing.T) {
	s, registry, db := testService(t)
	ctx := context.Background()
	for _, actor := range []auth.Actor{adminActor, maintActor} {
		seedUser(t, db, actor)
	}
	project := createProject(t, registry, "policy-gate", projects.VisibilityPublic)
	other := createProject(t, registry, "policy-gate-other", projects.VisibilityPublic)
	addMaintainer(t, registry, project.ID, maintActor)

	requireReview, required, latest := deploymentGate(t, s, project.ID)
	if requireReview || required != RequiredScanVerdictNone || latest != "" {
		t.Fatalf("default gate = %v %q %q", requireReview, required, latest)
	}

	// Latest is the scan a reader sees first: received_at DESC, id DESC.
	now := time.Now().Unix()
	seedScan(t, db, other.ID, "passed", now)
	seedScan(t, db, project.ID, "passed", now-20)
	seedScan(t, db, project.ID, "failed", now-10)
	if _, _, latest = deploymentGate(t, s, project.ID); latest != "failed" {
		t.Fatalf("latest scan = %q, want failed", latest)
	}
	seedScan(t, db, project.ID, "passed", now)
	if _, _, latest = deploymentGate(t, s, project.ID); latest != "passed" {
		t.Fatalf("latest scan after newer pass = %q, want passed", latest)
	}

	// Same received_at: the id tie-break decides, consistent with the list query.
	tieTime := now + 60
	first := seedScan(t, db, project.ID, "failed", tieTime)
	second := seedScan(t, db, project.ID, "warning", tieTime)
	newest := first
	if second > first {
		newest = second
	}
	verdict := "failed"
	if newest == second {
		verdict = "warning"
	}
	if _, _, latest = deploymentGate(t, s, project.ID); latest != verdict {
		t.Fatalf("same-timestamp latest scan = %q, want %q", latest, verdict)
	}

	// The policy row is returned as stored; another project stays untouched.
	if _, err := s.Upsert(ctx, maintActor, project.ID, Input{RequireReview: true, RequiredScanVerdict: RequiredScanVerdictPassed}); err != nil {
		t.Fatal(err)
	}
	requireReview, required, latest = deploymentGate(t, s, project.ID)
	if !requireReview || required != RequiredScanVerdictPassed || latest != verdict {
		t.Fatalf("stored gate = %v %q %q", requireReview, required, latest)
	}
	if requireReview, required, latest = deploymentGate(t, s, other.ID); requireReview || required != RequiredScanVerdictNone || latest != "passed" {
		t.Fatalf("other project gate = %v %q %q", requireReview, required, latest)
	}
}
