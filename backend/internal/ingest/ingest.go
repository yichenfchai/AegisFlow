// Package ingest holds the shared payload rules of the signed machine-to-machine
// webhooks (external review results, scan results and security defense events).
// HMAC verification and replay protection stay in the HTTP layer; this package
// only validates and normalizes a payload after authentication, so every
// inbound contract applies the same delivery-id, display-URL, single-line and
// summary rules. It depends on nothing but the standard library.
package ingest

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MinDeliveryBytes/MaxDeliveryBytes bound the sender idempotency key
	// X-AegisFlow-Delivery, which is also the database uniqueness key.
	MinDeliveryBytes = 8
	MaxDeliveryBytes = 128
	// MaxURLBytes bounds an optional display-only http(s) link (reportUrl /
	// detailsUrl). The server stores and renders it but never fetches it.
	MaxURLBytes = 2048
)

// ErrInvalid is the single payload-validation sentinel of the signed ingest
// contracts. The business packages alias it as their own ErrValidation so the
// HTTP layer maps every payload failure to one 400 without inspecting wording.
var ErrInvalid = errors.New("invalid ingest payload")

// ValidDeliveryID enforces the documented delivery-id shape: 8-128 characters
// from [A-Za-z0-9._-]. It is the idempotency key, so a malformed value is a
// validation error rather than a silently dropped delivery.
func ValidDeliveryID(deliveryID string) error {
	if len(deliveryID) < MinDeliveryBytes || len(deliveryID) > MaxDeliveryBytes {
		return fmt.Errorf("%w: deliveryId must be 8-128 characters", ErrInvalid)
	}
	for i := 0; i < len(deliveryID); i++ {
		c := deliveryID[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return fmt.Errorf("%w: deliveryId may contain only letters, digits, dot, underscore or hyphen", ErrInvalid)
		}
	}
	return nil
}

// SingleLine reports whether value contains no control characters (which
// includes CR/LF and tabs): the stored field is a single display line.
func SingleLine(value string) bool {
	return !strings.ContainsFunc(value, unicode.IsControl)
}

// SanitizeText keeps human-readable text and newlines: it normalizes CR/CRLF to
// LF, drops the remaining control characters (NUL, ESC, DEL, ...), trims
// surrounding whitespace and bounds the result to maxBytes. It backs the
// free-text summary of every ingest payload, so a newline is the only control
// character a sender can store.
func SanitizeText(value string, maxBytes int) (string, error) {
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("%w: text must be valid UTF-8 of at most %d bytes", ErrInvalid, maxBytes)
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
	text := strings.TrimSpace(builder.String())
	if len(text) > maxBytes {
		return "", fmt.Errorf("%w: text must be at most %d bytes", ErrInvalid, maxBytes)
	}
	return text, nil
}

// ValidateURL accepts an optional display-only link over http(s) and rejects
// credentials in the URL (they would be shown to every reader) and fragments
// (they do not select a server resource). Queries stay allowed: external tools
// frequently link to a filtered report view. field only names the rejected
// input in the error.
func ValidateURL(field, value string) error {
	if value == "" {
		return nil
	}
	if !utf8.ValidString(value) || len(value) > MaxURLBytes {
		return fmt.Errorf("%w: %s must be valid UTF-8 of at most %d bytes", ErrInvalid, field, MaxURLBytes)
	}
	if strings.ContainsFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return fmt.Errorf("%w: %s must not contain whitespace", ErrInvalid, field)
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return fmt.Errorf("%w: %s must be an absolute http or https URL with a host", ErrInvalid, field)
	}
	if parsed.User != nil {
		return fmt.Errorf("%w: %s must not contain userinfo", ErrInvalid, field)
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" {
		return fmt.Errorf("%w: %s must not contain a fragment", ErrInvalid, field)
	}
	return nil
}

// BoundedSingleLine trims a required or optional single-line field and enforces
// its byte bounds without control characters. emptyIsValid permits an omitted
// optional field (tool/version); a present value still has to fit the bound.
func BoundedSingleLine(field, value string, maxBytes int, emptyIsValid bool) (string, error) {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) || len(value) > maxBytes {
		return "", fmt.Errorf("%w: %s must be valid UTF-8 of at most %d bytes", ErrInvalid, field, maxBytes)
	}
	if value == "" && !emptyIsValid {
		return "", fmt.Errorf("%w: %s must be 1-%d UTF-8 bytes", ErrInvalid, field, maxBytes)
	}
	if !SingleLine(value) {
		return "", fmt.Errorf("%w: %s must be a single line without control characters", ErrInvalid, field)
	}
	return value, nil
}
