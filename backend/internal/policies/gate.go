package policies

import (
	"context"
	"database/sql"
	"errors"
)

// DeploymentGate returns the per-project gate inputs a deployment must apply
// inside the caller's transaction. It exists so the deployment service can read
// the policy and the project's latest scan verdict in the same snapshot as the
// deployment insert without importing this package: the method returns plain
// primitives, which lets internal/deployments declare its own small interface.
//
// The returned values are the effective gate inputs, not a decision: the
// deployment service combines requireReview with its environment review gate and
// compares latestScanVerdict with requiredScanVerdict. GateInput never fails for
// a project without a policy row or without scans; both mean "off".
//
// Scans are ordered exactly like the scan list (received_at DESC, id DESC), so
// "latest scan" means the same row a reader sees first. A project with no scan
// yields an empty latestScanVerdict; a project with a policy row yields the
// stored values. projectID must reference an existing project (the caller holds
// the project row lock in the deployment path, after its own tenant check), so
// the policy and scan reads are keyed by a globally unique project id and cannot
// cross a tenant boundary.
func (s *Service) DeploymentGate(ctx context.Context, tx *sql.Tx, projectID string) (requireReview bool, requiredScanVerdict, latestScanVerdict string, err error) {
	requiredScanVerdict = RequiredScanVerdictNone
	err = tx.QueryRowContext(ctx, `SELECT require_review,required_scan_verdict FROM project_policies WHERE project_id=$1`,
		projectID).Scan(&requireReview, &requiredScanVerdict)
	if errors.Is(err, sql.ErrNoRows) {
		// No row is the default (off) policy; keep reading the latest scan
		// anyway so tests and future policy extensions share one query shape.
		err = nil
		requiredScanVerdict = RequiredScanVerdictNone
	}
	if err != nil {
		return false, "", "", err
	}
	var verdict sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT verdict FROM scans WHERE project_id=$1
	 ORDER BY received_at DESC,id DESC LIMIT 1`, projectID).Scan(&verdict)
	if errors.Is(err, sql.ErrNoRows) {
		return requireReview, requiredScanVerdict, "", nil
	}
	if err != nil {
		return false, "", "", err
	}
	return requireReview, requiredScanVerdict, verdict.String, nil
}
