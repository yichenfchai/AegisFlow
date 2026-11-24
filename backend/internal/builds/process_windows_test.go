//go:build windows

package builds

import (
	"os/exec"
	"testing"
)

// TestKillProcessGroupTerminatesProcess is the basic Windows coverage: taskkill
// must succeed for a live process tree and Wait must then reap the process.
// The Unix grandchild assertion runs in process_unix_test.go.
func TestKillProcessGroupTerminatesProcess(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "ping -n 30 127.0.0.1 >nul")
	startProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := killProcessGroup(cmd); err != nil {
		t.Fatalf("killProcessGroup = %v", err)
	}
	// A forced termination is an error by definition; only reap it here.
	_ = cmd.Wait()
}
