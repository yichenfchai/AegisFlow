package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yichenfchai/AegisFlow/backend/internal/reviews"
)

// Inbound external-result webhook (P5-A E4). The endpoint is machine-to-machine
// and deliberately outside the Bearer session: the caller proves possession of
// AEGIS_REVIEW_WEBHOOK_SECRET by signing the exact raw body with the delivery
// timestamp. Signing the timestamp as a prefix gives replay protection (a
// captured body can only be replayed within the five-minute window) while the
// delivery id makes legitimate retries idempotent.
const (
	reviewResultTimestampHeader = "X-AegisFlow-Timestamp"
	reviewResultSignatureHeader = "X-AegisFlow-Signature"
	reviewResultDeliveryHeader  = "X-AegisFlow-Delivery"
	// reviewResultTimestampSkew is the accepted clock difference between the
	// sender's timestamp and the server; outside it the request is rejected.
	reviewResultTimestampSkew = 300 * time.Second
)

// reviewDeliveryPattern is the documented delivery-id shape: 8-128 characters
// of [A-Za-z0-9._-]. It is the receiver-side idempotency key.
var reviewDeliveryPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{8,128}$`)

// ingestReviewExternalResult accepts one signed external review result. It never
// changes the review state machine; it only appends evidence. A retried
// delivery returns the stored row with 200, a new one returns 201, and an
// unconfigured secret keeps the whole endpoint closed with 503.
func (s *Server) ingestReviewExternalResult(c *gin.Context) {
	if s.reviews == nil || len(s.reviewWebhookSecret) == 0 {
		s.fail(c, 503, "dependencies_unavailable", "review results are not configured")
		return
	}
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "application/json" {
		s.fail(c, 415, "unsupported_media_type", "Content-Type must be application/json")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		s.fail(c, 400, "invalid_request", "body must contain one JSON object of at most 16 KiB")
		return
	}
	delivery, ok := verifyReviewResultSignature(c, s.reviewWebhookSecret, raw, time.Now())
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

// verifyReviewResultSignature checks the timestamp window, the delivery id shape
// and the HMAC-SHA256 signature over "timestamp.rawBody". Every failure is an
// authentication failure (401) with the same message, so a caller cannot use
// the response to probe which header was wrong. On success it returns the
// delivery id for the idempotent insert.
func verifyReviewResultSignature(c *gin.Context, secret, raw []byte, now time.Time) (string, bool) {
	timestampRaw := c.GetHeader(reviewResultTimestampHeader)
	signatureRaw := c.GetHeader(reviewResultSignatureHeader)
	delivery := c.GetHeader(reviewResultDeliveryHeader)
	fail := func() (string, bool) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{
				"code":    "unauthorized",
				"message": "missing, invalid or expired X-AegisFlow signature headers",
			},
			"requestId": c.Request.Context().Value(requestIDKey),
		})
		return "", false
	}
	if timestampRaw == "" || signatureRaw == "" || delivery == "" {
		return fail()
	}
	timestamp, err := strconv.ParseInt(timestampRaw, 10, 64)
	if err != nil {
		return fail()
	}
	skew := now.Unix() - timestamp
	if skew < 0 {
		skew = -skew
	}
	if skew > int64(reviewResultTimestampSkew/time.Second) {
		return fail()
	}
	if !reviewDeliveryPattern.MatchString(delivery) {
		return fail()
	}
	if !strings.HasPrefix(signatureRaw, "sha256=") {
		return fail()
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(signatureRaw, "sha256="))
	if err != nil || len(provided) != sha256.Size {
		return fail()
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestampRaw))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(raw)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fail()
	}
	return delivery, true
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
