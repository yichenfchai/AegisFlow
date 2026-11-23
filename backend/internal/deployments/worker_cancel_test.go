package deployments

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fastDeployUpstream answers every deploy POST and health GET with a 2xx so a
// follow-up deployment can complete while the test watches it.
func fastDeployUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	}))
	t.Cleanup(server.Close)
	return server
}

// waitForStatusWithin is the bounded variant of waitDeploymentStatus used by
// cancellation tests: the worker must settle the row quickly, not merely
// eventually.
func waitForStatusWithin(t *testing.T, s *Service, id string, within time.Duration, want string) Deployment {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		deployment, err := s.GetDeployment(context.Background(), testActor, id)
		if err != nil {
			t.Fatal(err)
		}
		if deployment.Status == want {
			return deployment
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment still %s after %s, want %s", deployment.Status, within, want)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func waitForEventWithin(t *testing.T, s *Service, id, substr string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if strings.Contains(joinEvents(deploymentEvents(t, s, id)), substr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("event %q not seen within %s; got %s", substr, within, joinEvents(deploymentEvents(t, s, id)))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestDeployCancelAbortsSlowResponseBody regression: the cancellation watchdog
// used to stop as soon as the deploy POST returned its status line, so a slow
// streaming body kept the serial worker busy until the deployment timeout. The
// watchdog must cover the whole execution: cancel has to abort the body read
// and free the worker for the next queued deployment within the watchdog
// cadence, not the deploy timeout.
func TestDeployCancelAbortsSlowResponseBody(t *testing.T) {
	release := make(chan struct{})
	bodyStarted := make(chan struct{})
	var startedOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
		startedOnce.Do(func() { close(bodyStarted) })
		// Keep the response body open and trickling: without a cancellation
		// that reaches the body read this handler never lets the deployment
		// finish on its own.
		for index := 0; ; index++ {
			select {
			case <-release:
				return
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
				_, _ = fmt.Fprintf(w, "still deploying %d\n", index)
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { close(release) })

	s, _, registry := taskService(t, Options{})
	project := taskProject(t, registry, "cancel-slow-body")
	slow := taskTarget(t, s, "slow-stream", upstream.URL)
	fast := taskTarget(t, s, "fast-stream", fastDeployUpstream(t).URL)
	startDeployWorker(t, s)

	deployment, err := s.CreateDeployment(context.Background(), testActor, project.ID, DeployInput{TargetID: slow.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	waitDeploymentStatus(t, s, deployment.ID, DeployRunning)
	select {
	case <-bodyStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream never started streaming the response body")
	}
	// A follow-up deployment is only claimed once execute returns, so it is the
	// portable signal that cancellation actually released the worker.
	followUp, err := s.CreateDeployment(context.Background(), testActor, project.ID, DeployInput{TargetID: fast.ID, Version: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelDeployment(context.Background(), testActor, deployment.ID); err != nil {
		t.Fatal(err)
	}
	waitForEventWithin(t, s, deployment.ID, "system:deployment cancelled", 2*time.Second)
	waitForStatusWithin(t, s, followUp.ID, 2*time.Second, DeploySucceeded)
}

// TestDeployCancelAbortsHangingHealthCheck regression: the post-deploy health
// probe used the execution context rather than the cancellable run context, so
// a hanging health endpoint held the worker for every probe attempt even after
// the operator cancelled the deployment. The probe must be cancelled with the
// run and the worker must move on within the watchdog cadence.
func TestDeployCancelAbortsHangingHealthCheck(t *testing.T) {
	release := make(chan struct{})
	healthStarted := make(chan struct{})
	var startedOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			startedOnce.Do(func() { close(healthStarted) })
			select {
			case <-release:
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "deploy accepted\n")
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { close(release) })

	s, _, registry := taskService(t, Options{HealthCheckAttempts: 2, HealthCheckInterval: 10 * time.Millisecond})
	project := taskProject(t, registry, "cancel-hanging-health")
	slowHealth := taskTarget(t, s, "slow-health", upstream.URL)
	fast := taskTarget(t, s, "fast-health", fastDeployUpstream(t).URL)
	startDeployWorker(t, s)

	deployment, err := s.CreateDeployment(context.Background(), testActor, project.ID, DeployInput{TargetID: slowHealth.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-healthStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the post-deploy health probe never started")
	}
	followUp, err := s.CreateDeployment(context.Background(), testActor, project.ID, DeployInput{TargetID: fast.ID, Version: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelDeployment(context.Background(), testActor, deployment.ID); err != nil {
		t.Fatal(err)
	}
	waitForEventWithin(t, s, deployment.ID, "system:deployment cancelled", 2*time.Second)
	waitForStatusWithin(t, s, followUp.ID, 2*time.Second, DeploySucceeded)
}
