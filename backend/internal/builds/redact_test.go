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
