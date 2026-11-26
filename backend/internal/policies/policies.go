// Package policies owns the per-project automation policy: the opt-in gate rules
// that compose with the deployment review gate (target environments listed in
// AEGIS_DEPLOY_REQUIRE_REVIEW_ENVIRONMENTS) and the approval state machine. A
// project without a stored row uses the default policy (requireReview=false,
// requiredScanVerdict=none), so writing a policy is additive and an upgrade
// keeps every existing deployment path unchanged.
//
// The package has no Gin dependency. Reads and writes take a context.Context; the
// deployment-facing gate read runs inside the caller's transaction through
// DeploymentGate, and internal/deployments consumes it through a small interface
// (primitives only) so the two packages never import each other.
package policies

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
)

// RequiredScanVerdict values. "none" disables the scan gate; "passed" requires
// the project's latest scan verdict to be "passed" before a deployment may be
// created or rolled back.
const (
	RequiredScanVerdictNone   = "none"
	RequiredScanVerdictPassed = "passed"
)

// ActionUpdate is the audited action of a policy write.
const ActionUpdate = "policy.update"

// Errors map to the shared HTTP envelope in the HTTP layer: validation is 400,
// an invisible project is 404 and a visible project the caller may not gate is
// 403.
var (
	ErrValidation = errors.New("validation failed")
	ErrNotFound   = errors.New("not found")
	ErrForbidden  = errors.New("forbidden")
)

// Policy is the public view of one project's automation policy. A project
// without a stored row reports the defaults with an empty actor and updatedAt 0.
type Policy struct {
	ProjectID           string `json:"projectId"`
	RequireReview       bool   `json:"requireReview"`
	RequiredScanVerdict string `json:"requiredScanVerdict"`
	UpdatedBy           string `json:"updatedBy"`
	// UpdatedByName resolves the stored actor id to a username for display; it
	// is empty when the actor row no longer exists or no policy row is stored.
	UpdatedByName string `json:"updatedByName,omitempty"`
	UpdatedAt     int64  `json:"updatedAt"`
}

// Input is the PUT body. Both fields are mandatory there: the HTTP layer rejects
// a missing either field, so an explicit false/empty can never be confused with
// an omitted value.
type Input struct {
	RequireReview       bool
	RequiredScanVerdict string
}

// Service reads and writes policies against PostgreSQL. The project registry
// supplies the write authorization overlay (admin or project maintainer), which
// is the same decision rule the review center uses: being allowed to edit a
// public project does not imply being allowed to change its gates.
type Service struct {
	db       *sql.DB
	projects *projects.Service
}

// New builds the policy service. A nil registry keeps every write forbidden
// (fail closed) while reads stay available.
func New(db *sql.DB, registry *projects.Service) *Service {
	return &Service{db: db, projects: registry}
}

// defaultPolicy is the gate-off policy used when no row is stored.
func defaultPolicy(projectID string) Policy {
	return Policy{ProjectID: projectID, RequiredScanVerdict: RequiredScanVerdictNone}
}

// validateProjectID rejects a malformed id before any query.
func validateProjectID(projectID string) error {
	if !auth.ValidID(projectID) {
		return fmt.Errorf("%w: invalid project id", ErrValidation)
	}
	return nil
}

// validateInput checks the enum; the boolean needs no validation.
func validateInput(input Input) error {
	switch input.RequiredScanVerdict {
	case RequiredScanVerdictNone, RequiredScanVerdictPassed:
		return nil
	default:
		return fmt.Errorf("%w: requiredScanVerdict must be none or passed", ErrValidation)
	}
}

// authorizeVisible applies the single project-visibility predicate: an invisible
// project is reported as not found so policy reads cannot probe private project
// ids. Visibility is the caller's responsibility before authorizeGate; keeping
// it a separate step makes an invisible project a 404 instead of a 403.
func authorizeVisible(ctx context.Context, q projects.RowQuerier, actor auth.Actor, projectID string) error {
	isAdmin, userID := projects.VisibilityArgs(actor)
	var found int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM projects p WHERE p.id=$1 AND `+projects.VisiblePredicate("p", 2, 3),
		projectID, isAdmin, userID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// globalAdminRole mirrors the role matrix: the global role is the ceiling for
// every project-level decision.
const globalAdminRole = "admin"

// authorizeGate applies the admin-or-maintainer rule that governs gate changes.
// The project registry reports the same ErrNotFound/ErrForbidden vocabulary, so
// the mapping stays local and the HTTP layer sees one error set.
func (s *Service) authorizeGate(ctx context.Context, q projects.RowQuerier, actor auth.Actor, projectID string) error {
	if s.projects == nil {
		// Fail closed: a service built without the registry keeps policy writes
		// admin-only rather than silently allowing every member.
		if actor.Role == globalAdminRole {
			return nil
		}
		return fmt.Errorf("%w: only a project maintainer or a global admin can update the automation policy", ErrForbidden)
	}
	switch err := s.projects.AuthorizeDecide(ctx, q, actor, projectID); {
	case err == nil:
		return nil
	case errors.Is(err, projects.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, projects.ErrForbidden):
		return fmt.Errorf("%w: only a project maintainer or a global admin can update the automation policy", ErrForbidden)
	default:
		return err
	}
}

// writeAudit records the policy write inside the caller's transaction so the
// stored policy and its audit row commit together. The detail is deliberately
// limited to the project id and the two gate values: the policy has no other
// content, and no token, credential or request body is ever passed in.
func writeAudit(ctx context.Context, tx *sql.Tx, actor auth.Actor, projectID string, input Input) error {
	id, err := newID()
	if err != nil {
		return err
	}
	detail := fmt.Sprintf("projectId=%s requireReview=%t requiredScanVerdict=%s",
		projectID, input.RequireReview, input.RequiredScanVerdict)
	_, err = tx.ExecContext(ctx, `INSERT INTO auth_audit(id,created_at,actor_id,actor_username,action,object_type,object_id,result,detail,request_id)
	 VALUES($1,$2,$3,$4,$5,'policy',$6,'success',$7,$8)`,
		id, time.Now().Unix(), actor.ID, actor.Username, ActionUpdate, projectID, detail, actor.RequestID)
	return err
}

// newID builds an audit row id (18 random bytes, the same shape every module uses).
func newID() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Get returns the policy of a project the actor can see. A stored row is
// resolved with its updater username; no row reports the documented defaults
// with updatedBy empty and updatedAt 0.
func (s *Service) Get(ctx context.Context, actor auth.Actor, projectID string) (Policy, error) {
	if err := validateProjectID(projectID); err != nil {
		return Policy{}, err
	}
	if err := authorizeVisible(ctx, s.db, actor, projectID); err != nil {
		return Policy{}, err
	}
	return readPolicy(ctx, s.db, projectID)
}

// readPolicy loads one policy row (or the defaults) from a DB handle or a
// transaction.
func readPolicy(ctx context.Context, q projects.RowQuerier, projectID string) (Policy, error) {
	policy := defaultPolicy(projectID)
	var updatedByName sql.NullString
	err := q.QueryRowContext(ctx, `SELECT pp.require_review,pp.required_scan_verdict,pp.updated_by,u.username,pp.updated_at
	 FROM project_policies pp LEFT JOIN auth_users u ON u.id=pp.updated_by
	 WHERE pp.project_id=$1`, projectID).Scan(
		&policy.RequireReview, &policy.RequiredScanVerdict, &policy.UpdatedBy, &updatedByName, &policy.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return policy, nil
	}
	if err != nil {
		return Policy{}, err
	}
	policy.UpdatedByName = updatedByName.String
	return policy, nil
}

// Upsert validates and stores one project's policy, authorizing the caller first
// (visibility, then admin or maintainer). The upsert and its policy.update audit
// row commit in the same transaction; the stored row is returned with the
// updater's username so the client can render the last change.
func (s *Service) Upsert(ctx context.Context, actor auth.Actor, projectID string, input Input) (Policy, error) {
	if err := validateProjectID(projectID); err != nil {
		return Policy{}, err
	}
	if err := validateInput(input); err != nil {
		return Policy{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Policy{}, err
	}
	defer tx.Rollback()
	if err = authorizeVisible(ctx, tx, actor, projectID); err != nil {
		return Policy{}, err
	}
	if err = s.authorizeGate(ctx, tx, actor, projectID); err != nil {
		return Policy{}, err
	}
	now := time.Now().Unix()
	if _, err = tx.ExecContext(ctx, `INSERT INTO project_policies(project_id,require_review,required_scan_verdict,updated_by,updated_at)
	 VALUES($1,$2,$3,$4,$5)
	 ON CONFLICT (project_id) DO UPDATE SET require_review=EXCLUDED.require_review,
	 required_scan_verdict=EXCLUDED.required_scan_verdict, updated_by=EXCLUDED.updated_by, updated_at=EXCLUDED.updated_at`,
		projectID, input.RequireReview, input.RequiredScanVerdict, actor.ID, now); err != nil {
		return Policy{}, err
	}
	if err = writeAudit(ctx, tx, actor, projectID, input); err != nil {
		return Policy{}, err
	}
	if err = tx.Commit(); err != nil {
		return Policy{}, err
	}
	return Policy{
		ProjectID:           projectID,
		RequireReview:       input.RequireReview,
		RequiredScanVerdict: input.RequiredScanVerdict,
		UpdatedBy:           actor.ID,
		UpdatedByName:       actor.Username,
		UpdatedAt:           now,
	}, nil
}
