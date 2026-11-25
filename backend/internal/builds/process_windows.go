//go:build windows

package builds

import (
	"context"
	"os/exec"
	"strconv"
	"time"
)

// startProcessGroup is a no-op on Windows: the build command is the root of
// the taskkill tree, so no job object or extra process attributes are needed
// to terminate its descendants later.
func startProcessGroup(*exec.Cmd) {}

// killProcessGroup terminates the cmd.exe tree rooted at the build command.
// taskkill /T is used instead of Process.Kill so shell grandchildren (for
// example `start /b` background processes) do not outlive a cancel or timeout;
// when taskkill fails the direct process is killed as a fallback.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run(); err == nil {
		return nil
	}
	return cmd.Process.Kill()
}
