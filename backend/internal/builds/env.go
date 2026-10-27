package builds

import (
	"os"
	"runtime"
)

// buildCommandEnv is the environment a build command runs with. It is an
// explicit allowlist: the service environment holds AEGIS_* credentials and a
// project-configured command must not be able to read them. HOME and the temp
// variables point at the build working directory so tools keep their state
// inside it.
func buildCommandEnv(workDir string) []string {
	env := []string{
		"PATH=" + pathWithFallback(),
		"HOME=" + workDir,
		"TMPDIR=" + workDir,
		"TMP=" + workDir,
		"TEMP=" + workDir,
	}
	for _, key := range []string{"LANG", "LC_ALL", "LC_CTYPE"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	if runtime.GOOS == "windows" {
		// cmd.exe and the programs it starts need these to resolve builtins and
		// DLLs; none of them carries application credentials. The user-scoped
		// variables point into the build directory for the same reason as HOME.
		for _, key := range []string{"SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT"} {
			if value := os.Getenv(key); value != "" {
				env = append(env, key+"="+value)
			}
		}
		env = append(env, "USERPROFILE="+workDir, "APPDATA="+workDir, "LOCALAPPDATA="+workDir)
	}
	return env
}

func pathWithFallback() string {
	if value := os.Getenv("PATH"); value != "" {
		return value
	}
	if runtime.GOOS == "windows" {
		return `C:\Windows\System32;C:\Windows`
	}
	return "/usr/local/bin:/usr/bin:/bin"
}

// newBuildWorkDir creates the per-build directory with an explicit owner-only
// mode instead of relying on the process umask.
func newBuildWorkDir() (string, error) {
	dir, err := os.MkdirTemp("", "aegis-build-*")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}
