//go:build linux

package builds

import (
	"context"
	"os"
	"os/user"
	"strings"
	"testing"
)

// TestUserCommandArguments checks the privilege-drop command without needing
// root: setpriv is constructed for the target account and the environment stays
// on the allowlist.
func TestUserCommandArguments(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AEGIS_JWT_SECRET", "must-not-leak")
	cmd, err := (Sandbox{Mode: SandboxUser, User: current.Username}).commandForBuild(context.Background(), "n", "/tmp/work", "/tmp/work", "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"setpriv",
		"--reuid " + current.Uid,
		"--regid " + current.Gid,
		"--clear-groups",
		"/bin/sh -c echo hi",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("setpriv args missing %q: %s", want, args)
		}
	}
	for _, entry := range cmd.Env {
		if strings.HasPrefix(entry, "AEGIS_") {
			t.Fatalf("build environment leaked %q", entry)
		}
	}
}

// TestProbeUserSandbox asserts both halves of the fail-closed preflight: as a
// non-root process it refuses, and as root it accepts a real account.
func TestProbeUserSandbox(t *testing.T) {
	err := ProbeUserSandbox("nobody")
	if os.Geteuid() == 0 {
		if err != nil {
			t.Fatalf("root probe for nobody failed: %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("non-root probe should demand root, got %v", err)
	}
}

// TestUserSandboxRunsAsTargetAccount is the real privilege drop. It needs root,
// so the backend CI job runs it through sudo; a local non-root run skips it.
func TestUserSandboxRunsAsTargetAccount(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root; CI runs this test with sudo")
	}
	account, err := user.Lookup("nobody")
	if err != nil {
		t.Fatalf("look up nobody: %v", err)
	}
	dir := t.TempDir()
	sandbox := Sandbox{Mode: SandboxUser, User: "nobody"}
	if err = sandbox.prepareUserWorkspace(dir); err != nil {
		t.Fatal(err)
	}
	cmd, err := sandbox.commandForBuild(context.Background(), "n", dir, dir, "id -u")
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run as nobody: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != account.Uid {
		t.Fatalf("build ran as uid %q, want %q", got, account.Uid)
	}
}
