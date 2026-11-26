package httpapi

import (
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/yichenfchai/AegisFlow/backend/internal/policies"
)

// policyError maps automation-policy failures to the shared error envelope. The
// messages describe the contract (enum values and who may write) without echoing
// any request content.
func (s *Server) policyError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, policies.ErrValidation):
		s.fail(c, 400, "validation_failed", "requiredScanVerdict must be none or passed, and the project id must be valid")
	case errors.Is(err, policies.ErrForbidden):
		s.fail(c, 403, "forbidden", "only a project maintainer or a global admin can update the automation policy")
	case errors.Is(err, policies.ErrNotFound):
		s.fail(c, 404, "not_found", "resource not found")
	default:
		s.logger.ErrorContext(c.Request.Context(), "policy request failed", "requestId", requestID(c), "error", err)
		s.fail(c, 500, "internal_error", "internal server error")
	}
}

// getProjectPolicy returns one visible project's automation policy; a project
// without a stored row reports the defaults with updatedAt 0. Every role that
// can see the project may read it, because the policy explains why a deployment
// was gated.
func (s *Server) getProjectPolicy(c *gin.Context) {
	if s.policies == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	policy, err := s.policies.Get(c.Request.Context(), actorFrom(c), c.Param("id"))
	if err != nil {
		s.policyError(c, err)
		return
	}
	s.respond(c, 200, policy)
}

// putProjectPolicy replaces one project's automation policy. Both body fields are
// mandatory: an omitted requireReview could otherwise be mistaken for an explicit
// false. The service authorizes the caller (admin or project maintainer) and
// audits policy.update in the same transaction.
func (s *Server) putProjectPolicy(c *gin.Context) {
	if s.policies == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	var input struct {
		RequireReview       *bool   `json:"requireReview"`
		RequiredScanVerdict *string `json:"requiredScanVerdict"`
	}
	if !s.decode(c, &input) {
		return
	}
	if input.RequireReview == nil || input.RequiredScanVerdict == nil {
		s.fail(c, 400, "invalid_request", "requireReview and requiredScanVerdict are required")
		return
	}
	policy, err := s.policies.Upsert(c.Request.Context(), actorFrom(c), c.Param("id"), policies.Input{
		RequireReview:       *input.RequireReview,
		RequiredScanVerdict: *input.RequiredScanVerdict,
	})
	if err != nil {
		s.policyError(c, err)
		return
	}
	s.respond(c, 200, policy)
}
