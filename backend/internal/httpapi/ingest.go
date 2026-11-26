package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Shared signing contract of the authenticated machine-to-machine ingest
// webhooks (external review results, scan results, defense events). None of
// them is a Bearer session route: the caller proves possession of the dedicated
// secret by signing the exact raw body with the delivery timestamp. Signing the
// timestamp as a prefix gives replay protection (a captured body can only be
// replayed within the five-minute window) while the delivery id makes
// legitimate retries idempotent.
const (
	ingestTimestampHeader = "X-AegisFlow-Timestamp"
	ingestSignatureHeader = "X-AegisFlow-Signature"
	ingestDeliveryHeader  = "X-AegisFlow-Delivery"
	// ingestTimestampSkew is the accepted clock difference between the sender's
	// timestamp and the server; outside it the request is rejected.
	ingestTimestampSkew = 300 * time.Second
	// ingestMaxBodyBytes bounds one signed payload before the strict JSON decoder
	// applies the shared unknown/null/duplicate-document rules.
	ingestMaxBodyBytes = 16 << 10
)

// ingestDeliveryPattern is the documented delivery-id shape: 8-128 characters
// of [A-Za-z0-9._-]. It is the receiver-side idempotency key.
var ingestDeliveryPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{8,128}$`)

// readSignedBody enforces the shared content-type and 16 KiB limit of a signed
// ingest webhook and returns the raw bytes the signature must cover. It writes
// the error response itself and returns ok=false when the body is unusable.
func (s *Server) readSignedBody(c *gin.Context) ([]byte, bool) {
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "application/json" {
		s.fail(c, 415, "unsupported_media_type", "Content-Type must be application/json")
		return nil, false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, ingestMaxBodyBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		s.fail(c, 400, "invalid_request", "body must contain one JSON object of at most 16 KiB")
		return nil, false
	}
	return raw, true
}

// verifyIngestSignature checks the timestamp window, the delivery id shape and
// the HMAC-SHA256 signature over "timestamp.rawBody". Every failure is an
// authentication failure (401) with the same message, so a caller cannot use
// the response to probe which header was wrong. On success it returns the
// delivery id for the idempotent insert.
func verifyIngestSignature(c *gin.Context, secret, raw []byte, now time.Time) (string, bool) {
	timestampRaw := c.GetHeader(ingestTimestampHeader)
	signatureRaw := c.GetHeader(ingestSignatureHeader)
	delivery := c.GetHeader(ingestDeliveryHeader)
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
	if skew > int64(ingestTimestampSkew/time.Second) {
		return fail()
	}
	if !ingestDeliveryPattern.MatchString(delivery) {
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
