package deployments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
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
// trigger time; history stays explainable after the target is edited. TenantID
// is server-side only: every query is tenant-scoped, so it never enters the JSON
// contract, but the worker carries it with the claimed row.
type Deployment struct {
	ID                string `json:"id"`
	TenantID          string `json:"-"`
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
	ReviewID          string `json:"reviewId"`
	TriggeredBy       string `json:"triggeredBy"`
	Detail            string `json:"detail"`
	CreatedAt         int64  `json:"createdAt"`
	StartedAt         int64  `json:"startedAt"`
	FinishedAt        int64  `json:"finishedAt"`
}

// DeployInput creates a deployment. BuildID is optional; when present it must
// reference a succeeded build of the same project. ReviewID is optional unless
// the target environment is in the configured review-required set; when present
// it must reference an approved review of the same project and is recorded on
// the deployment as a snapshot.
type DeployInput struct {
	TargetID string
	Version  string
	BuildID  string
	ReviewID string
}

// DeploymentEvent is one lifecycle (system) or upstream response line.
type DeploymentEvent struct {
	ID           int64  `json:"id"`
	DeploymentID string `json:"deploymentId"`
	Stream       string `json:"stream"`
	Message      string `json:"message"`
	CreatedAt    int64  `json:"createdAt"`
}

const deploymentColumns = `id,tenant_id,project_id,target_id,target_name,target_endpoint,target_environment,deployment_number,status,version,build_id,rollback_of,approval_id,review_id,triggered_by,detail,created_at,started_at,finished_at`

func scanDeployment(row rowScanner) (Deployment, error) {
	var d Deployment
	var reviewID sql.NullString
	if err := row.Scan(&d.ID, &d.TenantID, &d.ProjectID, &d.TargetID, &d.TargetName, &d.TargetEndpoint, &d.TargetEnvironment,
		&d.DeploymentNumber, &d.Status, &d.Version, &d.BuildID, &d.RollbackOf, &d.ApprovalID, &reviewID, &d.TriggeredBy,
		&d.Detail, &d.CreatedAt, &d.StartedAt, &d.FinishedAt); err != nil {
		return Deployment{}, err
	}
	if reviewID.Valid {
		d.ReviewID = reviewID.String
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

// targetSnapshotFor reads the target fields a new deployment snapshots, inside
// the deployment row's tenant. A target that is missing, belongs to another
// tenant or is disabled is a validation error: deploy tasks are admin-only, so
// there is no enumeration concern.
func targetSnapshotFor(ctx context.Context, q rowQuerier, tenantID, id string) (targetSnapshot, error) {
	var t targetSnapshot
	t.ID = id
	var hasSecret int
	err := q.QueryRowContext(ctx, `SELECT name,endpoint,deploy_path,environment,status,(octet_length(secret_cipher)>0)::int
	 FROM deployment_targets WHERE id=$1 AND tenant_id=$2`, id, tenantID).Scan(&t.Name, &t.Endpoint, &t.DeployPath, &t.Environment, &t.Status, &hasSecret)
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

// archivedProjectError is the deployment-side read-only rejection; it reuses the
// project registry wording so every write path reports the same reason.
func archivedProjectError() error {
	return fmt.Errorf("%w: archived project is read-only until restored", ErrConflict)
}

// requireActiveProjectForDeployment locks the project row and rejects a
// deployment write on an archived project. FOR SHARE serializes against the
// archive transition (which updates the project row) without upgrading the lock
// later, so it cannot deadlock against another deployment write.
func requireActiveProjectForDeployment(ctx context.Context, tx *sql.Tx, projectID, tenantID string) error {
	var status string
	err := tx.QueryRowContext(ctx, `SELECT status FROM projects WHERE id=$1 AND tenant_id=$2 FOR SHARE`, projectID, tenantID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != projects.StatusActive {
		return archivedProjectError()
	}
	return nil
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
	if err = s.authorizeProjectWrite(ctx, tx, actor, projectID); err != nil {
		return Deployment{}, err
	}
	target, err := targetSnapshotFor(ctx, tx, actor.TenantID, input.TargetID)
	if err != nil {
		return Deployment{}, err
	}
	// The deploy request carries the stored credential too, so the same
	// transport rule as the probe applies (https, or http to loopback only).
	if err = validateSecretTransport(target.Endpoint, target.HasSecret); err != nil {
		return Deployment{}, err
	}
	// The review gate runs before the approval state machine: a required or
	// supplied review must already be approved, so a deployment can never be
	// enqueued (or await approval) without its change review. The project policy
	// is part of the same check and can only add requirements.
	policyApplied, err := s.applyDeploymentGates(ctx, tx, projectID, target.Environment, input.ReviewID)
	if err != nil {
		return Deployment{}, err
	}
	deployment, err := s.enqueueDeployment(ctx, tx, actor, projectID, target, input.Version, input.BuildID, input.ReviewID, "", policyApplied, ActionDeployCreate)
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
// approval.request are audited. reviewID is the approved review that gated the
// deployment (already validated by the caller) and is snapshotted on the row.
// policyApplied reports that the project automation policy contributed a
// requirement, which the detail records as policy=<projectId> so an audit reader
// can tell why a deployment was gated even when the environment gate is off.
// Detail follows design §8: create records approval=required|none, rollback
// records the source and version; both append review=<id> when gated and
// policy=<projectId> when the project policy applied.
func (s *Service) enqueueDeployment(ctx context.Context, tx *sql.Tx, actor auth.Actor, projectID string, target targetSnapshot,
	version, buildID, reviewID, rollbackOf string, policyApplied bool, action string) (Deployment, error) {
	var count int64
	var projectStatus, projectTenantID string
	err := tx.QueryRowContext(ctx, `SELECT deploy_count,status,tenant_id FROM projects WHERE id=$1 AND tenant_id=$2 FOR UPDATE`, projectID, actor.TenantID).Scan(&count, &projectStatus, &projectTenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	// The project row lock is already held for the deployment-number
	// allocation, so the archived read-only rule is decided in the same
	// snapshot: an archive cannot slip between this check and the insert.
	if projectStatus != projects.StatusActive {
		return Deployment{}, archivedProjectError()
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
		ID: id, TenantID: projectTenantID, ProjectID: projectID, TargetID: target.ID, TargetName: target.Name,
		TargetEndpoint: target.Endpoint, TargetEnvironment: target.Environment, DeploymentNumber: count + 1,
		Status: status, Version: version, BuildID: buildID, RollbackOf: rollbackOf, ApprovalID: approvalID,
		ReviewID: reviewID, TriggeredBy: actor.ID, CreatedAt: now,
	}
	// review_id is nullable: an ungated deployment stores NULL rather than an
	// empty string, which would violate the review foreign key.
	var reviewValue any
	if reviewID != "" {
		reviewValue = reviewID
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO deployments(id,tenant_id,project_id,target_id,target_name,target_endpoint,target_environment,deployment_number,status,version,build_id,rollback_of,approval_id,review_id,triggered_by,detail,created_at)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'',$16)`,
		deployment.ID, projectTenantID, projectID, deployment.TargetID, deployment.TargetName, deployment.TargetEndpoint, deployment.TargetEnvironment,
		deployment.DeploymentNumber, status, version, buildID, rollbackOf, approvalID, reviewValue, actor.ID, now); err != nil {
		return Deployment{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE projects SET deploy_count=$2 WHERE id=$1 AND tenant_id=$3`, projectID, deployment.DeploymentNumber, projectTenantID); err != nil {
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
	if reviewID != "" {
		detail += " review=" + reviewID
	}
	if policyApplied {
		detail += " policy=" + projectID
	}
	if err = writeAuditObject(ctx, tx, actor, action, "deployment", deployment.ID, detail); err != nil {
		return Deployment{}, err
	}
	if approvalRequired {
		expiresAt := now + int64(s.approvalTTL/time.Second)
		approval := Approval{
			ID: approvalID, TenantID: projectTenantID, DeploymentID: deployment.ID, ProjectID: projectID,
			Status: ApprovalPending, RequestedBy: actor.ID, CreatedAt: now, ExpiresAt: expiresAt,
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO approvals(id,tenant_id,deployment_id,project_id,status,requested_by,decided_by,reason,created_at,expires_at,decided_at)
		 VALUES($1,$2,$3,$4,'pending',$5,'','',$6,$7,0)`,
			approval.ID, approval.TenantID, approval.DeploymentID, approval.ProjectID, approval.RequestedBy, approval.CreatedAt, approval.ExpiresAt); err != nil {
			return Deployment{}, err
		}
		if err = writeAuditObject(ctx, tx, actor, ActionApprovalRequest, "approval", approvalID, "deployment="+deployment.ID); err != nil {
			return Deployment{}, err
		}
		if err = s.enqueueApprovalNotification(ctx, tx, approval, notificationRequested); err != nil {
			return Deployment{}, err
		}
	}
	return deployment, nil
}

// GetDeployment returns one deployment the actor is allowed to see. The
// visibility join mirrors the build list: an invisible project is a 404, so a
// deployment id cannot be used to probe project existence.
func (s *Service) GetDeployment(ctx context.Context, actor auth.Actor, id string) (Deployment, error) {
	if !auth.ValidID(id) {
		return Deployment{}, fmt.Errorf("%w: invalid deployment id", ErrValidation)
	}
	isAdmin, userID := projects.VisibilityArgs(actor)
	deployment, err := scanDeployment(s.db.QueryRowContext(ctx,
		`SELECT `+deploymentColumnsQualified+` FROM deployments d JOIN projects p ON p.id=d.project_id
		 WHERE d.id=$1 AND `+projects.VisiblePredicate("p", 2, 3, 4), id, actor.TenantID, isAdmin, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	return deployment, nil
}

// ListDeployments pages deployments, newest first; projectID and status are
// optional filters. Only deployments of projects the actor can see are
// returned, and the project filter cannot widen that set.
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
	isAdmin, userID := projects.VisibilityArgs(actor)
	filter := ` FROM deployments d JOIN projects p ON p.id=d.project_id
		WHERE ($1='' OR d.project_id=$1) AND ($2='' OR d.status=$2) AND ` + projects.VisiblePredicate("p", 3, 4, 5)
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+filter, projectID, status, actor.TenantID, isAdmin, userID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+deploymentColumnsQualified+filter+`
		ORDER BY d.created_at DESC,d.deployment_number DESC,d.id DESC LIMIT $6 OFFSET $7`,
		projectID, status, actor.TenantID, isAdmin, userID, limit, offset)
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
	var approvalID, projectID string
	err = tx.QueryRowContext(ctx, `SELECT approval_id, project_id FROM deployments WHERE id=$1 AND tenant_id=$2`, id, actor.TenantID).Scan(&approvalID, &projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	if err = s.authorizeProjectWrite(ctx, tx, actor, projectID); err != nil {
		return Deployment{}, err
	}
	// Cancelling is a project write too: an archived project stays read-only.
	if err = requireActiveProjectForDeployment(ctx, tx, projectID, actor.TenantID); err != nil {
		return Deployment{}, err
	}
	var approval *Approval
	if approvalID != "" {
		locked, lockErr := lockApproval(ctx, tx, approvalID)
		if lockErr != nil && !errors.Is(lockErr, ErrNotFound) {
			return Deployment{}, lockErr
		}
		if lockErr == nil {
			// A deployment id and its approval always share a tenant; the check
			// is defense in depth against a corrupted cross-tenant link.
			if locked.TenantID != actor.TenantID {
				return Deployment{}, ErrNotFound
			}
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
		approval.Status = ApprovalExpired
		approval.Reason = "deployment cancelled"
		approval.DecidedAt = now
		if err := writeAuditObject(ctx, tx, actor, ActionApprovalExpire, "approval", approval.ID,
			"deployment="+current.ID+" reason=deployment cancelled"); err != nil {
			return err
		}
		return s.enqueueApprovalNotification(ctx, tx, *approval, notificationExpired)
	}
	if current.Status == DeployCancelled {
		if err := closePendingApproval(); err != nil {
			return Deployment{}, err
		}
		if err := tx.Commit(); err != nil {
			return Deployment{}, err
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
// deployment is not an idempotent operation. reviewID follows the same gate as
// a fresh deployment: it is required when the target environment is in the
// configured review-required set and must always reference an approved review of
// the same project.
func (s *Service) RollbackDeployment(ctx context.Context, actor auth.Actor, id, reviewID string) (Deployment, error) {
	if !auth.ValidID(id) {
		return Deployment{}, fmt.Errorf("%w: invalid deployment id", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Deployment{}, err
	}
	defer tx.Rollback()
	// Authorize before taking the deployment lock so a caller without write
	// access neither sees the row nor blocks it.
	var projectID string
	if err = tx.QueryRowContext(ctx, `SELECT project_id FROM deployments WHERE id=$1 AND tenant_id=$2`, id, actor.TenantID).Scan(&projectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Deployment{}, ErrNotFound
		}
		return Deployment{}, err
	}
	if err = s.authorizeProjectWrite(ctx, tx, actor, projectID); err != nil {
		return Deployment{}, err
	}
	source, err := scanDeployment(tx.QueryRowContext(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE id=$1 AND tenant_id=$2 FOR UPDATE`, id, actor.TenantID))
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
	target, err := targetSnapshotFor(ctx, tx, source.TenantID, source.TargetID)
	if err != nil {
		return Deployment{}, err
	}
	if err = validateSecretTransport(target.Endpoint, target.HasSecret); err != nil {
		return Deployment{}, err
	}
	// A rollback is a second deploy attempt of the same project, so it passes
	// the same review and project-policy gates as a fresh deployment before the
	// approval machine.
	policyApplied, err := s.applyDeploymentGates(ctx, tx, source.ProjectID, target.Environment, reviewID)
	if err != nil {
		return Deployment{}, err
	}
	deployment, err := s.enqueueDeployment(ctx, tx, actor, source.ProjectID, target, version, "", reviewID, source.ID, policyApplied, ActionDeployRollback)
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
