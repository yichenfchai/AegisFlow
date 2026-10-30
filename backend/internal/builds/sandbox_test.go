package builds

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/dbmigrate"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// requireDocker returns the image to use and fails when the environment
// promises a runtime (CI sets AEGIS_TEST_DOCKER=1) but none is usable. A silent
// skip must never stand in for a passing isolation test there.
func requireDocker(t *testing.T) string {
	t.Helper()
	forced := os.Getenv("AEGIS_TEST_DOCKER") == "1"
	if _, err := exec.LookPath("docker"); err != nil {
		if forced {
			t.Fatalf("AEGIS_TEST_DOCKER=1 but the docker CLI is missing: %v", err)
		}
		t.Skip("docker CLI not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := ProbeContainerRuntime(ctx); err != nil {
		if forced {
			t.Fatalf("AEGIS_TEST_DOCKER=1 but the runtime is unusable: %v", err)
		}
		t.Skipf("docker runtime unavailable: %v", err)
	}
	image := os.Getenv("AEGIS_TEST_DOCKER_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}
	if out, err := exec.CommandContext(ctx, "docker", "pull", image).CombinedOutput(); err != nil {
		t.Fatalf("pull %s: %v: %s", image, err, out)
	}
	return image
}

func TestContainerCommandArguments(t *testing.T) {
	t.Setenv("AEGIS_JWT_SECRET", "must-not-leak")
	sandbox := Sandbox{Mode: SandboxContainer, Image: "alpine:3.20", CPU: "1.5", Memory: "256m", Pids: 64}
	cmd, err := sandbox.commandForBuild(context.Background(), "aegis-build-test", "/tmp/work dir", "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"--network none",
		"--cpus 1.5",
		"--memory 256m",
		"--pids-limit 64",
		"--cap-drop ALL",
		"--security-opt no-new-privileges",
		"--read-only",
		"-v /tmp/work dir:/workspace",
		"-w /workspace",
		"alpine:3.20 sh -c echo hi",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("docker args missing %q: %s", want, args)
		}
	}
	for _, entry := range cmd.Env {
		if strings.HasPrefix(entry, "AEGIS_") {
			t.Fatalf("docker client environment leaked %q", entry)
		}
	}
}

func TestContainerCommandRejectsIncompleteLimits(t *testing.T) {
	_, err := (Sandbox{Mode: SandboxContainer, Image: "alpine:3.20"}).commandForBuild(context.Background(), "n", "/tmp", "true")
	if err == nil {
		t.Fatal("expected an error for missing limits")
	}
}

// TestContainerSandboxRunsBuild proves the end-to-end path: the command runs
// inside the disposable container, the root filesystem is read-only, and the
// service credentials are not visible to it.
func TestContainerSandboxRunsBuild(t *testing.T) {
	image := requireDocker(t)
	s, registry := testService(t)
	s.WithSandbox(Sandbox{Mode: SandboxContainer, Image: image, CPU: "1", Memory: "256m", Pids: 128})
	startWorker(t, s)
	ctx := context.Background()

	project := createProject(t, registry, "container-sandbox", "touch /aegis-escape 2>/dev/null && echo root-fs-writable || echo root-fs-read-only; echo container-ok; env")
	build, err := s.Trigger(ctx, testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, build.ID, StatusSucceeded)

	logs, err := s.Logs(ctx, testActor, build.ID, 0, maxLogPageSize)
	if err != nil {
		t.Fatal(err)
	}
	var joined strings.Builder
	for _, event := range logs {
		joined.WriteString(event.Message)
		joined.WriteString("\n")
	}
	output := joined.String()
	if !strings.Contains(output, "root-fs-read-only") || strings.Contains(output, "root-fs-writable") {
		t.Fatalf("container root filesystem looks writable:\n%s", output)
	}
	if !strings.Contains(output, "container-ok") {
		t.Fatalf("container output missing:\n%s", output)
	}
	if strings.Contains(output, "AEGIS_JWT_SECRET") || strings.Contains(output, "must-not-leak") {
		t.Fatalf("service credentials reached the container:\n%s", output)
	}
}

// testServiceWithTimeout mirrors testService but lets the timeout test shrink
// the wall clock without waiting ten minutes.
func testServiceWithTimeout(t *testing.T, timeout time.Duration) (*Service, *projects.Service) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	if err := dbmigrate.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	registry := projects.New(db)
	return newWithLimits(db, registry, timeout, maxLogEvents), registry
}

// TestContainerSandboxTimeoutLeavesNoContainer checks the cleanup path: a build
// that exceeds its timeout is failed and its container is removed rather than
// left running in the background.
func TestContainerSandboxTimeoutLeavesNoContainer(t *testing.T) {
	image := requireDocker(t)
	s, registry := testServiceWithTimeout(t, 3*time.Second)
	s.WithSandbox(Sandbox{Mode: SandboxContainer, Image: image, CPU: "1", Memory: "128m", Pids: 64})
	startWorker(t, s)
	ctx := context.Background()

	project := createProject(t, registry, "container-timeout", "sleep 30")
	build, err := s.Trigger(ctx, testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, build.ID, StatusFailed)

	name := "aegis-build-" + build.ID
	deadline := time.Now().Add(15 * time.Second)
	for {
		out, err := exec.Command("docker", "ps", "-aq", "--filter", "name="+name).CombinedOutput()
		if err != nil {
			t.Fatalf("list containers: %v: %s", err, out)
		}
		if strings.TrimSpace(string(out)) == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s still exists after timeout: %s", name, out)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
