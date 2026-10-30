//go:build !linux

package builds

import "errors"

// ProbeUserSandbox reports that the mode is Linux-only instead of accepting a
// configuration the executor cannot honour on this platform.
func ProbeUserSandbox(string) error {
	return errors.New("AEGIS_BUILD_SANDBOX=user is supported on Linux only; use container or none")
}
