package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yichenfchai/AegisFlow/backend/internal/scans"
)

// Scan registry (P5 remaining modules, batch A). The write side is a signed
// machine-to-machine webhook (no Bearer session); the read side follows project
// visibility exactly like builds, deployments and reviews. No scanner runs
// inside AegisFlow: an external tool reports its verdict and AegisFlow stores,
// audits and displays the evidence.

// ingestScan accepts one signed scan delivery for an existing project. A retried
// delivery returns the stored row with 200, a new one returns 201, and an
// unconfigured secret keeps the whole endpoint closed with 503.
func (s *Server) ingestScan(c *gin.Context) {
	if s.scans == nil || len(s.ingestWebhookSecret) == 0 {
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
		Source    string `json:"source"`
		Tool      string `json:"tool"`
		Version   string `json:"version"`
		Verdict   string `json:"verdict"`
		Summary   string `json:"summary"`
		ReportURL string `json:"reportUrl"`
	}
	if !s.decodeRawJSON(c, raw, &input) {
		return
	}
	scan, created, err := s.scans.Ingest(c.Request.Context(), c.Param("id"), delivery, scans.Input{
		Source: input.Source, Tool: input.Tool, Version: input.Version, Verdict: input.Verdict,
		Summary: input.Summary, ReportURL: input.ReportURL,
	})
	if err != nil {
		s.scanError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.respond(c, status, scan)
}

// scanError maps scan-registry errors to the shared envelope. The validation
// message describes the scan contract rather than an implementation detail, so
// an integrating scanner learns what to fix.
func (s *Server) scanError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, scans.ErrValidation):
		s.fail(c, 400, "validation_failed", "source 1-64 bytes on one line, tool/version at most 64 bytes on one line, verdict passed/failed/warning, summary at most 1000 bytes, reportUrl an http(s) URL without userinfo or fragment, and deliveryId 8-128 characters of A-Z a-z 0-9 . _ -")
	case errors.Is(err, scans.ErrConflict):
		s.fail(c, 409, "conflict", "the scan delivery was removed concurrently; retry with a new deliveryId")
	case errors.Is(err, scans.ErrNotFound):
		s.fail(c, 404, "not_found", "resource not found")
	default:
		s.logger.ErrorContext(c.Request.Context(), "scan request failed", "requestId", requestID(c), "error", err)
		s.fail(c, 500, "internal_error", "internal server error")
	}
}

// listProjectScans pages one visible project's scans, newest first; verdict is
// an optional filter and the server reports the page bounds back.
func (s *Server) listProjectScans(c *gin.Context) {
	if s.scans == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	limit, offset, ok := s.page(c)
	if !ok {
		return
	}
	list, total, err := s.scans.ListProject(c.Request.Context(), actorFrom(c), c.Param("id"), c.Query("verdict"), limit, offset)
	if err != nil {
		s.scanError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": list, "total": total, "limit": limit, "offset": offset})
}

// listScansGlobal pages every scan the caller may see, newest first, with
// optional verdict and projectId filters. It mirrors the global review list:
// the visibility predicate lives in the service, so an invisible project filter
// yields an empty page instead of an error and scan ids stay non-enumerable.
func (s *Server) listScansGlobal(c *gin.Context) {
	if s.scans == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	limit, offset, ok := s.page(c)
	if !ok {
		return
	}
	list, total, err := s.scans.ListVisible(c.Request.Context(), actorFrom(c), scans.ListFilter{
		ProjectID: c.Query("projectId"), Verdict: c.Query("verdict"), Limit: limit, Offset: offset,
	})
	if err != nil {
		s.scanError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": list, "total": total, "limit": limit, "offset": offset})
}

// getScan returns one scan of a visible project; an invisible or unknown scan
// is a 404, like every other project-scoped read.
func (s *Server) getScan(c *gin.Context) {
	if s.scans == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	scan, err := s.scans.Get(c.Request.Context(), actorFrom(c), c.Param("id"))
	if err != nil {
		s.scanError(c, err)
		return
	}
	s.respond(c, 200, scan)
}
