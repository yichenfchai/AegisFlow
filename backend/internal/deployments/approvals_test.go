package deployments

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
)

// approvalService enables approval for production only, with a one-hour TTL.
func approvalService(t *testing.T) (*Service, *sql.DB, *projects.Service) {
	t.Helper()
	return taskService(t, Options{ApprovalEnvironments: []string{EnvironmentProduction}, ApprovalTTL: time.Hour})
}

func approvalTarget(t *testing.T, s *Service, name, endpoint, environment string) Target {
	t.Helper()
	target, err := s.Create(context.Background(), testActor, Input{Name: name, Endpoint: endpoint, Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func approvalForDeployment(t *testing.T, s *Service, deploymentID string) Approval {
	t.Helper()
	approval, err := scanApproval(s.db.QueryRow(`SELECT `+approvalColumns+` FROM approvals WHERE deployment_id=$1`, deploymentID))
	if err != nil {
		t.Fatal(err)
	}
	return approval
}

func approvalCountForDeployment(t *testing.T, s *Service, deploymentID string) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM approvals WHERE deployment_id=$1`, deploymentID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

type approvalAuditRow struct {
	Action, ActorID, ActorUsername, RequestID, Detail string
}

func approvalAuditRows(t *testing.T, db *sql.DB, approvalID string) []approvalAuditRow {
	t.Helper()
	rows, err := db.Query(`SELECT action,actor_id,actor_username,request_id,detail FROM auth_audit
	 WHERE object_type='approval' AND object_id=$1 ORDER BY created_at,id`, approvalID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []approvalAuditRow
	for rows.Next() {
		var row approvalAuditRow
		if err := rows.Scan(&row.Action, &row.ActorID, &row.ActorUsername, &row.RequestID, &row.Detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func approvalAction(rows []approvalAuditRow, action string) (approvalAuditRow, int) {
	var found approvalAuditRow
	count := 0
	for _, row := range rows {
		if row.Action == action {
			found = row
			count++
		}
	}
	return found, count
}

func TestCreateDeploymentRequiresApproval(t *testing.T) {
	s, db, registry := approvalService(t)
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	project := taskProject(t, registry, "approval-create")
	production := approvalTarget(t, s, "prod-target", upstream.URL, EnvironmentProduction)

	deployment, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: production.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Status != DeployAwaitingApproval || deployment.ApprovalID == "" || deployment.DeploymentNumber != 1 {
		t.Fatalf("deployment = %+v", deployment)
	}
	approval := approvalForDeployment(t, s, deployment.ID)
	if approval.ID != deployment.ApprovalID || approval.Status != ApprovalPending || approval.DeploymentID != deployment.ID ||
		approval.ProjectID != project.ID || approval.RequestedBy != testActor.ID || approval.CreatedAt == 0 ||
		approval.ExpiresAt-approval.CreatedAt != 3600 || approval.DecidedAt != 0 || approval.DecidedBy != "" {
		t.Fatalf("approval = %+v", approval)
	}
	rows := deploymentAudit(t, db, deployment.ID)
	if len(rows) != 1 || rows[0] != ActionDeployCreate+"|target=prod-target version=v1 approval=required" {
		t.Fatalf("deployment audit = %v", rows)
	}
	audit := approvalAuditRows(t, db, approval.ID)
	if len(audit) != 1 || audit[0].Action != ActionApprovalRequest || audit[0].Detail != "deployment="+deployment.ID || audit[0].ActorID != testActor.ID {
		t.Fatalf("approval audit = %+v", audit)
	}

	// Only the configured environments require approval.
	staging := approvalTarget(t, s, "staging-target", upstream.URL, EnvironmentStaging)
	direct, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: staging.ID, Version: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if direct.Status != DeployQueued || direct.ApprovalID != "" || approvalCountForDeployment(t, s, direct.ID) != 0 {
		t.Fatalf("staging deployment = %+v", direct)
	}
	if rows = deploymentAudit(t, db, direct.ID); len(rows) != 1 || !strings.HasSuffix(rows[0], "approval=none") {
		t.Fatalf("staging audit = %v", rows)
	}

	// An explicitly empty environment list never creates approvals.
	s2, _, registry2 := taskService(t, Options{ApprovalEnvironments: []string{}})
	project2 := taskProject(t, registry2, "approval-disabled")
	prod2 := approvalTarget(t, s2, "prod-target-2", upstream.URL, EnvironmentProduction)
	disabled, err := s2.CreateDeployment(ctx, testActor, project2.ID, DeployInput{TargetID: prod2.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Status != DeployQueued || disabled.ApprovalID != "" {
		t.Fatalf("approval-disabled deployment = %+v", disabled)
	}

	// Rollback goes through approval too. The project already has deployments
	// 1 and 2, so seed from 3 to keep the per-project number unique.
	base := time.Now().Unix()
	seedDeployment(t, db, project.ID, production.ID, 3, DeploySucceeded, "v3", base-30)
	seedDeployment(t, db, project.ID, production.ID, 4, DeploySucceeded, "v4", base-20)
	source := seedDeployment(t, db, project.ID, production.ID, 5, DeployFailed, "v5", base-10)
	rollback, err := s.RollbackDeployment(ctx, testActor, source)
	if err != nil {
		t.Fatal(err)
	}
	if rollback.Status != DeployAwaitingApproval || rollback.ApprovalID == "" || rollback.Version != "v4" || rollback.RollbackOf != source {
		t.Fatalf("rollback = %+v", rollback)
	}
	rollbackApproval := approvalForDeployment(t, s, rollback.ID)
	if rollbackApproval.Status != ApprovalPending {
		t.Fatalf("rollback approval = %+v", rollbackApproval)
	}
	if rows = deploymentAudit(t, db, rollback.ID); len(rows) != 1 || rows[0] != ActionDeployRollback+"|rollback_of="+source+" version=v4" {
		t.Fatalf("rollback audit = %v", rows)
	}
	if audit = approvalAuditRows(t, db, rollbackApproval.ID); len(audit) != 1 || audit[0].Action != ActionApprovalRequest {
		t.Fatalf("rollback approval audit = %+v", audit)
	}
}

func TestNewDefaultsApprovalToProduction(t *testing.T) {
	s, db := testServiceWith(t, Options{})
	registry := projects.New(db)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	project := taskProject(t, registry, "approval-defaults")
	target := approvalTarget(t, s, "default-prod", upstream.URL, EnvironmentProduction)
	deployment, err := s.CreateDeployment(context.Background(), testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Status != DeployAwaitingApproval {
		t.Fatalf("default service did not require approval: %+v", deployment)
	}
	approval := approvalForDeployment(t, s, deployment.ID)
	if approval.ExpiresAt-approval.CreatedAt != int64(defaultApprovalTTL/time.Second) {
		t.Fatalf("default TTL = %d seconds", approval.ExpiresAt-approval.CreatedAt)
	}
}

func TestApproveMovesDeploymentToQueuedAndIsIdempotent(t *testing.T) {
	s, db, registry := approvalService(t)
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	project := taskProject(t, registry, "approval-approve")
	target := approvalTarget(t, s, "approve-target", upstream.URL, EnvironmentProduction)
	deployment, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	startDeployWorker(t, s)

	approved, err := s.Approve(ctx, testActor, deployment.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != ApprovalApproved || approved.DecidedBy != testActor.ID || approved.DecidedAt == 0 {
		t.Fatalf("approved = %+v", approved)
	}
	queued, err := s.GetDeployment(ctx, testActor, deployment.ID)
	if err != nil || queued.Status != DeployQueued || queued.ApprovalID != deployment.ApprovalID {
		t.Fatalf("queued deployment = %+v err=%v", queued, err)
	}
	// The worker picks the now-queued row up.
	waitDeploymentStatus(t, s, deployment.ID, DeploySucceeded)

	again, err := s.Approve(ctx, testActor, deployment.ApprovalID)
	if err != nil || again.ID != approved.ID || again.Status != ApprovalApproved {
		t.Fatalf("repeat approve = %+v err=%v", again, err)
	}
	audit := approvalAuditRows(t, db, deployment.ApprovalID)
	approveRow, approveCount := approvalAction(audit, ActionApprovalApprove)
	if approveCount != 1 || approveRow.Detail != "deployment="+deployment.ID || approveRow.ActorID != testActor.ID {
		t.Fatalf("approve audit = %+v", audit)
	}

	if _, err = s.Approve(ctx, testActor, "missing-0000000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown approve error = %v", err)
	}
	if _, err = s.Approve(ctx, testActor, "short"); !errors.Is(err, ErrValidation) {
		t.Fatalf("malformed approve error = %v", err)
	}
}

func TestRejectMovesDeploymentAndIsIdempotent(t *testing.T) {
	s, db, registry := approvalService(t)
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	project := taskProject(t, registry, "approval-reject")
	target := approvalTarget(t, s, "reject-target", upstream.URL, EnvironmentProduction)
	deployment, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}

	rejected, err := s.Reject(ctx, testActor, deployment.ApprovalID, "  change window  ")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != ApprovalRejected || rejected.Reason != "change window" || rejected.DecidedBy != testActor.ID || rejected.DecidedAt == 0 {
		t.Fatalf("rejected = %+v", rejected)
	}
	got, err := s.GetDeployment(ctx, testActor, deployment.ID)
	if err != nil || got.Status != DeployRejected || got.Detail != "approval rejected" || got.FinishedAt == 0 {
		t.Fatalf("rejected deployment = %+v err=%v", got, err)
	}
	audit := approvalAuditRows(t, db, deployment.ApprovalID)
	rejectRow, rejectCount := approvalAction(audit, ActionApprovalReject)
	if rejectCount != 1 || rejectRow.Detail != "deployment="+deployment.ID+" reason=change window" {
		t.Fatalf("reject audit = %+v", audit)
	}
	// Idempotent repeat keeps the original reason and writes no second row.
	again, err := s.Reject(ctx, testActor, deployment.ApprovalID, "different")
	if err != nil || again.Reason != "change window" {
		t.Fatalf("repeat reject = %+v err=%v", again, err)
	}
	if audit = approvalAuditRows(t, db, deployment.ApprovalID); len(audit) != 2 {
		t.Fatalf("repeat reject wrote audit: %+v", audit)
	}

	// Invalid reasons leave the approval pending.
	pending, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{strings.Repeat("r", 513), "line\nbreak", "nul\x00byte"} {
		if _, err := s.Reject(ctx, testActor, pending.ApprovalID, reason); !errors.Is(err, ErrValidation) {
			t.Fatalf("reason %q error = %v", reason, err)
		}
	}
	if approvalForDeployment(t, s, pending.ID).Status != ApprovalPending {
		t.Fatal("invalid reason changed the approval")
	}
	// Empty reason is valid and the audit omits it.
	rejected, err = s.Reject(ctx, testActor, pending.ApprovalID, "")
	if err != nil || rejected.Reason != "" {
		t.Fatalf("empty reason reject = %+v err=%v", rejected, err)
	}
	if audit = approvalAuditRows(t, db, pending.ApprovalID); len(audit) != 2 {
		t.Fatalf("empty reason audit = %+v", audit)
	} else if row, count := approvalAction(audit, ActionApprovalReject); count != 1 || row.Detail != "deployment="+pending.ID {
		t.Fatalf("empty reason reject audit = %+v", audit)
	}

	// Approving or rejecting after the other decision is a conflict.
	if _, err = s.Approve(ctx, testActor, pending.ApprovalID); !errors.Is(err, ErrConflict) {
		t.Fatalf("approve after reject error = %v", err)
	}
}

func TestApproveAfterCancelConflictsAndCancelClosesApproval(t *testing.T) {
	s, db, registry := approvalService(t)
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	project := taskProject(t, registry, "approval-cancel")
	target := approvalTarget(t, s, "cancel-target", upstream.URL, EnvironmentProduction)
	deployment, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CancelDeployment(ctx, testActor, deployment.ID); err != nil {
		t.Fatal(err)
	}
	approval := approvalForDeployment(t, s, deployment.ID)
	if approval.Status != ApprovalExpired || approval.Reason != "deployment cancelled" || approval.DecidedAt == 0 {
		t.Fatalf("cancelled approval = %+v", approval)
	}
	audit := approvalAuditRows(t, db, approval.ID)
	expireRow, expireCount := approvalAction(audit, ActionApprovalExpire)
	if expireCount != 1 || expireRow.Detail != "deployment="+deployment.ID+" reason=deployment cancelled" || expireRow.ActorID != testActor.ID {
		t.Fatalf("cancel approval audit = %+v", audit)
	}
	// Repeat cancel is idempotent and does not duplicate either audit row.
	if _, err = s.CancelDeployment(ctx, testActor, deployment.ID); err != nil {
		t.Fatal(err)
	}
	if audit = approvalAuditRows(t, db, approval.ID); len(audit) != 2 {
		t.Fatalf("repeat cancel wrote approval audit: %+v", audit)
	}
	if rows := deploymentAudit(t, db, deployment.ID); len(rows) != 2 {
		t.Fatalf("repeat cancel wrote deployment audit: %v", rows)
	}
	if _, err = s.Approve(ctx, testActor, approval.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("approve after cancel error = %v", err)
	}
	if _, err = s.Reject(ctx, testActor, approval.ID, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("reject after cancel error = %v", err)
	}
}

func TestExpireApprovals(t *testing.T) {
	s, db, registry := approvalService(t)
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	project := taskProject(t, registry, "approval-expire")
	target := approvalTarget(t, s, "expire-target", upstream.URL, EnvironmentProduction)
	deployment, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	approval := approvalForDeployment(t, s, deployment.ID)
	if expired, err := s.ExpireApprovals(ctx, approval.ExpiresAt-1); err != nil || expired != 0 {
		t.Fatalf("early expiry = %d err=%v", expired, err)
	}
	expired, err := s.ExpireApprovals(ctx, approval.ExpiresAt)
	if err != nil || expired != 1 {
		t.Fatalf("expire = %d err=%v", expired, err)
	}
	approval = approvalForDeployment(t, s, deployment.ID)
	if approval.Status != ApprovalExpired || approval.Reason != "approval expired" || approval.DecidedAt != approval.ExpiresAt || approval.DecidedBy != "" {
		t.Fatalf("expired approval = %+v", approval)
	}
	got, err := s.GetDeployment(ctx, testActor, deployment.ID)
	if err != nil || got.Status != DeployRejected || got.Detail != "approval expired" || got.FinishedAt != approval.DecidedAt {
		t.Fatalf("expired deployment = %+v err=%v", got, err)
	}
	audit := approvalAuditRows(t, db, approval.ID)
	expireRow, expireCount := approvalAction(audit, ActionApprovalExpire)
	if expireCount != 1 || expireRow.ActorID != "" || expireRow.Detail != "deployment="+deployment.ID+" reason=approval expired" {
		t.Fatalf("expire audit = %+v", audit)
	}
	if expired, err = s.ExpireApprovals(ctx, approval.ExpiresAt+3600); err != nil || expired != 0 {
		t.Fatalf("repeat expiry = %d err=%v", expired, err)
	}

	// A second pending approval is handled by the same scan.
	second, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	secondApproval := approvalForDeployment(t, s, second.ID)
	if expired, err = s.ExpireApprovals(ctx, secondApproval.ExpiresAt); err != nil || expired != 1 {
		t.Fatalf("second expiry = %d err=%v", expired, err)
	}
	if got, err = s.GetDeployment(ctx, testActor, second.ID); err != nil || got.Status != DeployRejected {
		t.Fatalf("second deployment = %+v err=%v", got, err)
	}
}

func TestWorkerExpiryScanClosesApproval(t *testing.T) {
	s, _, registry := approvalService(t)
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	project := taskProject(t, registry, "approval-scan")
	target := approvalTarget(t, s, "scan-target", upstream.URL, EnvironmentProduction)
	deployment, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE approvals SET expires_at=1 WHERE deployment_id=$1`, deployment.ID); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s.expireApprovals(ctx, logger)
	approval := approvalForDeployment(t, s, deployment.ID)
	if approval.Status != ApprovalExpired {
		t.Fatalf("worker scan did not expire: %+v", approval)
	}
	if got, err := s.GetDeployment(ctx, testActor, deployment.ID); err != nil || got.Status != DeployRejected {
		t.Fatalf("deployment after worker scan = %+v err=%v", got, err)
	}
}

func TestListApprovals(t *testing.T) {
	s, _, registry := approvalService(t)
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	project := taskProject(t, registry, "approval-list")
	target := approvalTarget(t, s, "list-target", upstream.URL, EnvironmentProduction)
	var approvals []Approval
	for _, version := range []string{"v1", "v2", "v3"} {
		deployment, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: version})
		if err != nil {
			t.Fatal(err)
		}
		approvals = append(approvals, approvalForDeployment(t, s, deployment.ID))
	}
	// Distinct timestamps make the newest-first order deterministic.
	for index, approval := range approvals {
		if _, err := s.db.Exec(`UPDATE approvals SET created_at=$2 WHERE id=$1`, approval.ID, int64(1000+index)); err != nil {
			t.Fatal(err)
		}
	}
	list, total, err := s.ListApprovals(ctx, testActor, "", 2, 0)
	if err != nil || total != 3 || len(list) != 2 || list[0].ID != approvals[2].ID || list[1].ID != approvals[1].ID {
		t.Fatalf("list = %+v total=%d err=%v", list, total, err)
	}
	if list, total, err = s.ListApprovals(ctx, testActor, ApprovalPending, 10, 0); err != nil || total != 3 || len(list) != 3 {
		t.Fatalf("pending filter = %d total=%d err=%v", len(list), total, err)
	}
	if _, total, err = s.ListApprovals(ctx, testActor, ApprovalApproved, 10, 0); err != nil || total != 0 {
		t.Fatalf("approved filter total=%d err=%v", total, err)
	}
	if _, _, err = s.ListApprovals(ctx, testActor, "bogus", 10, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad status error = %v", err)
	}
	if _, _, err = s.ListApprovals(ctx, testActor, "", 0, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad bounds error = %v", err)
	}
	if _, _, err = s.ListApprovals(ctx, testActor, "", 10, -1); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad offset error = %v", err)
	}
}
