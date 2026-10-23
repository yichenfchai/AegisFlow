package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Explicit, deterministic test-only fake; never a production fallback.
type memoryTestLimiter struct {
	mu     sync.Mutex
	counts map[string]int
}

func newTestLimiter() *memoryTestLimiter { return &memoryTestLimiter{counts: map[string]int{}} }
func (l *memoryTestLimiter) Allow(_ context.Context, key string) (bool, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts[key] >= 10 {
		return false, 1500 * time.Millisecond, nil
	}
	l.counts[key]++
	return true, 0, nil
}

type errorTestLimiter struct{}

func (errorTestLimiter) Allow(context.Context, string) (bool, time.Duration, error) {
	return true, 0, errors.New("secret Redis failure")
}

func TestLoginLimiterFailClosed(t *testing.T) {
	for _, opts := range []Options{{}, {Limiter: errorTestLimiter{}}} {
		h := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, []string{"http://localhost:5173"}, opts)
		for i := 0; i < 12; i++ {
			w := request(h, "POST", "/api/v1/auth/login", `{}`, "")
			if w.Code != 503 || strings.Contains(w.Body.String(), "secret") || w.Header().Get("Retry-After") != "" {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		}
	}
}

func TestClientIPTrustAndQuota(t *testing.T) {
	for _, tc := range []struct {
		name            string
		proxies         []string
		peer, xff, want string
	}{
		{"default ignores spoofing", nil, "192.0.2.9:1234", "198.51.100.10", "192.0.2.9"},
		{"untrusted peer", []string{"10.0.0.0/8"}, "192.0.2.9:1234", "198.51.100.10", "192.0.2.9"},
		{"trusted proxy", []string{"10.0.0.0/8"}, "10.0.0.2:1234", "198.51.100.10, 10.0.0.3", "198.51.100.10"},
		{"trusted proxy ignores other IP headers", []string{"10.0.0.0/8"}, "10.0.0.2:1234", "", "10.0.0.2"},
		{"rightmost untrusted hop", []string{"10.0.0.0/8"}, "10.0.0.2:1234", "203.0.113.1, 198.51.100.10", "198.51.100.10"},
		{"invalid config disables trust", []string{"not-cidr"}, "192.0.2.9:1234", "198.51.100.10", "192.0.2.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newTestLimiter()
			h := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, []string{"http://localhost:5173"}, Options{Limiter: l, TrustedProxies: tc.proxies})
			// Invalid JSON stops before auth while still consuming a login attempt.
			for i := 0; i < 11; i++ {
				r := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{`))
				r.RemoteAddr = tc.peer
				r.Header.Set("Origin", "http://localhost:5173")
				r.Header.Set("Content-Type", "application/json")
				xff := tc.xff
				if tc.name == "default ignores spoofing" || tc.name == "untrusted peer" {
					xff = fmt.Sprintf("198.51.100.%d", i+1)
				}
				if tc.name == "rightmost untrusted hop" {
					xff = fmt.Sprintf("203.0.113.%d, 198.51.100.10", i+1)
				}
				r.Header.Set("X-Forwarded-For", xff)
				r.Header.Set("X-Real-IP", fmt.Sprintf("203.0.113.%d", i+1))
				r.Header.Set("Forwarded", fmt.Sprintf("for=203.0.113.%d", i+1))
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				want := 400
				if i == 10 {
					want = 429
					if w.Header().Get("Retry-After") != "2" {
						t.Fatal(w.Header())
					}
				}
				if w.Code != want {
					t.Fatalf("attempt %d: %d %s", i, w.Code, w.Body.String())
				}
			}
			if len(l.counts) != 1 || l.counts[tc.want] != 10 {
				t.Fatalf("IP keys: %v", l.counts)
			}
		})
	}
}

func TestReadinessDependencyMessage(t *testing.T) {
	h := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), func(context.Context) error { return errors.New("private dependency error") }, []string{"http://localhost:5173"})
	w := request(h, "GET", "/readyz", "", "")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "dependencies unavailable") || strings.Contains(w.Body.String(), "private") {
		t.Fatal(w.Code, w.Body.String())
	}
}
