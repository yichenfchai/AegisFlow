package builds

import (
	"regexp"
	"strings"
)

// Build output is untrusted: a command can print a token it was given, a
// database URL it discovered, or a bearer header. Every stdout/stderr line is
// therefore sanitized before it reaches build_log_events, which is readable by
// every role that can see the project.
//
// The rules are deliberately narrow so ordinary output survives: each one
// requires a credential-shaped substring (an AEGIS_* assignment, an
// Authorization value, a URL with inline credentials, a well-known token prefix
// or a JWT). A line is dropped when a matched value survives redaction, which
// is the fail-closed policy from the execution design (D6).
type redactionRule struct {
	pattern     *regexp.Regexp
	replacement string
}

var redactionRules = []redactionRule{
	// Environment-style assignments such as AEGIS_JWT_SECRET=... The key is
	// kept for diagnosis; the value is not.
	{regexp.MustCompile(`(?i)\b(AEGIS_[A-Z0-9_]+)=(\S+)`), `$1=[redacted]`},
	// Authorization headers and any other "bearer <token>" text.
	{regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`), `${1}[redacted]`},
	// Connection URLs with inline credentials (PostgreSQL, Redis, generic TCP).
	// The user part is optional: redis://:password@host is a common shape.
	{regexp.MustCompile(`(?i)\b(postgres(?:ql)?|redis|rediss)://[^:\s/@]*:[^@\s/]+@`), `$1://[redacted]@`},
	// Well-known access-token prefixes.
	{regexp.MustCompile(`\b(ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{10,}|sk-[A-Za-z0-9]{20,})\b`), `[redacted]`},
	// JSON Web Tokens: three base64url segments with the "eyJ" header prefix.
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), `[redacted-jwt]`},
	// PEM private-key headers. The body lines are not detected (they are
	// indistinguishable from build output); the header is what identifies them
	// and dropping it keeps the marker visible without the key material.
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`), `[redacted-private-key]`},
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
