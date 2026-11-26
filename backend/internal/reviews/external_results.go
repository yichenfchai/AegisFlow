package reviews

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
)

// External review results (P5-A E4). A signed inbound webhook appends one
// immutable result row to a review; it never changes the review state machine,
// so an external scanner cannot approve or reject a change by itself. The
// caller is authenticated by the HMAC signature in the HTTP layer; this package
// only validates the payload and enforces delivery-id idempotency.

// Verdicts accepted from an external review source.
const (
	VerdictPassed  = "passed"
	VerdictFailed  = "failed"
	VerdictWarning = "warning"
)

const (
	minSourceBytes = 1
	maxSourceBytes = 64
	// maxSummaryBytes bounds the free-text result note; it may be multi-line.
	maxSummaryBytes = 500
	// maxExternalResults is the read cap: the review detail page shows the most
	// recent evidence and the API never paginates beyond it.
	maxExternalResults = 50
)

// ExternalResult is one immutable result delivered by an external review tool.
// Summary may contain newlines; URL is empty when the delivery carried none.
type ExternalResult struct {
	ID         string `json:"id"`
	ReviewID   string `json:"reviewId"`
	DeliveryID string `json:"deliveryId"`
	Source     string `json:"source"`
	Verdict    string `json:"verdict"`
	Summary    string `json:"summary"`
	URL        string `json:"url"`
	ReceivedAt int64  `json:"receivedAt"`
}

// ExternalResultInput is the validated inbound payload. DeliveryID and
// ReceivedAt are supplied by the transport/database, not by the JSON body.
type ExternalResultInput struct {
	Source  string
	Verdict string
	Summary string
	URL     string
}

const externalResultColumns = `id,review_id,delivery_id,source,verdict,summary,url,received_at`

// scanExternalResult scans externalResultColumns; a NULL url is the stored
// representation of "no link".
func scanExternalResult(row rowScanner) (*ExternalResult, error) {
	var result ExternalResult
	var url sql.NullString
	if err := row.Scan(&result.ID, &result.ReviewID, &result.DeliveryID, &result.Source, &result.Verdict,
		&result.Summary, &url, &result.ReceivedAt); err != nil {
		return nil, err
	}
	result.URL = url.String
	return &result, nil
}

// validateDeliveryID enforces the delivery-id shape the HTTP layer documents:
// 8-128 characters from [A-Za-z0-9._-]. It is the idempotency key, so a
// malformed value is a validation error rather than a silently dropped result.
func validateDeliveryID(deliveryID string) error {
	if len(deliveryID) < 8 || len(deliveryID) > 128 {
		return fmt.Errorf("%w: deliveryId must be 8-128 characters", ErrValidation)
	}
	for i := 0; i < len(deliveryID); i++ {
		c := deliveryID[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return fmt.Errorf("%w: deliveryId may contain only letters, digits, dot, underscore or hyphen", ErrValidation)
		}
	}
	return nil
}

// normalizeExternalResult validates the inbound payload and returns the stored
// values. source is trimmed and bounded to one line; summary may be multi-line
// but every other control character is removed (CR is normalized to LF); url is
// an optional display-only http(s) link that the server never fetches.
func normalizeExternalResult(input ExternalResultInput) (ExternalResultInput, error) {
	source := strings.TrimSpace(input.Source)
	if !utf8.ValidString(source) || len(source) < minSourceBytes || len(source) > maxSourceBytes {
		return ExternalResultInput{}, fmt.Errorf("%w: source must be 1-64 UTF-8 bytes", ErrValidation)
	}
	if !singleLine(source) {
		return ExternalResultInput{}, fmt.Errorf("%w: source must be a single line without control characters", ErrValidation)
	}
	switch input.Verdict {
	case VerdictPassed, VerdictFailed, VerdictWarning:
	default:
		return ExternalResultInput{}, fmt.Errorf("%w: verdict must be passed, failed or warning", ErrValidation)
	}
	summary, err := sanitizeSummary(input.Summary)
	if err != nil {
		return ExternalResultInput{}, err
	}
	externalURL := strings.TrimSpace(input.URL)
	if err = validateDisplayURL("url", externalURL); err != nil {
		return ExternalResultInput{}, err
	}
	return ExternalResultInput{Source: source, Verdict: input.Verdict, Summary: summary, URL: externalURL}, nil
}

// sanitizeSummary keeps human-readable text and newlines: it normalizes CR/CRLF
// to LF, drops the remaining control characters (NUL, ESC, DEL, ...), trims
// surrounding whitespace and bounds the result to maxSummaryBytes.
func sanitizeSummary(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("%w: summary must be valid UTF-8 of at most %d bytes", ErrValidation, maxSummaryBytes)
	}
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	var builder strings.Builder
	builder.Grow(len(value))
	for _, r := range value {
		if r == '\n' {
			builder.WriteByte('\n')
			continue
		}
		if unicode.IsControl(r) {
			// Drop non-text control characters instead of storing them.
			continue
		}
		builder.WriteRune(r)
	}
	summary := strings.TrimSpace(builder.String())
	if len(summary) > maxSummaryBytes {
		return "", fmt.Errorf("%w: summary must be at most %d bytes", ErrValidation, maxSummaryBytes)
	}
	return summary, nil
}

// IngestExternalResult appends one signed external result to a review. The
// delivery id is the idempotency key: a repeated delivery returns the stored
// row and created=false, so a retried webhook never duplicates evidence even
// across replicas. A missing review is ErrNotFound; everything else that fails
// payload validation is ErrValidation. The review state machine is untouched.
func (s *Service) IngestExternalResult(ctx context.Context, reviewID, deliveryID string, input ExternalResultInput) (*ExternalResult, bool, error) {
	if !auth.ValidID(reviewID) {
		return nil, false, fmt.Errorf("%w: invalid review id", ErrValidation)
	}
	if err := validateDeliveryID(deliveryID); err != nil {
		return nil, false, err
	}
	normalized, err := normalizeExternalResult(input)
	if err != nil {
		return nil, false, err
	}
	var found int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM reviews WHERE id=$1`, reviewID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	id, err := newID()
	if err != nil {
		return nil, false, err
	}
	receivedAt := time.Now().Unix()
	result, err := scanExternalResult(s.db.QueryRowContext(ctx, `INSERT INTO review_external_results
		(id,review_id,delivery_id,source,verdict,summary,url,received_at)
		VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8)
		ON CONFLICT (delivery_id) DO NOTHING
		RETURNING `+externalResultColumns,
		id, reviewID, deliveryID, normalized.Source, normalized.Verdict, normalized.Summary, normalized.URL, receivedAt))
	switch {
	case err == nil:
		return result, true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, false, err
	}
	// The delivery id already exists: return the stored row unchanged (200), so
	// the sender's retry is idempotent. The stored review id is authoritative;
	// it is the row the original delivery created.
	stored, err := scanExternalResult(s.db.QueryRowContext(ctx,
		`SELECT `+externalResultColumns+` FROM review_external_results WHERE delivery_id=$1`, deliveryID))
	if errors.Is(err, sql.ErrNoRows) {
		// The conflicting row disappeared between INSERT and SELECT (review or
		// project deletion cascaded); surface a conflict instead of a stale id.
		return nil, false, fmt.Errorf("%w: delivery %q was removed concurrently", ErrConflict, deliveryID)
	}
	if err != nil {
		return nil, false, err
	}
	return stored, false, nil
}

// ListExternalResults returns the most recent evidence appended to a visible
// review, newest first and capped at maxExternalResults. Review visibility is
// the project visibility predicate: an invisible project is ErrNotFound, so
// private review ids stay non-enumerable.
func (s *Service) ListExternalResults(ctx context.Context, actor auth.Actor, reviewID string) ([]ExternalResult, error) {
	if !auth.ValidID(reviewID) {
		return nil, fmt.Errorf("%w: invalid review id", ErrValidation)
	}
	var projectID string
	err := s.db.QueryRowContext(ctx, `SELECT project_id FROM reviews WHERE id=$1`, reviewID).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := visibleProject(ctx, s.db, actor, projectID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+externalResultColumns+` FROM review_external_results
		WHERE review_id=$1 ORDER BY received_at DESC,id DESC LIMIT $2`, reviewID, maxExternalResults)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := make([]ExternalResult, 0)
	for rows.Next() {
		result, scanErr := scanExternalResult(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		list = append(list, *result)
	}
	return list, rows.Err()
}
