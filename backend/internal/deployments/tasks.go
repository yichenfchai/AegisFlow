package deployments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
)

// Deployment statuses (design §3). awaiting_approval and rejected are part of
// the schema so P4.3 is additive; this slice always enqueues directly and never
// sets them.
const (
	DeployQueued           = "queued"
	DeployAwaitingApproval = "awaiting_approval"
	DeployRunning          = "running"
	DeploySucceeded        = "succeeded"
	DeployFailed           = "failed"
	DeployCancelled        = "cancelled"
	DeployInterrupted      = "interrupted"
	DeployRejected         = "rejected"
)

const (
	ActionDeployCreate   = "deployment.create"
	ActionDeployCancel   = "deployment.cancel"
	ActionDeployRollback = "deployment.rollback"
)

// versionPattern accepts a tag, short SHA or semantic version, and rejects
// whitespace and path separators (design §7).
var versionPattern = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)

// Deployment is one deploy attempt. Target fields are the snapshot taken at
// trigger time; history stays explainable after the target is edited.
type Deployment struct {
	ID                string `json:"id"`
	ProjectID         string `json:"projectId"`
	TargetID          string `json:"targetId"`
	TargetName        string `json:"targetName"`
	TargetEndpoint    string `json:"targetEndpoint"`
	TargetEnvironment string `json:"targetEnvironment"`
	DeploymentNumber  int64  `json:"deploymentNumber"`
	Status            string `json:"status"`
	Version           string `json:"version"`
	BuildID           string `json:"buildId"`
	RollbackOf        string `json:"rollbackOf"`
	ApprovalID        string `json:"approvalId"`
	TriggeredBy       string `json:"triggeredBy"`
	Detail            string `json:"detail"`
	CreatedAt         int64  `json:"createdAt"`
	StartedAt         int64  `json:"startedAt"`
	FinishedAt        int64  `json:"finishedAt"`
}

// DeployInput creates a deployment. BuildID is optional; when present it must
// reference a succeeded build of the same project.
type DeployInput struct {
	TargetID string
	Version  string
	BuildID  string
}

// DeploymentEvent is one lifecycle (system) or upstream response line.
type DeploymentEvent struct {
	ID           int64  `json:"id"`
	DeploymentID string `json:"deploymentId"`
	Stream       string `json:"stream"`
	Message      string `json:"message"`
	CreatedAt    int64  `json:"createdAt"`
}

const deploymentColumns = `id,project_id,target_id,target_name,target_endpoint,target_environment,deployment_number,status,version,build_id,rollback_of,approval_id,triggered_by,detail,created_at,started_at,finished_at`

func scanDeployment(row rowScanner) (Deployment, error) {
	var d Deployment
	if err := row.Scan(&d.ID, &d.ProjectID, &d.TargetID, &d.TargetName, &d.TargetEndpoint, &d.TargetEnvironment,
		&d.DeploymentNumber, &d.Status, &d.Version, &d.BuildID, &d.RollbackOf, &d.ApprovalID, &d.TriggeredBy,
		&d.Detail, &d.CreatedAt, &d.StartedAt, &d.FinishedAt); err != nil {
		return Deployment{}, err
	}
	return d, nil
}

// rowQuerier is satisfied by *sql.DB and *sql.Tx.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// targetSnapshot is the target state captured into a deployment row.
type targetSnapshot struct {
	ID          string
	Name        string
	Endpoint    string
	DeployPath  string
	Environment string
	Status      string
	HasSecret   bool
}

// targetSnapshotFor reads the target fields a new deployment snapshots. A
// target that is missing or disabled is a validation error: deploy tasks are
// admin-only, so there is no enumeration concern.
func targetSnapshotFor(ctx context.Context, q rowQuerier, id string) (targetSnapshot, error) {
	var t targetSnapshot
	t.ID = id
	var hasSecret int
	err := q.QueryRowContext(ctx, `SELECT name,endpoint,deploy_path,environment,status,(octet_length(secret_cipher)>0)::int
	 FROM deployment_targets WHERE id=$1`, id).Scan(&t.Name, &t.Endpoint, &t.DeployPath, &t.Environment, &t.Status, &hasSecret)
	if errors.Is(err, sql.ErrNoRows) {
		return targetSnapshot{}, fmt.Errorf("%w: deployment target not found", ErrValidation)
	}
	if err != nil {
		return targetSnapshot{}, err
	}
	t.HasSecret = hasSecret != 0
	if t.Status != StatusActive {
		return targetSnapshot{}, fmt.Errorf("%w: deployment target is disabled", ErrValidation)
	}
	return t, nil
}

func validVersion(version string) error {
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("%w: version must be 1-64 characters of A-Z a-z 0-9 . _ + -", ErrValidation)
	}
	return nil
}

func validDeploymentStatus(status string) bool {
	switch status {
	case DeployQueued, DeployAwaitingApproval, DeployRunning, DeploySucceeded, DeployFailed, DeployCancelled, DeployInterrupted, DeployRejected:
		return true
	default:
		return false
	}
}

// CreateDeployment enqueues one deploy attempt. The project row lock allocates
// deployment_number from projects.deploy_count (the build_count pattern), the
// target snapshot is captured inside the same transaction, and the audit row
// commits with the deployment.
func (s *Service) CreateDeployment(ctx context.Context, actor auth.Actor, projectID string, input DeployInput) (Deployment, error) {
	if !auth.ValidID(projectID) {
		return Deployment{}, fmt.Errorf("%w: invalid project id", ErrValidation)
	}
	if !auth.ValidID(input.TargetID) {
		return Deployment{}, fmt.Errorf("%w: invalid deployment target id", ErrValidation)
	}
	if err := validVersion(input.Version); err != nil {
		return Deployment{}, err
	}
	if input.BuildID != "" && !auth.ValidID(input.BuildID) {
		return Deployment{}, fmt.Errorf("%w: invalid build id", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Deployment{}, err
	}
	defer tx.Rollback()
	target, err := targetSnapshotFor(ctx, tx, input.TargetID)
	if err != nil {
		return Deployment{}, err
	}
	// The deploy request carries the stored credential too, so the same
	// transport rule as the probe applies (https, or http to loopback only).
	if err = validateSecretTransport(target.Endpoint, target.HasSecret); err != nil {
		return Deployment{}, err
	}
	deployment, err := s.enqueueDeployment(ctx, tx, actor, projectID, target, input.Version, input.BuildID, "", ActionDeployCreate)
	if err != nil {
		return Deployment{}, err
	}
	if err = tx.Commit(); err != nil {
		return Deployment{}, err
	}
	s.wake()
	return deployment, nil
}

// enqueueDeployment inserts a deployment inside the caller's transaction and
// writes its audit row there. The project row is locked so concurrent enqueues
// allocate distinct deployment numbers. When the target environment requires
// approval, the row starts awaiting_approval, a pending approval is created in
// the same transaction, approval_id is set, and both the deployment action and
// approval.request are audited. Detail follows design §8: create records
// approval=required|none, rollback records the source and version.
func (s *Service) enqueueDeployment(ctx context.Context, tx *sql.Tx, actor auth.Actor, projectID string, target targetSnapshot,
	version, buildID, rollbackOf, action string) (Deployment, error) {
	var count int64
	err := tx.QueryRowContext(ctx, `SELECT deploy_count FROM projects WHERE id=$1 FOR UPDATE`, projectID).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	if buildID != "" {
		var exists int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM builds WHERE id=$1 AND project_id=$2 AND status='succeeded'`, buildID, projectID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return Deployment{}, fmt.Errorf("%w: buildId must reference a succeeded build of this project", ErrValidation)
		}
		if err != nil {
			return Deployment{}, err
		}
	}
	approvalRequired := s.requiresApproval(target.Environment)
	approvalID := ""
	status := DeployQueued
	if approvalRequired {
		if approvalID, err = newID(); err != nil {
			return Deployment{}, err
		}
		status = DeployAwaitingApproval
	}
	id, err := newID()
	if err != nil {
		return Deployment{}, err
	}
	now := time.Now().Unix()
	deployment := Deployment{
		ID: id, ProjectID: projectID, TargetID: target.ID, TargetName: target.Name,
		TargetEndpoint: target.Endpoint, TargetEnvironment: target.Environment, DeploymentNumber: count + 1,
		Status: status, Version: version, BuildID: buildID, RollbackOf: rollbackOf, ApprovalID: approvalID,
		TriggeredBy: actor.ID, CreatedAt: now,
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO deployments(id,project_id,target_id,target_name,target_endpoint,target_environment,deployment_number,status,version,build_id,rollback_of,approval_id,triggered_by,detail,created_at)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'',$14)`,
		deployment.ID, projectID, deployment.TargetID, deployment.TargetName, deployment.TargetEndpoint, deployment.TargetEnvironment,
		deployment.DeploymentNumber, status, version, buildID, rollbackOf, approvalID, actor.ID, now); err != nil {
		return Deployment{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE projects SET deploy_count=$2 WHERE id=$1`, projectID, deployment.DeploymentNumber); err != nil {
		return Deployment{}, err
	}
	detail := ""
	switch action {
	case ActionDeployCreate:
		approvalWord := "none"
		if approvalRequired {
			approvalWord = "required"
		}
		detail = fmt.Sprintf("target=%s version=%s approval=%s", target.Name, version, approvalWord)
	case ActionDeployRollback:
		detail = fmt.Sprintf("rollback_of=%s version=%s", rollbackOf, version)
	}
	if err = writeAuditObject(ctx, tx, actor, action, "deployment", deployment.ID, detail); err != nil {
		return Deployment{}, err
	}
	if approvalRequired {
		expiresAt := now + int64(s.approvalTTL/time.Second)
		if _, err = tx.ExecContext(ctx, `INSERT INTO approvals(id,deployment_id,project_id,status,requested_by,decided_by,reason,created_at,expires_at,decided_at)
		 VALUES($1,$2,$3,'pending',$4,'','',$5,$6,0)`,
			approvalID, deployment.ID, projectID, actor.ID, now, expiresAt); err != nil {
			return Deployment{}, err
		}
		if err = writeAuditObject(ctx, tx, actor, ActionApprovalRequest, "approval", approvalID, "deployment="+deployment.ID); err != nil {
			return Deployment{}, err
		}
	}
	return deployment, nil
}

// GetDeployment returns one deployment. Callers are already authorized by the
// HTTP layer (admin only).
func (s *Service) GetDeployment(ctx context.Context, actor auth.Actor, id string) (Deployment, error) {
	if !auth.ValidID(id) {
		return Deployment{}, fmt.Errorf("%w: invalid deployment id", ErrValidation)
	}
	deployment, err := scanDeployment(s.db.QueryRowContext(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	return deployment, nil
}

// ListDeployments pages deployments, newest first; projectID and status are
// optional filters.
func (s *Service) ListDeployments(ctx context.Context, actor auth.Actor, projectID, status string, limit, offset int) ([]Deployment, int, error) {
	if projectID != "" && !auth.ValidID(projectID) {
		return nil, 0, fmt.Errorf("%w: invalid project id", ErrValidation)
	}
	if status != "" && !validDeploymentStatus(status) {
		return nil, 0, fmt.Errorf("%w: status filter must be a deployment status", ErrValidation)
	}
	if err := paginate(limit, offset); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployments WHERE ($1='' OR project_id=$1) AND ($2='' OR status=$2)`,
		projectID, status).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+deploymentColumns+` FROM deployments
	 WHERE ($1='' OR project_id=$1) AND ($2='' OR status=$2)
	 ORDER BY created_at DESC,deployment_number DESC,id DESC LIMIT $3 OFFSET $4`, projectID, status, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	list := make([]Deployment, 0)
	for rows.Next() {
		deployment, err := scanDeployment(rows)
		if err != nil {
			return nil, 0, err
		}
		list = append(list, deployment)
	}
	return list, total, rows.Err()
}

// CancelDeployment moves a queued, awaiting-approval or running deployment to
// cancelled. Cancelling an awaiting deployment also closes its pending approval
// as expired with reason "deployment cancelled" (the approval state machine has
// no cancelled state). The approval row is locked before the deployment, the
// same order approve/reject/expire use. Repeating on an already-cancelled
// deployment is idempotent and writes no second audit row; any other terminal
// deployment is a conflict.
func (s *Service) CancelDeployment(ctx context.Context, actor auth.Actor, id string) (Deployment, error) {
	if !auth.ValidID(id) {
		return Deployment{}, fmt.Errorf("%w: invalid deployment id", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Deployment{}, err
	}
	defer tx.Rollback()
	// approval_id never changes after creation, so it can be read without the
	// deployment lock; taking the approval lock first keeps the lock order
	// consistent with approve/reject/expire.
	var approvalID string
	err = tx.QueryRowContext(ctx, `SELECT approval_id FROM deployments WHERE id=$1`, id).Scan(&approvalID)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	var approval *Approval
	if approvalID != "" {
		locked, lockErr := lockApproval(ctx, tx, approvalID)
		if lockErr != nil && !errors.Is(lockErr, ErrNotFound) {
			return Deployment{}, lockErr
		}
		if lockErr == nil {
			approval = &locked
		}
	}
	current, err := lockDeployment(ctx, tx, id)
	if err != nil {
		return Deployment{}, err
	}
	closePendingApproval := func() error {
		if approval == nil || approval.Status != ApprovalPending {
			return nil
		}
		now := time.Now().Unix()
		if _, err := tx.ExecContext(ctx, `UPDATE approvals SET status=$2,reason=$3,decided_at=$4 WHERE id=$1`,
			approval.ID, ApprovalExpired, "deployment cancelled", now); err != nil {
			return err
		}
		return writeAuditObject(ctx, tx, actor, ActionApprovalExpire, "approval", approval.ID,
			"deployment="+current.ID+" reason=deployment cancelled")
	}
	if current.Status == DeployCancelled {
		if err := closePendingApproval(); err != nil {
			return Deployment{}, err
		}
		if approval != nil && approval.Status == ApprovalPending {
			if err := tx.Commit(); err != nil {
				return Deployment{}, err
			}
		}
		return current, nil
	}
	switch current.Status {
	case DeployQueued, DeployAwaitingApproval, DeployRunning:
	default:
		return Deployment{}, fmt.Errorf("%w: only queued, awaiting-approval or running deployments can be cancelled", ErrConflict)
	}
	updated, err := scanDeployment(tx.QueryRowContext(ctx, `UPDATE deployments SET status=$2, finished_at=$3 WHERE id=$1 RETURNING `+deploymentColumns,
		id, DeployCancelled, time.Now().Unix()))
	if err != nil {
		return Deployment{}, err
	}
	if err = writeAuditObject(ctx, tx, actor, ActionDeployCancel, "deployment", id, "from="+current.Status); err != nil {
		return Deployment{}, err
	}
	if err = closePendingApproval(); err != nil {
		return Deployment{}, err
	}
	if err = tx.Commit(); err != nil {
		return Deployment{}, err
	}
	return updated, nil
}

// RollbackDeployment creates a new deployment for the most recent earlier
// succeeded version of the same project and target (design §5). The source must
// be terminal; repeated rollbacks create multiple rows on purpose, because a
// deployment is not an idempotent operation.
func (s *Service) RollbackDeployment(ctx context.Context, actor auth.Actor, id string) (Deployment, error) {
	if !auth.ValidID(id) {
		return Deployment{}, fmt.Errorf("%w: invalid deployment id", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Deployment{}, err
	}
	defer tx.Rollback()
	source, err := scanDeployment(tx.QueryRowContext(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	if source.Status != DeploySucceeded && source.Status != DeployFailed {
		return Deployment{}, fmt.Errorf("%w: only succeeded or failed deployments can be rolled back", ErrConflict)
	}
	var version string
	err = tx.QueryRowContext(ctx, `SELECT version FROM deployments
	 WHERE project_id=$1 AND target_id=$2 AND status=$3 AND deployment_number<$4
	 ORDER BY deployment_number DESC LIMIT 1`,
		source.ProjectID, source.TargetID, DeploySucceeded, source.DeploymentNumber).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, fmt.Errorf("%w: no earlier succeeded deployment to roll back to", ErrValidation)
	}
	if err != nil {
		return Deployment{}, err
	}
	target, err := targetSnapshotFor(ctx, tx, source.TargetID)
	if err != nil {
		return Deployment{}, err
	}
	if err = validateSecretTransport(target.Endpoint, target.HasSecret); err != nil {
		return Deployment{}, err
	}
	deployment, err := s.enqueueDeployment(ctx, tx, actor, source.ProjectID, target, version, "", source.ID, ActionDeployRollback)
	if err != nil {
		return Deployment{}, err
	}
	if err = tx.Commit(); err != nil {
		return Deployment{}, err
	}
	s.wake()
	return deployment, nil
}

// DeploymentEvents returns events after the exclusive cursor, oldest first; the
// id doubles as the client resume cursor (the builds log semantics).
func (s *Service) DeploymentEvents(ctx context.Context, actor auth.Actor, deploymentID string, after int64, limit int) ([]DeploymentEvent, error) {
	if !auth.ValidID(deploymentID) {
		return nil, fmt.Errorf("%w: invalid deployment id", ErrValidation)
	}
	if after < 0 {
		return nil, fmt.Errorf("%w: after must be non-negative", ErrValidation)
	}
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("%w: limit must be 1-1000", ErrValidation)
	}
	if _, err := s.GetDeployment(ctx, actor, deploymentID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,deployment_id,stream,message,created_at FROM deployment_events
	 WHERE deployment_id=$1 AND id>$2 ORDER BY id LIMIT $3`, deploymentID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]DeploymentEvent, 0)
	for rows.Next() {
		var event DeploymentEvent
		if err := rows.Scan(&event.ID, &event.DeploymentID, &event.Stream, &event.Message, &event.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}
