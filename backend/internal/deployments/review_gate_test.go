package deployments

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
)

// seedReview inserts one review row directly so deployment tests can pin every
// status without importing the review service. The row is a valid review: the
// deployment gate only reads id, project_id and status.
func seedReview(t *testing.T, db *sql.DB, projectID, status string) string {
	t.Helper()
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	var number int64
	if err = db.QueryRow(`SELECT COALESCE(MAX(number),0)+1 FROM reviews WHERE project_id=$1`, projectID).Scan(&number); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err = db.Exec(`INSERT INTO reviews(id,project_id,number,subject,description,version,external_url,status,requested_by,created_at,updated_at)
	 VALUES($1,$2,$3,'gated release','','','',$4,$5,$6,$6)`, id, projectID, number, status, testActor.ID, now); err != nil {
		t.Fatal(err)
	}
	return id
}

// storedReviewID reads the nullable review_id column directly; it distinguishes
// NULL (ungated, empty string) from a stored review id.
func storedReviewID(t *testing.T, db *sql.DB, deploymentID string) (string, bool) {
	t.Helper()
	var reviewID sql.NullString
	if err := db.QueryRow(`SELECT review_id FROM deployments WHERE id=$1`, deploymentID).Scan(&reviewID); err != nil {
		t.Fatal(err)
	}
	return reviewID.String, reviewID.Valid
}

func reviewGateService(t *testing.T, environments ...string) (*Service, *sql.DB, *projects.Service) {
	t.Helper()
	return taskService(t, Options{RequireReviewEnvironments: environments})
}

func reviewGateTarget(t *testing.T, s *Service, name, endpoint, environment string) Target {
	t.Helper()
	target, err := s.Create(context.Background(), testActor, Input{Name: name, Endpoint: endpoint, Environment: environment})
	if err != nil {
		t.Fatalf("create target %q: %v", name, err)
	}
	return target
}

// The default stays the pre-gate behavior: no review is required, but a review
// that is supplied must still be an approved review of the same project.
func TestReviewGateDisabledByDefault(t *testing.T) {
	s, db, registry := reviewGateService(t)
	ctx := context.Background()
	project := taskProject(t, registry, "review-gate-off")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	target := reviewGateTarget(t, s, "gate-off-target", upstream.URL, EnvironmentProduction)

	ungated, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatalf("ungated create: %v", err)
	}
	if ungated.ReviewID != "" {
		t.Fatalf("ungated deployment reviewId = %q, want empty", ungated.ReviewID)
	}
	if id, ok := storedReviewID(t, db, ungated.ID); ok || id != "" {
		t.Fatalf("ungated deployment stored review_id = %q valid=%v, want NULL", id, ok)
	}

	approved := seedReview(t, db, project.ID, "approved")
	gated, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v2", ReviewID: approved})
	if err != nil {
		t.Fatalf("create with approved review: %v", err)
	}
	if gated.ReviewID != approved {
		t.Fatalf("deployment reviewId = %q, want %q", gated.ReviewID, approved)
	}
	if id, ok := storedReviewID(t, db, gated.ID); !ok || id != approved {
		t.Fatalf("stored review_id = %q valid=%v, want %q", id, ok, approved)
	}
	if audit := deploymentAudit(t, db, gated.ID); len(audit) != 1 || !strings.Contains(audit[0], "review="+approved) {
		t.Fatalf("gated create audit = %+v, want detail with review=%s", audit, approved)
	}

	pending := seedReview(t, db, project.ID, "pending")
	if _, err = s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v3", ReviewID: pending}); !errors.Is(err, ErrValidation) {
		t.Fatalf("create with pending review error = %v, want ErrValidation", err)
	}
	if audit := deploymentAudit(t, db, ungated.ID); len(audit) != 1 || strings.Contains(audit[0], "review=") {
		t.Fatalf("ungated create audit = %+v, want no review context", audit)
	}
}

// TestRequiredReviewGate covers the opt-in gate: a missing or non-approved
// review is rejected before any deployment row is written, while an approved
// same-project review is snapshotted and audited.
func TestRequiredReviewGate(t *testing.T) {
	s, db, registry := reviewGateService(t, EnvironmentProduction)
	ctx := context.Background()
	project := taskProject(t, registry, "review-gate-on")
	other := taskProject(t, registry, "review-gate-other")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	target := reviewGateTarget(t, s, "gate-on-target", upstream.URL, EnvironmentProduction)
	devTarget := reviewGateTarget(t, s, "gate-on-dev", upstream.URL, EnvironmentDevelopment)

	before, total, err := s.ListDeployments(ctx, testActor, project.ID, "", 20, 0)
	if err != nil || len(before) != 0 || total != 0 {
		t.Fatalf("initial deployments = %d %d err=%v", len(before), total, err)
	}

	// Missing review on a gated environment is a validation error and writes
	// nothing (neither a deployment nor its audit row).
	if _, err = s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing review error = %v, want ErrValidation", err)
	}
	if _, total, err = s.ListDeployments(ctx, testActor, project.ID, "", 20, 0); err != nil || total != 0 {
		t.Fatalf("deployments after rejected create = %d err=%v, want 0", total, err)
	}

	// Every non-approved status is rejected with the same validation error.
	for _, status := range []string{"pending", "rejected", "cancelled"} {
		reviewID := seedReview(t, db, project.ID, status)
		if _, err = s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1", ReviewID: reviewID}); !errors.Is(err, ErrValidation) {
			t.Fatalf("review status %s error = %v, want ErrValidation", status, err)
		}
	}
	// Another project's approved review must not gate this project, and an
	// unknown/malformed review id is the same validation error (no oracle).
	crossProject := seedReview(t, db, other.ID, "approved")
	unknown, idErr := newID()
	if idErr != nil {
		t.Fatal(idErr)
	}
	for name, reviewID := range map[string]string{
		"cross-project": crossProject,
		"unknown":       unknown,
		"malformed":     "short",
	} {
		if _, err = s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1", ReviewID: reviewID}); !errors.Is(err, ErrValidation) {
			t.Fatalf("%s review error = %v, want ErrValidation", name, err)
		}
	}

	approved := seedReview(t, db, project.ID, "approved")
	deployment, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1", ReviewID: approved})
	if err != nil {
		t.Fatalf("gated create: %v", err)
	}
	if deployment.Status != DeployQueued || deployment.ReviewID != approved || deployment.ApprovalID != "" {
		t.Fatalf("gated deployment = %+v", deployment)
	}
	if id, ok := storedReviewID(t, db, deployment.ID); !ok || id != approved {
		t.Fatalf("stored review_id = %q valid=%v, want %q", id, ok, approved)
	}
	audit := deploymentAudit(t, db, deployment.ID)
	if len(audit) != 1 || !strings.Contains(audit[0], "review="+approved) || !strings.Contains(audit[0], "approval=none") {
		t.Fatalf("gated create audit = %+v", audit)
	}

	// The gate is per environment: development does not require a review, but a
	// supplied review is still validated wherever it is supplied.
	if _, err = s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: devTarget.ID, Version: "v2"}); err != nil {
		t.Fatalf("ungated development create: %v", err)
	}
	pending := seedReview(t, db, project.ID, "pending")
	if _, err = s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: devTarget.ID, Version: "v3", ReviewID: pending}); !errors.Is(err, ErrValidation) {
		t.Fatalf("pending review on ungated environment error = %v, want ErrValidation", err)
	}
}

// TestReviewGateComposesWithApproval pins the order of the two gates: the
// review gate is validated first, then the existing approval state machine runs
// unchanged, so an approved review still yields awaiting_approval.
func TestReviewGateComposesWithApproval(t *testing.T) {
	s, db, registry := taskService(t, Options{
		RequireReviewEnvironments: []string{EnvironmentProduction},
		ApprovalEnvironments:      []string{EnvironmentProduction},
	})
	ctx := context.Background()
	project := taskProject(t, registry, "review-gate-approval")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	target := reviewGateTarget(t, s, "gate-approval-target", upstream.URL, EnvironmentProduction)

	if _, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing review with approval enabled error = %v, want ErrValidation", err)
	}
	approved := seedReview(t, db, project.ID, "approved")
	deployment, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1", ReviewID: approved})
	if err != nil {
		t.Fatalf("gated deployment: %v", err)
	}
	if deployment.Status != DeployAwaitingApproval || deployment.ApprovalID == "" || deployment.ReviewID != approved {
		t.Fatalf("gated deployment with approval = %+v", deployment)
	}
	// Both audit rows exist: deployment.create carries the review context and
	// approval.request is unchanged.
	audit := deploymentAudit(t, db, deployment.ID)
	if len(audit) != 1 || !strings.Contains(audit[0], "review="+approved) || !strings.Contains(audit[0], "approval=required") {
		t.Fatalf("deployment audit = %+v", audit)
	}
	var approvalAudits int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_audit WHERE object_type='approval' AND object_id=$1`, deployment.ApprovalID).Scan(&approvalAudits); err != nil {
		t.Fatal(err)
	}
	if approvalAudits != 1 {
		t.Fatalf("approval audit rows = %d, want 1", approvalAudits)
	}
}

// TestRollbackReviewGate applies the same gate to rollbacks: the new deployment
// row snapshots the approved review and the audit detail carries it.
func TestRollbackReviewGate(t *testing.T) {
	s, db, registry := reviewGateService(t, EnvironmentProduction)
	ctx := context.Background()
	project := taskProject(t, registry, "review-gate-rollback")
	other := taskProject(t, registry, "review-gate-rollback-other")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	target := reviewGateTarget(t, s, "gate-rollback-target", upstream.URL, EnvironmentProduction)

	base := time.Now().Unix()
	first := seedDeployment(t, db, project.ID, target.ID, 1, DeploySucceeded, "v1", base-30)
	source := seedDeployment(t, db, project.ID, target.ID, 2, DeployFailed, "v2", base-20)

	if _, err := s.RollbackDeployment(ctx, testActor, source, ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("rollback without review error = %v, want ErrValidation", err)
	}
	pending := seedReview(t, db, project.ID, "pending")
	if _, err := s.RollbackDeployment(ctx, testActor, source, pending); !errors.Is(err, ErrValidation) {
		t.Fatalf("rollback with pending review error = %v, want ErrValidation", err)
	}
	crossProject := seedReview(t, db, other.ID, "approved")
	if _, err := s.RollbackDeployment(ctx, testActor, source, crossProject); !errors.Is(err, ErrValidation) {
		t.Fatalf("rollback with cross-project review error = %v, want ErrValidation", err)
	}

	approved := seedReview(t, db, project.ID, "approved")
	rollback, err := s.RollbackDeployment(ctx, testActor, source, approved)
	if err != nil {
		t.Fatalf("rollback with approved review: %v", err)
	}
	if rollback.Version != "v1" || rollback.RollbackOf != source || rollback.ReviewID != approved || rollback.Status != DeployQueued {
		t.Fatalf("rollback deployment = %+v", rollback)
	}
	// The seeded source rows are still intact; the rollback is a new row.
	if _, err = s.GetDeployment(ctx, testActor, first); err != nil {
		t.Fatalf("get earlier deployment: %v", err)
	}
	if id, ok := storedReviewID(t, db, rollback.ID); !ok || id != approved {
		t.Fatalf("stored rollback review_id = %q valid=%v, want %q", id, ok, approved)
	}
	audit := deploymentAudit(t, db, rollback.ID)
	if len(audit) != 1 || !strings.Contains(audit[0], "rollback_of="+source) || !strings.Contains(audit[0], "review="+approved) {
		t.Fatalf("rollback audit = %+v", audit)
	}
}
