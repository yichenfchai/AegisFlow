//go:build windows

package builds

import (
	"context"
	"strings"
	"testing"
)

// TestUserSandboxRejectedOnWindows keeps the startup promise honest: the
// executor never pretends it can drop privileges on a platform where setpriv
// does not exist.
func TestUserSandboxRejectedOnWindows(t *testing.T) {
	if err := ProbeUserSandbox("nobody"); err == nil || !strings.Contains(err.Error(), "Linux only") {
		t.Fatalf("probe=%v", err)
	}
	_, err := (Sandbox{Mode: SandboxUser, User: "nobody"}).commandForBuild(context.Background(), "n", `C:\work`, `C:\work`, "echo hi")
	if err == nil || !strings.Contains(err.Error(), "Linux only") {
		t.Fatalf("command=%v", err)
	}
}
