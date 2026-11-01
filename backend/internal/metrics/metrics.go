// Package metrics is a dependency-free Prometheus text exposition for the
// operational signals this service actually has: HTTP request counts and a
// latency histogram keyed by route template, plus point-in-time gauges such as
// builds per status. It is served on a separate listener (AEGIS_METRICS_ADDR)
// so scraping needs no user session and the public API origin stays unchanged.
//
// Labels deliberately never carry raw paths, user ids, project ids or command
// output: the route label is Gin's matched template (for example
// "/api/v1/builds/:id"), and unmatched requests collapse into "unmatched".
package metrics

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// durationBuckets are the histogram upper bounds in seconds. They cover a fast
// health probe through a slow listing under load.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type requestKey struct {
	method, route string
	status        int
}

type routeKey struct {
	method, route string
}

type histogram struct {
	buckets []uint64
	count   uint64
	sum     float64
}

// Registry accumulates counters and histograms. It is safe for concurrent use
// and writes a deterministic exposition so tests can compare exact output.
type Registry struct {
	mu        sync.Mutex
	requests  map[requestKey]uint64
	durations map[routeKey]*histogram
}

// Gauge is one point-in-time sample supplied by a callback at scrape time.
type Gauge struct {
	Name   string
	Help   string
	Labels map[string]string
	Value  float64
}

func New() *Registry {
	return &Registry{
		requests:  map[requestKey]uint64{},
		durations: map[routeKey]*histogram{},
	}
}

// ObserveHTTP records one completed request. route is the matched template, not
// the raw path; callers pass "unmatched" when no route matched.
func (r *Registry) ObserveHTTP(method, route string, status int, duration time.Duration) {
	if r == nil {
		return
	}
	if route == "" {
		route = "unmatched"
	}
	seconds := duration.Seconds()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests[requestKey{method: method, route: route, status: status}]++
	key := routeKey{method: method, route: route}
	h := r.durations[key]
	if h == nil {
		h = &histogram{buckets: make([]uint64, len(durationBuckets))}
		r.durations[key] = h
	}
	for i, bound := range durationBuckets {
		if seconds <= bound {
			h.buckets[i]++
		}
	}
	h.count++
	h.sum += seconds
}

// Handler exposes the registry plus optional scrape-time gauges. The response
// is the Prometheus text format (version 0.0.4).
func (r *Registry) Handler(gauges func(context.Context) []Gauge) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		var buf bytes.Buffer
		if r != nil {
			r.write(&buf)
		}
		if gauges != nil {
			writeGauges(&buf, gauges(req.Context()))
		}
		_, _ = w.Write(buf.Bytes())
	})
}

func (r *Registry) write(buf *bytes.Buffer) {
	r.mu.Lock()
	requests := make([]requestKey, 0, len(r.requests))
	for key := range r.requests {
		requests = append(requests, key)
	}
	values := make(map[requestKey]uint64, len(r.requests))
	for key, value := range r.requests {
		values[key] = value
	}
	histograms := make(map[routeKey]histogram, len(r.durations))
	for key, h := range r.durations {
		cloned := histogram{buckets: append([]uint64(nil), h.buckets...), count: h.count, sum: h.sum}
		histograms[key] = cloned
	}
	r.mu.Unlock()

	sort.Slice(requests, func(i, j int) bool {
		if requests[i].route != requests[j].route {
			return requests[i].route < requests[j].route
		}
		if requests[i].method != requests[j].method {
			return requests[i].method < requests[j].method
		}
		return requests[i].status < requests[j].status
	})
	if len(requests) > 0 {
		fmt.Fprintln(buf, "# HELP aegisflow_http_requests_total HTTP requests by method, matched route template and status.")
		fmt.Fprintln(buf, "# TYPE aegisflow_http_requests_total counter")
	}
	for _, key := range requests {
		fmt.Fprintf(buf, "aegisflow_http_requests_total%s %d\n",
			formatLabels(map[string]string{"method": key.method, "route": key.route, "status": strconv.Itoa(key.status)}), values[key])
	}

	routes := make([]routeKey, 0, len(histograms))
	for key := range histograms {
		routes = append(routes, key)
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].route != routes[j].route {
			return routes[i].route < routes[j].route
		}
		return routes[i].method < routes[j].method
	})
	if len(routes) > 0 {
		fmt.Fprintln(buf, "# HELP aegisflow_http_request_duration_seconds HTTP request latency by method and matched route template.")
		fmt.Fprintln(buf, "# TYPE aegisflow_http_request_duration_seconds histogram")
	}
	for _, key := range routes {
		h := histograms[key]
		for i, bound := range durationBuckets {
			fmt.Fprintf(buf, "aegisflow_http_request_duration_seconds_bucket%s %d\n",
				formatLabels(map[string]string{"method": key.method, "route": key.route, "le": strconv.FormatFloat(bound, 'g', -1, 64)}), h.buckets[i])
		}
		fmt.Fprintf(buf, "aegisflow_http_request_duration_seconds_bucket%s %d\n",
			formatLabels(map[string]string{"method": key.method, "route": key.route, "le": "+Inf"}), h.count)
		fmt.Fprintf(buf, "aegisflow_http_request_duration_seconds_sum%s %s\n",
			formatLabels(map[string]string{"method": key.method, "route": key.route}),
			strconv.FormatFloat(h.sum, 'g', -1, 64))
		fmt.Fprintf(buf, "aegisflow_http_request_duration_seconds_count%s %d\n",
			formatLabels(map[string]string{"method": key.method, "route": key.route}), h.count)
	}
}

func writeGauges(buf *bytes.Buffer, gauges []Gauge) {
	sort.Slice(gauges, func(i, j int) bool {
		if gauges[i].Name != gauges[j].Name {
			return gauges[i].Name < gauges[j].Name
		}
		return labelString(gauges[i].Labels) < labelString(gauges[j].Labels)
	})
	seen := map[string]bool{}
	for _, gauge := range gauges {
		if gauge.Name == "" {
			continue
		}
		if !seen[gauge.Name] {
			seen[gauge.Name] = true
			if gauge.Help != "" {
				fmt.Fprintf(buf, "# HELP %s %s\n", gauge.Name, escapeHelp(gauge.Help))
			}
			fmt.Fprintf(buf, "# TYPE %s gauge\n", gauge.Name)
		}
		fmt.Fprintf(buf, "%s%s %s\n", gauge.Name, formatLabels(gauge.Labels), strconv.FormatFloat(gauge.Value, 'g', -1, 64))
	}
}

func labelString(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+labels[key])
	}
	return strings.Join(parts, ",")
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+`="`+escapeLabel(labels[key])+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// escapeLabel escapes the three characters Prometheus forbids raw in label
// values. Values come from the database (build statuses) and are still escaped.
func escapeLabel(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return replacer.Replace(value)
}

func escapeHelp(value string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(value)
}
