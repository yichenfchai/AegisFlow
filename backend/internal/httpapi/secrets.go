package httpapi

import (
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/yichenfchai/AegisFlow/backend/internal/secrets"
)

// Independent secret store (P5 remaining modules, batch B). Everything here is
// admin-only: list returns metadata only (never a value or ciphertext), create
// stores a sealed value, rotate replaces it and delete discards it. The value
// is write-only: no response, audit detail, log or error can contain it. When
// AEGIS_SECRET_KEY is unset, create/rotate answer 503 while list/delete keep
// working on existing metadata.

// secretError maps store errors to the shared envelope. ErrNotConfigured is a
// deliberate 503 with the documented "secrets are not configured" message: the
// endpoint exists but cannot accept a value without the encryption key.
func (s *Server) secretError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, secrets.ErrNotConfigured):
		s.fail(c, 503, "secrets_not_configured", "secrets are not configured")
	case errors.Is(err, secrets.ErrValidation):
		s.fail(c, 400, "validation_failed", "name 1-128 bytes on one line, value 1-4096 UTF-8 bytes, and the id must be valid")
	case errors.Is(err, secrets.ErrForbidden):
		s.fail(c, 403, "forbidden", "secrets are admin-only")
	case errors.Is(err, secrets.ErrConflict):
		s.fail(c, 409, "conflict", "secret name already exists")
	case errors.Is(err, secrets.ErrNotFound):
		s.fail(c, 404, "not_found", "resource not found")
	default:
		s.logger.ErrorContext(c.Request.Context(), "secret request failed", "requestId", requestID(c), "error", err)
		s.fail(c, 500, "internal_error", "internal server error")
	}
}

// listSecrets pages secret metadata, newest first. The response cannot contain
// the value or the ciphertext.
func (s *Server) listSecrets(c *gin.Context) {
	if s.secrets == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	limit, offset, ok := s.page(c)
	if !ok {
		return
	}
	list, total, err := s.secrets.List(c.Request.Context(), actorFrom(c), secrets.ListFilter{Limit: limit, Offset: offset})
	if err != nil {
		s.secretError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": list, "total": total, "limit": limit, "offset": offset})
}

// createSecret stores one named secret (201). Only metadata is returned; the
// value never leaves the request.
func (s *Server) createSecret(c *gin.Context) {
	if s.secrets == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	var input struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if !s.decode(c, &input) {
		return
	}
	meta, err := s.secrets.Create(c.Request.Context(), actorFrom(c), input.Name, input.Value)
	if err != nil {
		s.secretError(c, err)
		return
	}
	s.respond(c, 201, meta)
}

// rotateSecret replaces a secret's value and bumps its rotation counter. Only
// metadata is returned; the audit detail never contains the value.
func (s *Server) rotateSecret(c *gin.Context) {
	if s.secrets == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	var input struct {
		Value string `json:"value"`
	}
	if !s.decode(c, &input) {
		return
	}
	meta, err := s.secrets.Rotate(c.Request.Context(), actorFrom(c), c.Param("id"), input.Value)
	if err != nil {
		s.secretError(c, err)
		return
	}
	s.respond(c, 200, meta)
}

// deleteSecret removes one secret and its ciphertext; an unknown id is a 404.
func (s *Server) deleteSecret(c *gin.Context) {
	if s.secrets == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	if err := s.secrets.Delete(c.Request.Context(), actorFrom(c), c.Param("id")); err != nil {
		s.secretError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"deleted": true})
}
