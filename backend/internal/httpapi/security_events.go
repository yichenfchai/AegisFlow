package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yichenfchai/AegisFlow/backend/internal/securityevents"
)

// Defense events (P5 remaining modules, batch A). The write side is a signed
// machine-to-machine webhook (no Bearer session); reading and acknowledging are
// admin-only. No detection engine runs inside AegisFlow: an external tool
// reports an event, and an administrator acknowledges it with an audit row in
// the same transaction.

// ingestSecurityEvent accepts one signed defense-event delivery. A retried
// delivery returns the stored row with 200, a new one returns 201, and an
// unconfigured secret keeps the whole endpoint closed with 503.
func (s *Server) ingestSecurityEvent(c *gin.Context) {
	if s.securityEvents == nil || len(s.ingestWebhookSecret) == 0 {
		s.fail(c, 503, "dependencies_unavailable", "ingest is not configured")
		return
	}
	raw, ok := s.readSignedBody(c)
	if !ok {
		return
	}
	delivery, ok := verifyIngestSignature(c, s.ingestWebhookSecret, raw, time.Now())
	if !ok {
		return
	}
	var input struct {
		Source     string `json:"source"`
		Severity   string `json:"severity"`
		EventType  string `json:"eventType"`
		Summary    string `json:"summary"`
		OccurredAt int64  `json:"occurredAt"`
		DetailsURL string `json:"detailsUrl"`
	}
	if !s.decodeRawJSON(c, raw, &input) {
		return
	}
	event, created, err := s.securityEvents.Ingest(c.Request.Context(), delivery, securityevents.IngestInput{
		Source: input.Source, Severity: input.Severity, EventType: input.EventType, Summary: input.Summary,
		OccurredAt: input.OccurredAt, DetailsURL: input.DetailsURL,
	})
	if err != nil {
		s.securityEventError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.respond(c, status, event)
}

// securityEventError maps defense-event errors to the shared envelope. The
// validation message describes the event contract, and the admin-only routes
// still answer the documented 403 envelope through requireAdmin.
func (s *Server) securityEventError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, securityevents.ErrValidation):
		s.fail(c, 400, "validation_failed", "source 1-64 bytes on one line, severity low/medium/high/critical, eventType 1-64 bytes on one line, summary 1-1000 bytes, occurredAt a Unix timestamp not more than 300 seconds in the future, detailsUrl an http(s) URL without userinfo or fragment, and deliveryId 8-128 characters of A-Z a-z 0-9 . _ -")
	case errors.Is(err, securityevents.ErrForbidden):
		s.fail(c, 403, "forbidden", "defense events are admin-only")
	case errors.Is(err, securityevents.ErrConflict):
		s.fail(c, 409, "conflict", "the security event delivery was removed concurrently; retry with a new deliveryId")
	case errors.Is(err, securityevents.ErrNotFound):
		s.fail(c, 404, "not_found", "resource not found")
	default:
		s.logger.ErrorContext(c.Request.Context(), "security event request failed", "requestId", requestID(c), "error", err)
		s.fail(c, 500, "internal_error", "internal server error")
	}
}

// listSecurityEvents pages defense events, newest occurrence first, with
// optional severity/status/source filters. Admin-only.
func (s *Server) listSecurityEvents(c *gin.Context) {
	if s.securityEvents == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	limit, offset, ok := s.page(c)
	if !ok {
		return
	}
	list, total, err := s.securityEvents.List(c.Request.Context(), actorFrom(c), securityevents.ListFilter{
		Severity: c.Query("severity"), Status: c.Query("status"), Source: c.Query("source"), Limit: limit, Offset: offset,
	})
	if err != nil {
		s.securityEventError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": list, "total": total, "limit": limit, "offset": offset})
}

// getSecurityEvent returns one defense event. Admin-only; an unknown event is a
// 404.
func (s *Server) getSecurityEvent(c *gin.Context) {
	if s.securityEvents == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	event, err := s.securityEvents.Get(c.Request.Context(), actorFrom(c), c.Param("id"))
	if err != nil {
		s.securityEventError(c, err)
		return
	}
	s.respond(c, 200, event)
}

// ackSecurityEvent moves an open event to acknowledged. A repeated
// acknowledgement is idempotent (200, the stored event, no second audit row).
// Admin-only.
func (s *Server) ackSecurityEvent(c *gin.Context) {
	if s.securityEvents == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	if !s.decode(c, &struct{}{}) {
		return
	}
	event, err := s.securityEvents.Ack(c.Request.Context(), actorFrom(c), c.Param("id"))
	if err != nil {
		s.securityEventError(c, err)
		return
	}
	s.respond(c, 200, event)
}
