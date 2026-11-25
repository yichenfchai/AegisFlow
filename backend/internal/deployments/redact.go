package deployments

import (
	"regexp"
	"strings"
)

// Deployment response events are untrusted upstream output, but unlike build
// logs they are stored per deployment and readable by every role that can see
// the project. An upstream that echoes its Authorization header, prints a
// connection URL or dumps an environment assignment would otherwise persist
// the target credential. The redaction lives in this package (not builds) so
// the two domains stay independent; the rules are deliberately narrow so
// ordinary deploy output survives.
var deploymentRedactionRules = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	// Authorization headers and any other "bearer <token>" text.
	{regexp.MustCompile(`(?i)(bearer\s+)\S+`), `${1}[redacted]`},
	// Environment-style assignments such as AEGIS_JWT_SECRET=... The key is
	// kept for diagnosis; the value is not.
	{regexp.MustCompile(`(?i)\b(AEGIS_[A-Z0-9_]+)=\S+`), `$1=[redacted]`},
	// Connection URLs with inline credentials (PostgreSQL, Redis). The user
	// part is optional: redis://:password@host is a common shape.
	{regexp.MustCompile(`(?i)\b(postgres(?:ql)?|redis|rediss)://[^:\s/@]*:[^@\s/]+@`), `$1://[redacted]@`},
}

// redactResponseLine replaces the deployment's own secret and
// credential-shaped substrings in one upstream line with [redacted]. The exact
// secret match runs first, so a credential with an otherwise unrecognizable
// shape is still removed; the remaining rules cover credentials the target
// never sent as its own secret.
func redactResponseLine(line, secret string) string {
	if secret != "" {
		line = strings.ReplaceAll(line, secret, "[redacted]")
	}
	for _, rule := range deploymentRedactionRules {
		line = rule.pattern.ReplaceAllString(line, rule.replacement)
	}
	return line
}
