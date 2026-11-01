package deployments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
)

// Approval statuses (design §3). A deployment in awaiting_approval always has
// exactly one pending approval through the unique approvals_deployment_idx.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
	ApprovalExpired  = "expired"
)

const (
	ActionApprovalRequest = "approval.request"
	ActionApprovalApprove = "approval.approve"
	ActionApprovalReject  = "approval.reject"
	ActionApprovalExpire  = "approval.expire"
)

const (
	defaultApprovalTTL = 24 * time.Hour
	// defaultApprovalScanInterval is how often the lease holder expires
	// pending approvals whose deadline passed.
	defaultApprovalScanInterval = 30 * time.Second
	// maxApprovalExpiryBatch bounds one expiry scan; the worker repeats the
	// scan on its next tick, so a large backlog does not hold one transaction.
	maxApprovalExpiryBatch = 100
	// maxApprovalReasonBytes bounds the operator-provided rejection reason.
	maxApprovalReasonBytes = 512
)

// Approval is one deployment approval request.
type Approval struct {
	ID           string `json:"id"`
	DeploymentID string `json:"deploymentId"`
	ProjectID    string `json:"projectId"`
	Status       string `json:"status"`
	RequestedBy  string `json:"requestedBy"`
	DecidedBy    string `json:"decidedBy"`
	Reason       string `json:"reason"`
	CreatedAt    int64  `json:"createdAt"`
	ExpiresAt    int64  `json:"expiresAt"`
	DecidedAt    int64  `json:"decidedAt"`
}

const approvalColumns = `id,deployment_id,project_id,status,requested_by,decided_by,reason,created_at,expires_at,decided_at`

func scanApproval(row rowScanner) (Approval, error) {
	var a Approval
	if err := row.Scan(&a.ID, &a.DeploymentID, &a.ProjectID, &a.Status, &a.RequestedBy, &a.DecidedBy, &a.Reason,
		&a.CreatedAt, &a.ExpiresAt, &a.DecidedAt); err != nil {
		return Approval{}, err
	}
	return a, nil
}

func validApprovalStatus(status string) bool {
	switch status {
	case ApprovalPending, ApprovalApproved, ApprovalRejected, ApprovalExpired:
		return true
	default:
		return false
	}
}

// requiresApproval reports whether the target environment needs approval.
func (s *Service) requiresApproval(environment string) bool {
	return s.approvalEnvironments[environment]
}

// lockApproval takes the approval row lock. Approval flows always lock the
// approval before the deployment; CancelDeployment follows the same order so
// approve/reject/cancel/expire cannot deadlock each other.
func lockApproval(ctx context.Context, tx *sql.Tx, id string) (Approval, error) {
	approval, err := scanApproval(tx.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, ErrNotFound
	}
	return approval, err
}

// lockDeployment takes the deployment row lock after the approval lock.
func lockDeployment(ctx context.Context, tx *sql.Tx, id string) (Deployment, error) {
	deployment, err := scanDeployment(tx.QueryRowContext(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	return deployment, err
}

// normalizeApprovalReason trims and bounds the optional rejection reason; it
// becomes an audit detail and a stored field, so control characters (including
// line breaks) are rejected rather than escaped.
func normalizeApprovalReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if !utf8.ValidString(reason) || len(reason) > maxApprovalReasonBytes {
		return "", fmt.Errorf("%w: reason must be valid UTF-8 of at most %d bytes", ErrValidation, maxApprovalReasonBytes)
	}
	if strings.ContainsFunc(reason, unicode.IsControl) {
		return "", fmt.Errorf("%w: reason must be a single line without control characters", ErrValidation)
	}
	return reason, nil
}

// Approve moves a pending approval to approved and its deployment from
// awaiting_approval to queued, then wakes the worker. Repeating on an approved
// approval is idempotent and writes no second audit row. A deployment that is
// no longer awaiting approval is a conflict; the approval stays pending so the
// expiry scan can close it.
func (s *Service) Approve(ctx context.Context, actor auth.Actor, approvalID string) (Approval, error) {
	if !auth.ValidID(approvalID) {
		return Approval{}, fmt.Errorf("%w: invalid approval id", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Approval{}, err
	}
	defer tx.Rollback()
	approval, err := lockApproval(ctx, tx, approvalID)
	if err != nil {
		return Approval{}, err
	}
	if approval.Status == ApprovalApproved {
		return approval, nil
	}
	if approval.Status != ApprovalPending {
		return Approval{}, fmt.Errorf("%w: only pending approvals can be approved", ErrConflict)
	}
	deployment, err := lockDeployment(ctx, tx, approval.DeploymentID)
	if err != nil {
		return Approval{}, err
	}
	if deployment.Status != DeployAwaitingApproval {
		return Approval{}, fmt.Errorf("%w: deployment is no longer awaiting approval", ErrConflict)
	}
	now := time.Now().Unix()
	approval, err = scanApproval(tx.QueryRowContext(ctx, `UPDATE approvals SET status=$2,decided_by=$3,decided_at=$4 WHERE id=$1 RETURNING `+approvalColumns,
		approvalID, ApprovalApproved, actor.ID, now))
	if err != nil {
		return Approval{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deployments SET status=$2 WHERE id=$1 AND status=$3`,
		deployment.ID, DeployQueued, DeployAwaitingApproval); err != nil {
		return Approval{}, err
	}
	if err = writeAuditObject(ctx, tx, actor, ActionApprovalApprove, "approval", approvalID, "deployment="+deployment.ID); err != nil {
		return Approval{}, err
	}
	if err = tx.Commit(); err != nil {
		return Approval{}, err
	}
	s.wake()
	return approval, nil
}

// Reject moves a pending approval to rejected and its deployment to rejected.
// Repeating on a rejected approval is idempotent and writes no second audit
// row; every other non-pending status is a conflict.
func (s *Service) Reject(ctx context.Context, actor auth.Actor, approvalID, reason string) (Approval, error) {
	if !auth.ValidID(approvalID) {
		return Approval{}, fmt.Errorf("%w: invalid approval id", ErrValidation)
	}
	reason, err := normalizeApprovalReason(reason)
	if err != nil {
		return Approval{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Approval{}, err
	}
	defer tx.Rollback()
	approval, err := lockApproval(ctx, tx, approvalID)
	if err != nil {
		return Approval{}, err
	}
	if approval.Status == ApprovalRejected {
		return approval, nil
	}
	if approval.Status != ApprovalPending {
		return Approval{}, fmt.Errorf("%w: only pending approvals can be rejected", ErrConflict)
	}
	deployment, err := lockDeployment(ctx, tx, approval.DeploymentID)
	if err != nil {
		return Approval{}, err
	}
	if deployment.Status != DeployAwaitingApproval {
		return Approval{}, fmt.Errorf("%w: deployment is no longer awaiting approval", ErrConflict)
	}
	now := time.Now().Unix()
	approval, err = scanApproval(tx.QueryRowContext(ctx, `UPDATE approvals SET status=$2,decided_by=$3,reason=$4,decided_at=$5 WHERE id=$1 RETURNING `+approvalColumns,
		approvalID, ApprovalRejected, actor.ID, reason, now))
	if err != nil {
		return Approval{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deployments SET status=$2,finished_at=$3,detail=$4 WHERE id=$1 AND status=$5`,
		deployment.ID, DeployRejected, now, "approval rejected", DeployAwaitingApproval); err != nil {
		return Approval{}, err
	}
	detail := "deployment=" + deployment.ID
	if reason != "" {
		detail += " reason=" + reason
	}
	if err = writeAuditObject(ctx, tx, actor, ActionApprovalReject, "approval", approvalID, detail); err != nil {
		return Approval{}, err
	}
	if err = tx.Commit(); err != nil {
		return Approval{}, err
	}
	return approval, nil
}

// ListApprovals pages approvals, newest first; status may be empty or one of
// the four approval statuses.
func (s *Service) ListApprovals(ctx context.Context, actor auth.Actor, status string, limit, offset int) ([]Approval, int, error) {
	if status != "" && !validApprovalStatus(status) {
		return nil, 0, fmt.Errorf("%w: status filter must be pending, approved, rejected or expired", ErrValidation)
	}
	if err := paginate(limit, offset); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE ($1='' OR status=$1)`, status).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+approvalColumns+` FROM approvals
	 WHERE ($1='' OR status=$1) ORDER BY created_at DESC,id DESC LIMIT $2 OFFSET $3`, status, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	approvals := make([]Approval, 0)
	for rows.Next() {
		approval, err := scanApproval(rows)
		if err != nil {
			return nil, 0, err
		}
		approvals = append(approvals, approval)
	}
	return approvals, total, rows.Err()
}

// ExpireApprovals closes pending approvals whose deadline has passed and
// rejects their deployments. It locks at most maxApprovalExpiryBatch rows with
// SKIP LOCKED, so several replicas can scan concurrently. The audit actor is
// empty: expiry is a system action.
func (s *Service) ExpireApprovals(ctx context.Context, now int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+approvalColumns+` FROM approvals
	 WHERE status=$1 AND expires_at<=$2 ORDER BY expires_at,id LIMIT $3 FOR UPDATE SKIP LOCKED`,
		ApprovalPending, now, maxApprovalExpiryBatch)
	if err != nil {
		return 0, err
	}
	var pending []Approval
	for rows.Next() {
		approval, err := scanApproval(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		pending = append(pending, approval)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	var expired int64
	for _, approval := range pending {
		if _, err = tx.ExecContext(ctx, `UPDATE approvals SET status=$2,reason=$3,decided_at=$4 WHERE id=$1`,
			approval.ID, ApprovalExpired, "approval expired", now); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE deployments SET status=$2,finished_at=$3,detail=$4 WHERE id=$1 AND status=$5`,
			approval.DeploymentID, DeployRejected, now, "approval expired", DeployAwaitingApproval); err != nil {
			return 0, err
		}
		detail := "deployment=" + approval.DeploymentID + " reason=approval expired"
		if err = writeAuditObject(ctx, tx, auth.Actor{}, ActionApprovalExpire, "approval", approval.ID, detail); err != nil {
			return 0, err
		}
		expired++
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return expired, nil
}
