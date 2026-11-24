//go:build !windows

package builds

import (
	"os/exec"
	"syscall"
)

// startProcessGroup puts the build command in its own process group so that a
// cancel or timeout can terminate the shell together with every process it
// spawned. The worker's own process group is deliberately left untouched.
func startProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup sends SIGKILL to the build command's process group. A shell
// that backgrounded a grandchild would otherwise survive Process.Kill and keep
// running (and holding the output pipes) after the build was cancelled. When
// the group is already gone the direct process is killed as a fallback.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// A negative pid addresses the whole group whose leader is the child.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
