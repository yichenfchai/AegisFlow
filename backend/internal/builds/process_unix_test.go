//go:build !windows

package builds

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// grandchildCommand backgrounds a long sleep and writes its pid before waiting,
// so a test can assert the whole process group dies with the build shell.
func grandchildCommand(pidFile string) string {
	return "sleep 30 & echo $! > '" + strings.ReplaceAll(pidFile, "'", `'\''`) + "'; wait"
}

func waitForPidFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild pid file %s was not written", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild process %d still exists after its build ended (kill error: %v)", pid, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestCancelKillsProcessGroup is the P1-05 regression: a plain Process.Kill
// only terminates the shell, leaving a backgrounded grandchild running.
func TestCancelKillsProcessGroup(t *testing.T) {
	s, registry := testService(t)
	startWorker(t, s)
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	project := createProject(t, registry, "process-group-cancel", grandchildCommand(pidFile))
	build, err := s.Trigger(context.Background(), testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, build.ID, StatusRunning)
	pid := waitForPidFile(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	if _, err = s.Cancel(context.Background(), testActor, build.ID); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, build.ID, StatusCancelled)
	assertProcessGone(t, pid)
}

// TestTimeoutKillsProcessGroup covers the same tree kill on the wall-clock
// timeout path.
func TestTimeoutKillsProcessGroup(t *testing.T) {
	s, registry := testService(t)
	s.WithTimeout(1500 * time.Millisecond)
	startWorker(t, s)
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	project := createProject(t, registry, "process-group-timeout", grandchildCommand(pidFile))
	build, err := s.Trigger(context.Background(), testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, build.ID, StatusRunning)
	pid := waitForPidFile(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	waitForStatus(t, s, build.ID, StatusFailed)
	waitForLog(t, s, build.ID, "timed out")
	assertProcessGone(t, pid)
}
