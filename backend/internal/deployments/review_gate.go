package deployments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
)

// scanVerdictPassed is the only latest-scan verdict accepted under the project
// policy gate. The scan vocabulary lives in internal/scans; this package only
// needs the terminal passed value, so it compares the literal exactly like the
// buildId check queries the build's 'succeeded' status.
const scanVerdictPassed = "passed"

// PolicyGate supplies the per-project automation policy inside the deployment
// transaction. internal/policies.Service implements it; the interface returns
// primitives only, so this package does not import the policy package (no cycle)
// and the gate stays testable with a fake. A nil gate keeps the policy gate
// disabled, which is exactly the behavior of a project without a policy row.
type PolicyGate interface {
	DeploymentGate(ctx context.Context, tx *sql.Tx, projectID string) (requireReview bool, requiredScanVerdict, latestScanVerdict string, err error)
}

// requiresReview reports whether the target environment is gated on an
// approved review. The set is empty by default, so the gate is opt-in.
func (s *Service) requiresReview(environment string) bool {
	return s.reviewEnvironments[environment]
}

// applyDeploymentGates applies the project automation policy and the environment
// review gate before the approval state machine, in the documented order: the
// review gate first, then the scan gate. The project policy can only add
// requirements: requireReview forces an approved same-project review regardless
// of the target environment, and requiredScanVerdict=passed requires the
// project's latest scan verdict to be passed. Both are off by default and for a
// project without a policy row.
//
// The boolean result reports whether a project policy contributed a requirement,
// so the caller can append policy=<projectId> to the deployment audit detail.
func (s *Service) applyDeploymentGates(ctx context.Context, tx *sql.Tx, projectID, environment, reviewID string) (bool, error) {
	requireReview := s.requiresReview(environment)
	policyApplied := false
	requiredScanVerdict := ""
	latestScanVerdict := ""
	if s.policyGate != nil {
		policyRequireReview, required, latest, err := s.policyGate.DeploymentGate(ctx, tx, projectID)
		if err != nil {
			return false, err
		}
		requireReview = requireReview || policyRequireReview
		requiredScanVerdict, latestScanVerdict = required, latest
		policyApplied = policyRequireReview || required == scanVerdictPassed
	}
	if err := s.requireApprovedReview(ctx, tx, projectID, environment, reviewID, requireReview); err != nil {
		return policyApplied, err
	}
	if requiredScanVerdict == scanVerdictPassed && latestScanVerdict != scanVerdictPassed {
		// The message names the failing gate so the shared error mapping and the
		// server log stay diagnosable without echoing any scan content.
		return policyApplied, fmt.Errorf("%w: scan gate: latest scan verdict must be passed", ErrValidation)
	}
	return policyApplied, nil
}

// requireApprovedReview applies the deployment review gate inside the caller's
// transaction, after the project write overlay and before the scan gate and the
// approval state machine: a deployment that needs a change review never reaches
// the queue (or the approval queue) without one.
//
// A supplied review id must reference an approved review of the same project.
// An unknown id, a review of another project, and a review that is not approved
// all return the same validation error on purpose: the deployment caller must
// not be able to use review ids as an existence oracle for other projects. When
// required is true (the target environment is in RequireReviewEnvironments or
// the project policy sets requireReview), omitting the id is a validation error
// too. An empty id is accepted when required is false, which is exactly the
// pre-gate behavior.
func (s *Service) requireApprovedReview(ctx context.Context, q rowQuerier, projectID, environment, reviewID string, required bool) error {
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
