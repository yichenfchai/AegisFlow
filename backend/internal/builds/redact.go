package builds

import (
	"regexp"
	"strings"
)

// Build output is untrusted: a command can print a token it was given, a
// database URL it discovered, or a bearer header. Every stdout/stderr line is
// therefore sanitized before it reaches build_log_events, which is readable by
// every role that can see the project. System lines are redacted too: internal
// lifecycle messages embed command and clone error text, so they are not a
// trusted channel.
//
// The rules are deliberately narrow so ordinary output survives: each one
// requires a credential-shaped substring (an AEGIS_* assignment, an
// Authorization value, a URL with inline credentials, a well-known token prefix
// or a JWT). A line is dropped when a matched value survives redaction, which
// is the fail-closed policy from the execution design (D6). Multi-line PEM
// private keys are handled by the log writer's cross-line state machine (see
// logWriter.redactMessage), not by a single-line rule.
type redactionRule struct {
	pattern     *regexp.Regexp
	replacement string
}

var (
	// privateKeyBeginPattern / privateKeyEndPattern delimit PEM private keys.
	// RFC 7468 labels are "PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY",
	// "OPENSSH PRIVATE KEY", "ENCRYPTED PRIVATE KEY", ...
	privateKeyBeginPattern = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	privateKeyEndPattern   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
)

// pemRedactedMessage replaces a private-key header or body line; the block's
// content must never be stored, and the marker keeps the timeline readable.
const pemRedactedMessage = "[redacted]"

var redactionRules = []redactionRule{
	// Environment-style assignments such as AEGIS_JWT_SECRET=... The key is
	// kept for diagnosis; the value is not.
	{regexp.MustCompile(`(?i)\b(AEGIS_[A-Z0-9_]+)=(\S+)`), `$1=[redacted]`},
	// Authorization headers and any other "bearer <token>" text.
	{regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`), `${1}[redacted]`},
	// "Authorization: Basic <base64>" and bare "Basic <base64>". The keyword is
	// matched case-insensitively, the credential itself must contain at least
	// one upper-case, digit or base64 symbol so prose such as
	// "Basic authentication is required" is not mistaken for a credential.
	{regexp.MustCompile(`(?i:\b(basic\s+))[A-Za-z0-9+/]*[A-Z0-9+/=][A-Za-z0-9+/=]{7,}`), `${1}[redacted]`},
	// Any scheme with inline credentials (postgresql, redis, redis+ssl,
	// mysql, https, ...). The user part is optional: redis://:password@host is
	// a common shape. The scheme is kept so the log line stays diagnosable.
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*)://[^:\s/@]*:[^@\s/]+@`), `${1}://[redacted]@`},
	// Well-known access-token prefixes.
	{regexp.MustCompile(`\b(ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{10,}|sk-[A-Za-z0-9]{20,})\b`), `[redacted]`},
	// JSON Web Tokens: three base64url segments with the "eyJ" header prefix.
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), `[redacted-jwt]`},
	// PEM markers outside a tracked block (the log writer's state machine
	// handles lines inside one). The body of an untracked key cannot be
	// identified by shape, so the marker is what makes the omission auditable.
	{privateKeyBeginPattern, `[redacted-private-key]`},
	{privateKeyEndPattern, `[redacted-private-key]`},
}

// redactOutputLine replaces credential-shaped substrings in one line. It also
// reports how many values were replaced and whether any matched value survived
// (which the caller treats as a reason to drop the line).
func redactOutputLine(line string) (string, int, bool) {
	hits := 0
	var matched []string
	redacted := line
	for _, rule := range redactionRules {
		found := rule.pattern.FindAllString(redacted, -1)
		if len(found) == 0 {
			continue
		}
		hits += len(found)
		matched = append(matched, found...)
		redacted = rule.pattern.ReplaceAllString(redacted, rule.replacement)
	}
	for _, value := range matched {
		// A value that still appears after replacement means the rule did not
		// actually remove it; the line must not be stored.
		if strings.Contains(redacted, value) {
			return redacted, hits, false
		}
	}
	return redacted, hits, true
}

// valuesSurvive reports whether any matched value is still present after
// redaction. It is the fail-closed decision, kept separate so it can be tested
// without depending on a rule that fails to replace.
func valuesSurvive(redacted string, matched []string) bool {
	for _, value := range matched {
		if strings.Contains(redacted, value) {
			return true
		}
	}
	return false
}
