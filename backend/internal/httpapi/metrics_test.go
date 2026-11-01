package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/metrics"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// The registry records the matched route template, never the raw path, so
// resource ids cannot leak into metric labels; unmatched requests collapse
// into "unmatched".
func TestMetricsRecording(t *testing.T) {
	db := testutil.OpenPostgres(t)
	service := auth.New(db, strings.Repeat("k", 32), time.Minute, time.Hour)
	if err := service.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Bootstrap(context.Background(), "admin", "a-strong-password"); err != nil {
		t.Fatal(err)
	}
	registry := metrics.New()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	handler := New(service, logger, db.PingContext, []string{"http://localhost:5173"}, Options{Limiter: newTestLimiter(), Projects: projects.New(db), Metrics: registry})

	token, _, _ := loginHTTP(t, handler, "admin", "a-strong-password")
	if w := request(handler, "GET", "/api/v1/projects", "", token); w.Code != 200 {
		t.Fatalf("projects list: %d %s", w.Code, w.Body.String())
	}
	if w := request(handler, "GET", "/api/v1/no-such-route", "", token); w.Code != 404 {
		t.Fatalf("unknown route: %d", w.Code)
	}

	scrape := httptest.NewServer(registry.Handler(nil))
	defer scrape.Close()
	resp, err := http.Get(scrape.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{
		`aegisflow_http_requests_total{method="GET",route="/api/v1/projects",status="200"} 1`,
		`aegisflow_http_requests_total{method="GET",route="unmatched",status="404"} 1`,
		`aegisflow_http_requests_total{method="POST",route="/api/v1/auth/login",status="200"} 1`,
		`aegisflow_http_request_duration_seconds_count{method="GET",route="/api/v1/projects"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "/api/v1/no-such-route") {
		t.Fatalf("raw path leaked into metrics:\n%s", text)
	}
}

// Without a registry the middleware must stay silent and the API unchanged.
func TestMetricsDisabled(t *testing.T) {
	handler, _ := testServer(t)
	token, _, _ := loginHTTP(t, handler, "admin", "a-strong-password")
	if w := request(handler, "GET", "/api/v1/projects", "", token); w.Code != 200 {
		t.Fatalf("projects list without metrics: %d", w.Code)
	}
}
