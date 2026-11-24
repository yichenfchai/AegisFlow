package deployments

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDeployResponseEventsAreRedactedBeforeStorage regression: upstream
// response lines were persisted verbatim, so a deploy endpoint that echoed its
// Authorization header (or printed a credential-shaped line) leaked the target
// secret into deployment events, which viewers of the project can read. Every
// response line must be sanitized before it reaches deployment_events, while
// ordinary output passes through unchanged.
func TestDeployResponseEventsAreRedactedBeforeStorage(t *testing.T) {
	const secret = "redact-deploy-token-42"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "remote: Authorization: Bearer %s\n", secret)
		_, _ = io.WriteString(w, "connecting with postgres://app:db-pass-1@db.internal:5432/aegis\n")
		_, _ = io.WriteString(w, "environment AEGIS_JWT_SECRET=jwt-secret-value\n")
		_, _ = io.WriteString(w, "cache redis://:redis-pass-9@cache.internal:6379/0\n")
		_, _ = io.WriteString(w, "deployment finished cleanly\n")
	}))
	defer upstream.Close()

	s, db, registry := taskService(t, Options{})
	project := taskProject(t, registry, "deploy-redaction")
	target, err := s.Create(context.Background(), testActor, Input{Name: "redaction-target", Endpoint: upstream.URL, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	startDeployWorker(t, s)
	deployment, err := s.CreateDeployment(context.Background(), testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	waitDeploymentStatus(t, s, deployment.ID, DeploySucceeded)
	waitForEventText(t, s, deployment.ID, "response:deployment finished cleanly")

	text := joinEvents(deploymentEvents(t, s, deployment.ID))
	for _, leaked := range []string{secret, "db-pass-1", "jwt-secret-value", "redis-pass-9"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("deployment events leaked %q: %s", leaked, text)
		}
	}
	if !strings.Contains(text, "[redacted]") {
		t.Fatalf("deployment events carry no redaction marker: %s", text)
	}
	// Ordinary output must survive: the redaction rules stay credential-shaped.
	if !strings.Contains(text, "response:deployment finished cleanly") {
		t.Fatalf("ordinary upstream output was changed: %s", text)
	}
	var leakedRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM deployment_events WHERE deployment_id=$1 AND message LIKE '%'||$2||'%'`,
		deployment.ID, secret).Scan(&leakedRows); err != nil {
		t.Fatal(err)
	}
	if leakedRows != 0 {
		t.Fatalf("secret found in %d stored deployment event rows", leakedRows)
	}
}

// TestRedactResponseLine pins the rule set: credential-shaped values are
// replaced, while ordinary deploy output and ordinary URLs are untouched.
func TestRedactResponseLine(t *testing.T) {
	const secret = "deploy-secret-value"
	cases := []struct {
		name string
		line string
		want string
	}{
		{"own secret", "echo deploy-secret-value", "echo [redacted]"},
		{"bearer header", "Authorization: Bearer abcdef123456", "Authorization: Bearer [redacted]"},
		{"own secret in bearer header", "Authorization: Bearer deploy-secret-value", "Authorization: Bearer [redacted]"},
		{"aegis assignment", "AEGIS_JWT_SECRET=topsecret", "AEGIS_JWT_SECRET=[redacted]"},
		{"postgres url", "dsn=postgres://app:db-pass@db.internal/app", "dsn=postgres://[redacted]@db.internal/app"},
		{"redis url", "cache redis://:redis-pass@cache:6379/0", "cache redis://[redacted]@cache:6379/0"},
		{"ordinary output", "deploy finished in 12ms", "deploy finished in 12ms"},
		{"ordinary url", "health https://example.com/healthz", "health https://example.com/healthz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactResponseLine(tc.line, secret); got != tc.want {
				t.Fatalf("redactResponseLine(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
	// The exact-secret rule is a literal replacement, so regex metacharacters
	// in a credential cannot make it survive.
	if got := redactResponseLine("value a.b+c", "a.b+c"); got != "value [redacted]" {
		t.Fatalf("literal secret not redacted: %q", got)
	}
	if got := redactResponseLine("nothing to hide", ""); got != "nothing to hide" {
		t.Fatalf("an empty secret changed a normal line: %q", got)
	}
}
