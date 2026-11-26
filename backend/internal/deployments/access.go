package deployments

import (
	"context"
	"database/sql"
	"errors"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
)

// deploymentColumnsQualified is deploymentColumns with the alias used by the
// visibility join; the unqualified list becomes ambiguous once projects joins.
const deploymentColumnsQualified = `d.id,d.project_id,d.target_id,d.target_name,d.target_endpoint,d.target_environment,d.deployment_number,d.status,d.version,d.build_id,d.rollback_of,d.approval_id,d.review_id,d.triggered_by,d.detail,d.created_at,d.started_at,d.finished_at`

// globalAdminRole mirrors the role matrix in the auth contract: the global role
// is the ceiling for every project-level decision.
const globalAdminRole = "admin"

// authorizeProjectWrite applies the project write overlay to a deployment
// action. Visibility is checked first, so an invisible project stays a 404 and
// deployment ids cannot be used to probe projects; a visible project the caller
// may not write is a 403 (a viewer, or a member on a private project it does
// not maintain). An admin always writes.
func (s *Service) authorizeProjectWrite(ctx context.Context, q projects.RowQuerier, actor auth.Actor, projectID string) error {
	if actor.Role == globalAdminRole {
		return nil
	}
	if s.projects == nil {
		// Fail closed: a service built without the registry keeps deployments
		// admin-only rather than silently allowing every member.
		return ErrForbidden
	}
	isAdmin, userID := projects.VisibilityArgs(actor)
	var found int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM projects p WHERE p.id=$1 AND `+projects.VisiblePredicate("p", 2, 3),
		projectID, isAdmin, userID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	switch err := s.projects.AuthorizeWrite(ctx, q, actor, projectID); {
	case err == nil:
		return nil
	case errors.Is(err, projects.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, projects.ErrForbidden):
		return ErrForbidden
	default:
		return err
	}
}
