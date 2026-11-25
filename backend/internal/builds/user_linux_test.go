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
// non-root process it refuses, and as root it accepts a real account and still
// rejects root as the build account.
func TestProbeUserSandbox(t *testing.T) {
	err := ProbeUserSandbox("nobody")
	if os.Geteuid() == 0 {
		if err != nil {
			t.Fatalf("root probe for nobody failed: %v", err)
		}
		if err = ProbeUserSandbox("root"); err == nil || !strings.Contains(err.Error(), "uid 0") {
			t.Fatalf("root probe for root = %v, want a uid 0 rejection", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("non-root probe should demand root, got %v", err)
	}
}

// TestUserSandboxRejectsRootBuildAccount is the P2-09 regression: setting
// AEGIS_BUILD_USER=root would silently undo the privilege drop, so the startup
// probe and every execution path must refuse a uid-0 account. The check runs
// before any chown, so it is asserted without needing root.
func TestUserSandboxRejectsRootBuildAccount(t *testing.T) {
	account, err := user.Lookup("root")
	if err != nil {
		t.Fatalf("look up root: %v", err)
	}
	if account.Uid != "0" {
		t.Fatalf("root uid = %q, want 0 on Linux", account.Uid)
	}
	if _, err = lookupSandboxUser("root"); err == nil || !strings.Contains(err.Error(), "uid 0") {
		t.Fatalf("lookup root = %v, want a uid 0 rejection", err)
	}
	sandbox := Sandbox{Mode: SandboxUser, User: "root"}
	if _, err = sandbox.commandForBuild(context.Background(), "n", "/tmp/work", "/tmp/work", "echo hi"); err == nil || !strings.Contains(err.Error(), "uid 0") {
		t.Fatalf("commandForBuild root = %v, want a uid 0 rejection", err)
	}
	if err = sandbox.prepareUserWorkspace(t.TempDir()); err == nil || !strings.Contains(err.Error(), "uid 0") {
		t.Fatalf("prepareUserWorkspace root = %v, want a uid 0 rejection", err)
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
