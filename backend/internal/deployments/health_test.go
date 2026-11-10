package deployments

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
)

// healthUpstream answers the deploy POST with 2xx and routes GETs through the
// supplied health function, so a test can keep the deploy successful while the
// health path misbehaves. The returned counter records health probes only.
func healthUpstream(t *testing.T, health func(attempt int32) int) (*httptest.Server, *int32) {
	t.Helper()
	var probes int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			attempt := atomic.AddInt32(&probes, 1)
			w.WriteHeader(health(attempt))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("deploy accepted\n"))
	}))
	t.Cleanup(server.Close)
	return server, &probes
}

// deployToHealthTarget creates a project and one deployment against target with
// the worker running, mirroring the other execution tests.
func deployToHealthTarget(t *testing.T, s *Service, registry *projects.Service, projectName string, target Target) Deployment {
	t.Helper()
	project := taskProject(t, registry, projectName)
	startDeployWorker(t, s)
	deployment, err := s.CreateDeployment(context.Background(), testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	return deployment
}

func TestDeployHealthCheckConfirmsSuccess(t *testing.T) {
	upstream, probes := healthUpstream(t, func(int32) int { return http.StatusOK })
	s, _, registry := taskService(t, Options{HealthCheckAttempts: 3, HealthCheckInterval: 10 * time.Millisecond})
	target := taskTarget(t, s, "health-ok", upstream.URL)

	deployment := deployToHealthTarget(t, s, registry, "health-ok", target)
	deployment = waitDeploymentStatus(t, s, deployment.ID, DeploySucceeded)
	if deployment.Detail != "" {
		t.Fatalf("detail = %q, want empty", deployment.Detail)
	}
	waitForEventText(t, s, deployment.ID, "system:health check passed (attempt 1/3")
	waitForEventText(t, s, deployment.ID, "system:deployment succeeded")
	// The first probe succeeded, so no retry should have been spent.
	if got := atomic.LoadInt32(probes); got != 1 {
		t.Fatalf("health probes = %d, want 1", got)
	}
}

func TestDeployHealthCheckRetriesUntilHealthy(t *testing.T) {
	upstream, probes := healthUpstream(t, func(attempt int32) int {
		if attempt < 3 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	s, _, registry := taskService(t, Options{HealthCheckAttempts: 3, HealthCheckInterval: 10 * time.Millisecond})
	target := taskTarget(t, s, "health-retry", upstream.URL)

	deployment := deployToHealthTarget(t, s, registry, "health-retry", target)
	deployment = waitDeploymentStatus(t, s, deployment.ID, DeploySucceeded)
	if deployment.Detail != "" {
		t.Fatalf("detail = %q, want empty", deployment.Detail)
	}
	waitForEventText(t, s, deployment.ID, "system:health check passed (attempt 3/3")
	text := joinEvents(deploymentEvents(t, s, deployment.ID))
	for _, want := range []string{
		"health check failed (attempt 1/3): http 503",
		"health check failed (attempt 2/3): http 503",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("events missing %q: %s", want, text)
		}
	}
	if got := atomic.LoadInt32(probes); got != 3 {
		t.Fatalf("health probes = %d, want 3", got)
	}
}

func TestDeployHealthCheckFailsDeployment(t *testing.T) {
	upstream, probes := healthUpstream(t, func(int32) int { return http.StatusInternalServerError })
	s, _, registry := taskService(t, Options{HealthCheckAttempts: 2, HealthCheckInterval: 10 * time.Millisecond})
	target := taskTarget(t, s, "health-bad", upstream.URL)

	deployment := deployToHealthTarget(t, s, registry, "health-bad", target)
	deployment = waitDeploymentStatus(t, s, deployment.ID, DeployFailed)
	if deployment.Detail != "health check failed: http 500" {
		t.Fatalf("detail = %q", deployment.Detail)
	}
	if deployment.FinishedAt == 0 {
		t.Fatalf("finishedAt = 0 on a failed deployment")
	}
	text := joinEvents(deploymentEvents(t, s, deployment.ID))
	for _, want := range []string{
		"health check failed (attempt 1/2): http 500",
		"health check failed (attempt 2/2): http 500",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("events missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "deployment succeeded") {
		t.Fatalf("an unhealthy target must not report success: %s", text)
	}
	if got := atomic.LoadInt32(probes); got != 2 {
		t.Fatalf("health probes = %d, want 2", got)
	}
}

func TestDeployHealthCheckDisabledByZeroAttempts(t *testing.T) {
	upstream, probes := healthUpstream(t, func(int32) int { return http.StatusInternalServerError })
	// Zero attempts is the documented opt-out; the deploy response alone decides.
	s, _, registry := taskService(t, Options{})
	target := taskTarget(t, s, "health-off", upstream.URL)

	deployment := deployToHealthTarget(t, s, registry, "health-off", target)
	deployment = waitDeploymentStatus(t, s, deployment.ID, DeploySucceeded)
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(probes); got != 0 {
		t.Fatalf("health probes = %d, want 0 when disabled", got)
	}
	if text := joinEvents(deploymentEvents(t, s, deployment.ID)); strings.Contains(text, "health check") {
		t.Fatalf("disabled health check wrote events: %s", text)
	}
}
