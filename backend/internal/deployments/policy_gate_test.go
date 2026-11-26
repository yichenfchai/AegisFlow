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

// fakePolicyGate pins the policy projection without a database, so these tests
// cover the deployment-side composition exactly: what the gate requires and what
// the deployment service does with it.
type fakePolicyGate struct {
	requireReview       bool
	requiredScanVerdict string
	latestScanVerdict   string
	err                 error
	// projectIDs records the project argument of every call so a test can prove
	// the gate is read for the deployment's own project.
	projectIDs []string
}

func (f *fakePolicyGate) DeploymentGate(_ context.Context, _ *sql.Tx, projectID string) (bool, string, string, error) {
	f.projectIDs = append(f.projectIDs, projectID)
	if f.err != nil {
		return false, "", "", f.err
	}
	required := f.requiredScanVerdict
	if required == "" {
		required = "none"
	}
	return f.requireReview, required, f.latestScanVerdict, nil
}

// policyGateService builds a service with an explicit gate and no environment
// review gate, so every requirement in the test comes from the project policy.
func policyGateService(t *testing.T, gate PolicyGate) (*Service, *sql.DB, *projects.Service) {
	t.Helper()
	return taskService(t, Options{PolicyGate: gate})
}

func policyGateTarget(t *testing.T, s *Service, name, environment string, upstream *httptest.Server) Target {
	t.Helper()
	target, err := s.Create(context.Background(), testActor, Input{Name: name, Endpoint: upstream.URL, Environment: environment})
	if err != nil {
		t.Fatalf("create target %q: %v", name, err)
	}
	return target
}

// TestProjectPolicyRequireReviewGate covers the project-level review gate on a
// development target: the environment gate is off, so only the policy can
// require the approved review, and the same rule applies to a rollback.
func TestProjectPolicyRequireReviewGate(t *testing.T) {
	gate := &fakePolicyGate{requireReview: true}
	s, db, registry := policyGateService(t, gate)
	ctx := context.Background()
	project := taskProject(t, registry, "policy-require-review")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	target := policyGateTarget(t, s, "policy-review-dev", EnvironmentDevelopment, upstream)

	// Seed an earlier succeeded version and a terminal source row for the
	// rollback path before the API creates any deployment number.
	base := time.Now().Unix()
	seedDeployment(t, db, project.ID, target.ID, 1, DeploySucceeded, "v0", base-30)
	source := seedDeployment(t, db, project.ID, target.ID, 2, DeployFailed, "v2", base-20)

	if _, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"}); !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "approved review") {
		t.Fatalf("missing review under policy error = %v, want validation mentioning approved review", err)
	}
	if len(gate.projectIDs) == 0 || gate.projectIDs[len(gate.projectIDs)-1] != project.ID {
		t.Fatalf("gate project ids = %v, want %q", gate.projectIDs, project.ID)
	}
	_, seededTotal, err := s.ListDeployments(ctx, testActor, project.ID, "", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, total, err := s.ListDeployments(ctx, testActor, project.ID, "", 20, 0); err != nil || total != seededTotal {
		t.Fatalf("deployments after rejected create = %d err=%v, want %d", total, err, seededTotal)
	}

	pending := seedReview(t, db, project.ID, "pending")
	if _, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1", ReviewID: pending}); !errors.Is(err, ErrValidation) {
		t.Fatalf("pending review under policy error = %v, want ErrValidation", err)
	}
	approved := seedReview(t, db, project.ID, "approved")
	deployment, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1", ReviewID: approved})
	if err != nil {
		t.Fatalf("gated create under policy: %v", err)
	}
	if deployment.ReviewID != approved || deployment.Status != DeployQueued {
		t.Fatalf("gated deployment = %+v", deployment)
	}
	audit := deploymentAudit(t, db, deployment.ID)
	if len(audit) != 1 || !strings.Contains(audit[0], "review="+approved) || !strings.Contains(audit[0], "policy="+project.ID) {
		t.Fatalf("gated create audit = %+v, want review and policy context", audit)
	}

	// A rollback is a second deploy attempt: the policy review gate applies too.
	if _, err := s.RollbackDeployment(ctx, testActor, source, ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("rollback without review under policy error = %v, want ErrValidation", err)
	}
	rollback, err := s.RollbackDeployment(ctx, testActor, source, approved)
	if err != nil {
		t.Fatalf("gated rollback under policy: %v", err)
	}
	if rollback.ReviewID != approved || rollback.RollbackOf != source {
		t.Fatalf("rollback = %+v", rollback)
	}
	if audit := deploymentAudit(t, db, rollback.ID); len(audit) != 1 ||
		!strings.Contains(audit[0], "policy="+project.ID) || !strings.Contains(audit[0], "review="+approved) {
		t.Fatalf("rollback audit = %+v", audit)
	}
}

// TestProjectPolicyScanGate covers requiredScanVerdict=passed: no latest scan and
// a non-passed latest scan are rejected before any row is written, while a passed
// latest scan lets the deployment through. A failed scan without the requirement
// stays ungated.
func TestProjectPolicyScanGate(t *testing.T) {
	gate := &fakePolicyGate{requiredScanVerdict: "passed"}
	s, _, registry := policyGateService(t, gate)
	ctx := context.Background()
	project := taskProject(t, registry, "policy-scan-gate")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	target := policyGateTarget(t, s, "policy-scan-dev", EnvironmentDevelopment, upstream)

	for name, latest := range map[string]string{"no scan": "", "failed scan": "failed", "warning scan": "warning"} {
		gate.latestScanVerdict = latest
		_, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
		if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "scan gate") {
			t.Fatalf("%s error = %v, want validation mentioning the scan gate", name, err)
		}
	}
	if _, total, err := s.ListDeployments(ctx, testActor, project.ID, "", 20, 0); err != nil || total != 0 {
		t.Fatalf("deployments after rejected creates = %d err=%v, want 0", total, err)
	}

	gate.latestScanVerdict = "passed"
	deployment, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatalf("deployment with passed scan: %v", err)
	}
	if audit := deploymentAudit(t, db(t, s), deployment.ID); len(audit) != 1 || !strings.Contains(audit[0], "policy="+project.ID) {
		t.Fatalf("scan-gated create audit = %+v, want policy context", audit)
	}

	// The default policy (required verdict none) never rejects on scan state.
	gate.requireReview = false
	gate.requiredScanVerdict = "none"
	gate.latestScanVerdict = "failed"
	if _, err = s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v2"}); err != nil {
		t.Fatalf("ungated deployment with failed latest scan: %v", err)
	}
}

// db exposes the service's pool to the audit helper without changing the
// production API.
func db(t *testing.T, s *Service) *sql.DB {
	t.Helper()
	return s.db
}

// TestPolicyGateOrderAndComposition pins the documented order: the review gate is
// evaluated before the scan gate, and an approved policy review still composes
// with the existing approval state machine.
func TestPolicyGateOrderAndComposition(t *testing.T) {
	gate := &fakePolicyGate{requireReview: true, requiredScanVerdict: "passed"}
	s, db, registry := taskService(t, Options{
		RequireReviewEnvironments: []string{EnvironmentProduction},
		ApprovalEnvironments:      []string{EnvironmentProduction},
		PolicyGate:                gate,
	})
	ctx := context.Background()
	project := taskProject(t, registry, "policy-order")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	target := policyGateTarget(t, s, "policy-order-prod", EnvironmentProduction, upstream)

	// Both gates would fail; the review gate error wins because it runs first.
	_, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "approved review") || strings.Contains(err.Error(), "scan gate") {
		t.Fatalf("gate order error = %v, want the review gate error first", err)
	}

	// Once the review is approved, the scan gate is the next blocker.
	approved := seedReview(t, db, project.ID, "approved")
	if _, err = s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1", ReviewID: approved}); !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "scan gate") {
		t.Fatalf("scan gate after review error = %v, want the scan gate error", err)
	}
	gate.latestScanVerdict = "passed"
	deployment, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1", ReviewID: approved})
	if err != nil {
		t.Fatalf("both gates satisfied: %v", err)
	}
	// The environment approval gate still applies on top of both policy gates.
	if deployment.Status != DeployAwaitingApproval || deployment.ApprovalID == "" {
		t.Fatalf("deployment = %+v, want awaiting_approval with an approval id", deployment)
	}
	audit := deploymentAudit(t, db, deployment.ID)
	if len(audit) != 1 || !strings.Contains(audit[0], "approval=required") || !strings.Contains(audit[0], "policy="+project.ID) || !strings.Contains(audit[0], "review="+approved) {
		t.Fatalf("composed audit = %+v", audit)
	}
}

// TestPolicyGateDisabledAndErrors keeps the compatibility promises: a nil gate
// changes nothing, a gate that reports the default policy adds no audit context,
// and a storage error from the gate fails the deployment without writing rows.
func TestPolicyGateDisabledAndErrors(t *testing.T) {
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	// No gate injected = the pre-policy behavior.
	plain, _, registry := taskService(t, Options{})
	plainProject := taskProject(t, registry, "policy-plain")
	plainTarget := policyGateTarget(t, plain, "policy-plain-target", EnvironmentDevelopment, upstream)
	deployment, err := plain.CreateDeployment(ctx, testActor, plainProject.ID, DeployInput{TargetID: plainTarget.ID, Version: "v1"})
	if err != nil {
		t.Fatalf("deployment without a policy gate: %v", err)
	}
	if audit := deploymentAudit(t, db(t, plain), deployment.ID); len(audit) != 1 || strings.Contains(audit[0], "policy=") {
		t.Fatalf("default audit = %+v, want no policy context", audit)
	}

	// A gate configured with the default policy behaves identically.
	defGate := &fakePolicyGate{}
	gated, db, gatedRegistry := policyGateService(t, defGate)
	gatedProject := taskProject(t, gatedRegistry, "policy-default-gate")
	gatedTarget := policyGateTarget(t, gated, "policy-default-target", EnvironmentDevelopment, upstream)
	deployment, err = gated.CreateDeployment(ctx, testActor, gatedProject.ID, DeployInput{TargetID: gatedTarget.ID, Version: "v1"})
	if err != nil {
		t.Fatalf("deployment under the default policy: %v", err)
	}
	if audit := deploymentAudit(t, db, deployment.ID); len(audit) != 1 || strings.Contains(audit[0], "policy=") {
		t.Fatalf("default policy audit = %+v, want no policy context", audit)
	}
	if defGate.projectIDs[0] != gatedProject.ID {
		t.Fatalf("gate read project = %q, want %q", defGate.projectIDs[0], gatedProject.ID)
	}

	// A gate storage failure is not a validation error and writes nothing.
	failing := &fakePolicyGate{err: errors.New("policy storage unavailable")}
	broken, _, brokenRegistry := policyGateService(t, failing)
	brokenProject := taskProject(t, brokenRegistry, "policy-broken")
	brokenTarget := policyGateTarget(t, broken, "policy-broken-target", EnvironmentDevelopment, upstream)
	if _, err = broken.CreateDeployment(ctx, testActor, brokenProject.ID, DeployInput{TargetID: brokenTarget.ID, Version: "v1"}); err == nil || errors.Is(err, ErrValidation) {
		t.Fatalf("gate storage failure error = %v, want an internal error", err)
	}
	if _, total, err := broken.ListDeployments(ctx, testActor, brokenProject.ID, "", 20, 0); err != nil || total != 0 {
		t.Fatalf("deployments after gate failure = %d err=%v, want 0", total, err)
	}
}
