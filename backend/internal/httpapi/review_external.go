package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yichenfchai/AegisFlow/backend/internal/reviews"
)

// Inbound external-result webhook (P5-A E4). The endpoint is machine-to-machine
// and deliberately outside the Bearer session: the caller proves possession of
// AEGIS_REVIEW_WEBHOOK_SECRET by signing the exact raw body with the delivery
// timestamp. The signing/replay/idempotency contract and its helpers are shared
// with the scan and defense-event ingest endpoints (see ingest.go).

// ingestReviewExternalResult accepts one signed external review result. It never
// changes the review state machine; it only appends evidence. A retried
// delivery returns the stored row with 200, a new one returns 201, and an
// unconfigured secret keeps the whole endpoint closed with 503.
func (s *Server) ingestReviewExternalResult(c *gin.Context) {
	if s.reviews == nil || len(s.reviewWebhookSecret) == 0 {
		s.fail(c, 503, "dependencies_unavailable", "review results are not configured")
		return
	}
	raw, ok := s.readSignedBody(c)
	if !ok {
		return
	}
	delivery, ok := verifyIngestSignature(c, s.reviewWebhookSecret, raw, time.Now())
	if !ok {
		return
	}
	var input struct {
		Source  string `json:"source"`
		Verdict string `json:"verdict"`
		Summary string `json:"summary"`
		URL     string `json:"url"`
	}
	if !s.decodeRawJSON(c, raw, &input) {
		return
	}
	result, created, err := s.reviews.IngestExternalResult(c.Request.Context(), c.Param("id"), delivery, reviews.ExternalResultInput{
		Source: input.Source, Verdict: input.Verdict, Summary: input.Summary, URL: input.URL,
	})
	if err != nil {
		s.reviewExternalResultError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.respond(c, status, result)
}

// reviewExternalResultError maps external-result errors to the shared envelope.
// The validation message describes the external-result contract rather than the
// review-create fields, so an integrating tool learns what to fix.
func (s *Server) reviewExternalResultError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, reviews.ErrValidation):
		s.fail(c, 400, "validation_failed", "source 1-64 bytes on one line, verdict passed/failed/warning, summary at most 500 bytes, url an http(s) URL without userinfo or fragment, and deliveryId 8-128 characters of A-Z a-z 0-9 . _ -")
	case errors.Is(err, reviews.ErrConflict):
		s.fail(c, 409, "conflict", "the external result delivery was removed concurrently; retry with a new deliveryId")
	case errors.Is(err, reviews.ErrNotFound):
		s.fail(c, 404, "not_found", "resource not found")
	default:
		s.logger.ErrorContext(c.Request.Context(), "review external result failed", "requestId", requestID(c), "error", err)
		s.fail(c, 500, "internal_error", "internal server error")
	}
}

// listReviewExternalResults returns the recent evidence of a visible review,
// newest first and capped at 50 rows. An invisible project is a 404, like every
// other review read.
func (s *Server) listReviewExternalResults(c *gin.Context) {
	if s.reviews == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	list, err := s.reviews.ListExternalResults(c.Request.Context(), actorFrom(c), c.Param("id"))
	if err != nil {
		s.reviewError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": list})
}
