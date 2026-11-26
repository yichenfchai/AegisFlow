package reviews

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
)

// decision describes one terminal transition of the review state machine.
type decision struct {
	target         string
	idempotent     string
	action         string
	event          string
	verb           string
	requiredReason bool
}

// Approve moves a pending review to approved with the reviewer overlay
// (global admin or project maintainer). The optional reason is trimmed and
// stored with the decision. Repeating on an already approved review returns the
// same review without a second audit row; every other non-pending status is a
// conflict.
func (s *Service) Approve(ctx context.Context, actor auth.Actor, reviewID, reason string) (*Review, error) {
	return s.decide(ctx, actor, reviewID, reason, decision{
		target: StatusApproved, idempotent: StatusApproved, action: ActionApprove, event: EventApproved, verb: "approved",
	})
}

// Reject moves a pending review to rejected. reason is required: a rejection
// without an explanation is not accepted. Repeating on an already rejected
// review returns the same review without a second audit row; every other
// non-pending status is a conflict.
func (s *Service) Reject(ctx context.Context, actor auth.Actor, reviewID, reason string) (*Review, error) {
	return s.decide(ctx, actor, reviewID, reason, decision{
		target: StatusRejected, idempotent: StatusRejected, action: ActionReject, event: EventRejected, verb: "rejected", requiredReason: true,
	})
}

// decide is the shared approve/reject path: one transaction locks the review,
// applies the visibility and reviewer overlays, enforces the pending state, and
// commits the terminal status together with its audit row.
func (s *Service) decide(ctx context.Context, actor auth.Actor, reviewID, reason string, kind decision) (*Review, error) {
	if !auth.ValidID(reviewID) {
		return nil, fmt.Errorf("%w: invalid review id", ErrValidation)
	}
	reason, err := normalizeReason(reason, kind.requiredReason)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	locked, err := lockReview(ctx, tx, reviewID)
	if err != nil {
		return nil, err
	}
	// Visibility first: an invisible project stays a 404 and its review ids
	// cannot be enumerated. Then the reviewer overlay decides 403.
	if err = visibleProject(ctx, tx, actor, locked.ProjectID); err != nil {
		return nil, err
	}
	if err = s.authorizeDecide(ctx, tx, actor, locked.ProjectID); err != nil {
		return nil, err
	}
	if locked.Status == kind.idempotent {
		// Idempotent repeat: return the stored decision unchanged, no audit row.
		return getReviewTx(ctx, tx, reviewID)
	}
	if locked.Status != StatusPending {
		return nil, fmt.Errorf("%w: only pending reviews can be %s", ErrConflict, kind.verb)
	}
	now := time.Now().Unix()
	reasonArg := sql.NullString{String: reason, Valid: reason != ""}
	if _, err = tx.ExecContext(ctx, `UPDATE reviews SET status=$2,decided_by=$3,decided_at=$4,reason=$5,updated_at=$4 WHERE id=$1`,
		reviewID, kind.target, actor.ID, now, reasonArg); err != nil {
		return nil, err
	}
	review, err := getReviewTx(ctx, tx, reviewID)
	if err != nil {
		return nil, err
	}
	detail := fmt.Sprintf("number=%d", review.Number)
	if reason != "" {
		detail += " reason=" + reason
	}
	if err = writeAudit(ctx, tx, actor, kind.action, reviewID, detail); err != nil {
		return nil, err
	}
	// The event row is part of the same transaction as the decision, so the
	// outbox can never announce a status that did not commit.
	if err = s.enqueueReviewNotification(ctx, tx, review, kind.event); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return review, nil
}

// Cancel closes a pending review as cancelled. The requester may always cancel
// their own request; anyone else needs the reviewer overlay (global admin or
// project maintainer). Repeating on an already cancelled review is idempotent
// and writes no second audit row; every other terminal status is a conflict.
// decidedBy/decidedAt record who cancelled and when; reason stays null because
// the cancel payload carries no reason.
func (s *Service) Cancel(ctx context.Context, actor auth.Actor, reviewID string) (*Review, error) {
	if !auth.ValidID(reviewID) {
		return nil, fmt.Errorf("%w: invalid review id", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	locked, err := lockReview(ctx, tx, reviewID)
	if err != nil {
		return nil, err
	}
	if err = visibleProject(ctx, tx, actor, locked.ProjectID); err != nil {
		return nil, err
	}
	// The requester may withdraw their own pending request even after losing
	// write access; everybody else needs the reviewer overlay.
	if actor.ID != locked.RequestedBy {
		if err = s.authorizeDecide(ctx, tx, actor, locked.ProjectID); err != nil {
			return nil, err
		}
	}
	if locked.Status == StatusCancelled {
		return getReviewTx(ctx, tx, reviewID)
	}
	if locked.Status != StatusPending {
		return nil, fmt.Errorf("%w: only pending reviews can be cancelled", ErrConflict)
	}
	now := time.Now().Unix()
	if _, err = tx.ExecContext(ctx, `UPDATE reviews SET status=$2,decided_by=$3,decided_at=$4,updated_at=$4 WHERE id=$1`,
		reviewID, StatusCancelled, actor.ID, now); err != nil {
		return nil, err
	}
	review, err := getReviewTx(ctx, tx, reviewID)
	if err != nil {
		return nil, err
	}
	if err = writeAudit(ctx, tx, actor, ActionCancel, reviewID, fmt.Sprintf("number=%d", review.Number)); err != nil {
		return nil, err
	}
	if err = s.enqueueReviewNotification(ctx, tx, review, EventCancelled); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return review, nil
}
