package metrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func scrape(t *testing.T, registry *Registry, gauges []Gauge) string {
	t.Helper()
	server := httptest.NewServer(registry.Handler(func(context.Context) []Gauge { return gauges }))
	defer server.Close()
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("content type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache control = %q", cc)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestExpositionFormat(t *testing.T) {
	registry := New()
	registry.ObserveHTTP("GET", "/api/v1/builds", 200, 30*time.Millisecond)
	registry.ObserveHTTP("GET", "/api/v1/builds", 200, 60*time.Millisecond)
	registry.ObserveHTTP("POST", "/api/v1/projects", 201, 4*time.Millisecond)
	registry.ObserveHTTP("GET", "", 404, time.Millisecond)

	body := scrape(t, registry, []Gauge{
		{Name: "aegisflow_builds", Help: "Builds by status.", Labels: map[string]string{"status": "queued"}, Value: 2},
		{Name: "aegisflow_builds", Help: "Builds by status.", Labels: map[string]string{"status": "failed"}, Value: 1},
	})

	for _, want := range []string{
		`aegisflow_http_requests_total{method="GET",route="/api/v1/builds",status="200"} 2`,
		`aegisflow_http_requests_total{method="POST",route="/api/v1/projects",status="201"} 1`,
		`aegisflow_http_requests_total{method="GET",route="unmatched",status="404"} 1`,
		`aegisflow_http_request_duration_seconds_bucket{le="+Inf",method="GET",route="/api/v1/builds"} 2`,
		`aegisflow_http_request_duration_seconds_count{method="GET",route="/api/v1/builds"} 2`,
		`# TYPE aegisflow_http_request_duration_seconds histogram`,
		`# TYPE aegisflow_builds gauge`,
		`aegisflow_builds{status="queued"} 2`,
		`aegisflow_builds{status="failed"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	// Buckets are cumulative and must not exceed the total.
	sum := 0.0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, `aegisflow_http_request_duration_seconds_bucket{le="0.025",method="GET",route="/api/v1/builds"}`) && !strings.HasSuffix(line, " 0") {
			t.Fatalf("30ms request wrongly counted in the 25ms bucket: %s", line)
		}
		if strings.HasPrefix(line, `aegisflow_http_request_duration_seconds_bucket{le="0.05",method="GET",route="/api/v1/builds"}`) && !strings.HasSuffix(line, " 1") {
			t.Fatalf("50ms bucket = %q, want 1", line)
		}
		if strings.HasPrefix(line, `aegisflow_http_request_duration_seconds_bucket{le="0.1",method="GET",route="/api/v1/builds"}`) && !strings.HasSuffix(line, " 2") {
			t.Fatalf("100ms bucket = %q, want 2", line)
		}
		if strings.HasPrefix(line, `aegisflow_http_request_duration_seconds_sum{method="GET",route="/api/v1/builds"}`) {
			sum = parseValue(t, line)
		}
	}
	if sum < 0.089 || sum > 0.091 {
		t.Fatalf("duration sum = %v, want about 0.09", sum)
	}
	// Deterministic output: gauges sorted by name then labels.
	if strings.Index(body, `status="failed"`) > strings.Index(body, `status="queued"`) {
		t.Fatalf("gauge samples are not sorted:\n%s", body)
	}
}

func TestLabelEscaping(t *testing.T) {
	registry := New()
	body := scrape(t, registry, []Gauge{{
		Name:   "aegisflow_probe",
		Help:   "line one\nline two",
		Labels: map[string]string{"value": "a\"b\\c\nd"},
		Value:  1,
	}})
	if !strings.Contains(body, `aegisflow_probe{value="a\"b\\c\nd"} 1`) {
		t.Fatalf("label escaping wrong:\n%s", body)
	}
	if !strings.Contains(body, "# HELP aegisflow_probe line one\\nline two") {
		t.Fatalf("help escaping wrong:\n%s", body)
	}
}

func TestEmptyRegistryAndNilHandler(t *testing.T) {
	body := scrape(t, New(), nil)
	if strings.TrimSpace(body) != "" {
		t.Fatalf("empty registry wrote %q", body)
	}
	var nilRegistry *Registry
	server := httptest.NewServer(nilRegistry.Handler(nil))
	defer server.Close()
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("nil registry status = %d", resp.StatusCode)
	}
}

func TestConcurrentObservation(t *testing.T) {
	registry := New()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				registry.ObserveHTTP("GET", "/api/v1/projects", 200, time.Millisecond)
			}
		}()
	}
	wg.Wait()
	body := scrape(t, registry, nil)
	if !strings.Contains(body, `aegisflow_http_requests_total{method="GET",route="/api/v1/projects",status="200"} 1000`) {
		t.Fatalf("concurrent count wrong:\n%s", body)
	}
	if !strings.Contains(body, `aegisflow_http_request_duration_seconds_count{method="GET",route="/api/v1/projects"} 1000`) {
		t.Fatalf("histogram count wrong:\n%s", body)
	}
}

func parseValue(t *testing.T, line string) float64 {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) < 2 {
		t.Fatalf("malformed metric line %q", line)
	}
	parsed, err := strconv.ParseFloat(fields[len(fields)-1], 64)
	if err != nil {
		t.Fatalf("parse %q: %v", line, err)
	}
	return parsed
}
