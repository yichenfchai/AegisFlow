package builds

import (
	"strings"
	"testing"
)

func TestRedactOutputLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
		gone []string
		hits int
	}{
		{
			name: "environment style secret keeps the key",
			in:   "AEGIS_JWT_SECRET=not-a-real-secret",
			want: []string{"AEGIS_JWT_SECRET=", "[redacted]"},
			gone: []string{"not-a-real-secret"},
			hits: 1,
		},
		{
			name: "lowercase assignment",
			in:   "aegis_database_url=postgres://a:b@host/db",
			want: []string{"aegis_database_url=[redacted]"},
			gone: []string{"a:b@host"},
			hits: 1,
		},
		{
			name: "authorization header",
			in:   "Authorization: Bearer abcdefghijklmnop",
			want: []string{"Bearer [redacted]"},
			gone: []string{"abcdefghijklmnop"},
			hits: 1,
		},
		{
			name: "postgres url with credentials",
			in:   "connecting to postgres://aegis:hunter2@127.0.0.1:5432/aegisflow",
			want: []string{"postgres://[redacted]@127.0.0.1:5432/aegisflow"},
			gone: []string{"hunter2"},
			hits: 1,
		},
		{
			name: "redis url with password only",
			in:   "redis://:s3cret-value@127.0.0.1:6379/0",
			want: []string{"redis://[redacted]@"},
			gone: []string{"s3cret-value"},
			hits: 1,
		},
		{
			name: "generic scheme with inline credentials",
			in:   "mysql://appdb:pa55w0rd@db.internal:3306/app",
			want: []string{"mysql://[redacted]@"},
			gone: []string{"pa55w0rd"},
			hits: 1,
		},
		{
			name: "https url with inline credentials",
			in:   "fetching https://svc:token-value@example.com/repo.git",
			want: []string{"https://[redacted]@"},
			gone: []string{"token-value"},
			hits: 1,
		},
		{
			name: "url without credentials is untouched",
			in:   "fetching https://example.com/repo?ref=main",
			want: []string{"https://example.com/repo?ref=main"},
			hits: 0,
		},
		{
			name: "authorization basic header",
			in:   "Authorization: Basic dXNlcjpwYXNz",
			want: []string{"Authorization: Basic [redacted]"},
			gone: []string{"dXNlcjpwYXNz"},
			hits: 1,
		},
		{
			name: "lowercase basic credential",
			in:   "curl -H 'authorization: basic YWJjZGVmZ2hpamtsbW5vcA=='",
			want: []string{"basic [redacted]"},
			gone: []string{"YWJjZGVmZ2hpamtsbW5vcA=="},
			hits: 1,
		},
		{
			name: "prose about basic authentication is not a credential",
			in:   "Basic authentication is required for this endpoint",
			want: []string{"Basic authentication is required for this endpoint"},
			hits: 0,
		},
		{
			name: "github token",
			in:   "using ghp_abcdefghijklmnopqrstuvwxyz0123",
			want: []string{"[redacted]"},
			gone: []string{"ghp_abcdefghijklmnopqrstuvwxyz0123"},
			hits: 1,
		},
		{
			name: "aws access key id",
			in:   "AKIAIOSFODNN7EXAMPLE",
			want: []string{"[redacted]"},
			gone: []string{"AKIAIOSFODNN7EXAMPLE"},
			hits: 1,
		},
		{
			name: "jwt",
			in:   "token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
			want: []string{"[redacted-jwt]"},
			gone: []string{"eyJhbGciOiJIUzI1NiJ9"},
			hits: 1,
		},
		{
			name: "private key header",
			in:   "-----BEGIN RSA PRIVATE KEY-----",
			want: []string{"[redacted-private-key]"},
			gone: []string{"BEGIN RSA PRIVATE KEY"},
			hits: 1,
		},
		{
			name: "two secrets on one line",
			in:   "AEGIS_A=one-secret and AEGIS_B=two-secret",
			want: []string{"AEGIS_A=[redacted]", "AEGIS_B=[redacted]"},
			gone: []string{"one-secret", "two-secret"},
			hits: 2,
		},
		{
			name: "ordinary output is untouched",
			in:   "ok github.com/yichenfchai/AegisFlow/backend/internal/builds 6.4s",
			want: []string{"ok github.com/yichenfchai/AegisFlow/backend/internal/builds 6.4s"},
			hits: 0,
		},
		{
			name: "prose that mentions a password is not a credential shape",
			in:   "warning: password must be at least 12 bytes",
			want: []string{"warning: password must be at least 12 bytes"},
			hits: 0,
		},
		{
			name: "short bearer word is not a token",
			in:   "Bearer token",
			want: []string{"Bearer token"},
			hits: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, hits, ok := redactOutputLine(tc.in)
			if !ok {
				t.Fatalf("line unexpectedly marked unredactable: %q", got)
			}
			if hits != tc.hits {
				t.Fatalf("hits = %d, want %d (%q)", hits, tc.hits, got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("%q missing from %q", want, got)
				}
			}
			for _, gone := range tc.gone {
				if strings.Contains(got, gone) {
					t.Fatalf("%q still present in %q", gone, got)
				}
			}
		})
	}
}

func TestValuesSurvive(t *testing.T) {
	if valuesSurvive("Bearer [redacted]", []string{"Bearer abcdefghijklmnop"}) {
		t.Fatal("a removed value reported as surviving")
	}
	if !valuesSurvive("AEGIS_JWT_SECRET=abc", []string{"abc"}) {
		t.Fatal("a surviving value was not detected")
	}
	if valuesSurvive("clean line", nil) {
		t.Fatal("no matches must not report survival")
	}
}

// TestLogWriterRedactsMultilinePrivateKey is the P2-10 regression for PEM
// bodies: only the BEGIN/END delimiters identify a key, so the log writer must
// track the block across lines and redact every line inside it. The body lines
// deliberately have no recognizable token shape; output outside the block must
// survive unchanged, and stderr keeps its own block state.
func TestLogWriterRedactsMultilinePrivateKey(t *testing.T) {
	w := streamOnlyWriter()
	console := strings.Join([]string{
		"before-key",
		"-----BEGIN RSA PRIVATE KEY-----",
		"MIIEowIBAAKCAQEA-body-line-one-must-not-be-stored",
		"body-line-two-must-not-be-stored",
		"-----END RSA PRIVATE KEY-----",
		"between-blocks",
		"-----BEGIN OPENSSH PRIVATE KEY-----",
		"b3BlbnNzaC1rZXkt-body-line-three-must-not-be-stored",
		"-----END OPENSSH PRIVATE KEY-----",
		"after-key",
		"-----BEGIN PRIVATE KEY-----",
		"unterminated-body-line-must-not-be-stored",
		"still-in-block-must-not-be-stored",
	}, "\n") + "\n"
	w.stream(strings.NewReader(console), "stdout")

	var messages []string
	for _, event := range w.pending {
		messages = append(messages, event.Message)
		if strings.Contains(event.Message, "must-not-be-stored") {
			t.Fatalf("private key material reached storage: stream=%q message=%q", event.Stream, event.Message)
		}
	}
	stored := strings.Join(messages, "\n")
	for _, want := range []string{"before-key", "between-blocks", "after-key"} {
		if !strings.Contains(stored, want) {
			t.Fatalf("ordinary line %q was dropped: %q", want, stored)
		}
	}
	if !strings.Contains(stored, "[redacted]") {
		t.Fatalf("no redaction marker stored: %q", stored)
	}
	// 4 lines in the RSA block, 3 in the OpenSSH block and 3 in the
	// unterminated block; each line counts as one redacted value.
	if w.redacted != 10 {
		t.Fatalf("redacted count = %d, want 10", w.redacted)
	}
	for _, event := range w.pending {
		if event.Message == "[redacted]" && event.Stream != "system" {
			t.Fatalf("redacted block line stored on stream %q", event.Stream)
		}
	}
	// A private-key block on stdout must not swallow unrelated stderr output.
	w.stream(strings.NewReader("stderr-ok\n"), "stderr")
	if last := w.pending[len(w.pending)-1]; last.Message != "stderr-ok" || last.Stream != "stderr" {
		t.Fatalf("stderr line was hit by the stdout block state: %+v", last)
	}
}

// TestLogWriterRedactsSystemLines is the P2-10 regression for the system stream:
// internal messages embed command error text, so they must go through the same
// rules instead of being treated as trusted. Ordinary system lines keep their
// meaning.
func TestLogWriterRedactsSystemLines(t *testing.T) {
	w := streamOnlyWriter()
	w.systemf("AEGIS_JWT_SECRET=%s and postgres://app:%s@db/x and Basic %s and Bearer %s",
		"leaked-env-value", "leaked-db-pass", "dXNlcjpwYXNz", "abcdefghijklmnop")
	w.systemf("build succeeded")
	if len(w.pending) != 2 {
		t.Fatalf("captured %d events, want 2: %+v", len(w.pending), w.pending)
	}
	stored := w.pending[0]
	if stored.Stream != "system" {
		t.Fatalf("system line stored on stream %q", stored.Stream)
	}
	for _, leaked := range []string{"leaked-env-value", "leaked-db-pass", "dXNlcjpwYXNz", "abcdefghijklmnop"} {
		if strings.Contains(stored.Message, leaked) {
			t.Fatalf("system line leaked %q: %q", leaked, stored.Message)
		}
	}
	if !strings.Contains(stored.Message, "[redacted]") {
		t.Fatalf("system line was not redacted: %q", stored.Message)
	}
	if w.redacted != 4 {
		t.Fatalf("redacted count = %d, want 4", w.redacted)
	}
	if w.pending[1].Message != "build succeeded" {
		t.Fatalf("ordinary system line changed: %q", w.pending[1].Message)
	}
}
