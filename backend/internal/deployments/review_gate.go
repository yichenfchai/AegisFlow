package deployments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
)

// requiresReview reports whether the target environment is gated on an
// approved review. The set is empty by default, so the gate is opt-in.
func (s *Service) requiresReview(environment string) bool {
	return s.reviewEnvironments[environment]
}

// requireApprovedReview applies the deployment review gate inside the caller's
// transaction, after the project write overlay and before the approval state
// machine: a deployment that needs a change review never reaches the queue (or
// the approval queue) without one.
//
// A supplied review id must reference an approved review of the same project.
// An unknown id, a review of another project, and a review that is not approved
// all return the same validation error on purpose: the deployment caller must
// not be able to use review ids as an existence oracle for other projects. When
// the target environment is in RequireReviewEnvironments, omitting the id is a
// validation error too. An empty id is accepted for environments that do not
// require a review, which is exactly the pre-gate behavior.
func (s *Service) requireApprovedReview(ctx context.Context, q rowQuerier, projectID, environment, reviewID string) error {
	required := s.requiresReview(environment)
	if reviewID == "" {
		if required {
			return fmt.Errorf("%w: an approved review of this project is required for deployments to the %s environment", ErrValidation, environment)
		}
		return nil
	}
	if !auth.ValidID(reviewID) {
		return fmt.Errorf("%w: invalid review id", ErrValidation)
	}
	// The review status vocabulary lives in internal/reviews; this package only
	// needs the terminal approved state, so it queries the literal exactly like
	// the buildId check queries the build's 'succeeded' status. Reviews are
	// immutable after a decision, so no lock is needed to pin the approval.
	var status string
	err := q.QueryRowContext(ctx, `SELECT status FROM reviews WHERE id=$1 AND project_id=$2`, reviewID, projectID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: reviewId must reference an approved review of this project", ErrValidation)
	}
	if err != nil {
		return err
	}
	if status != "approved" {
		return fmt.Errorf("%w: reviewId must reference an approved review of this project", ErrValidation)
	}
	return nil
}
