//go:build linux

package builds

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ProbeUserSandbox fails closed unless the host can actually drop privileges:
// Linux, a root service, a dedicated non-root account, and setpriv from
// util-linux.
func ProbeUserSandbox(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("AEGIS_BUILD_SANDBOX=user requires AEGIS_BUILD_USER")
	}
	if os.Geteuid() != 0 {
		return errors.New("AEGIS_BUILD_SANDBOX=user requires the service to start as root so it can drop privileges; use container or none")
	}
	if _, err := lookupSandboxUser(name); err != nil {
		return fmt.Errorf("AEGIS_BUILD_USER is invalid: %w", err)
	}
	if _, err := exec.LookPath("setpriv"); err != nil {
		return fmt.Errorf("the user sandbox requires setpriv from util-linux: %w", err)
	}
	return nil
}
