package builds

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// SandboxMode mirrors the AEGIS_BUILD_SANDBOX switch from
// docs/build-execution-design.md.
type SandboxMode string

const (
	// SandboxNone runs the command as the service account. It exists for
	// development only and the server warns about it at startup.
	SandboxNone SandboxMode = "none"
	// SandboxUser runs each build as a dedicated OS account. The service must
	// start as root so it can hand the workspace over and drop privileges;
	// startup fails closed otherwise.
	SandboxUser SandboxMode = "user"
	// SandboxContainer runs each build in a one-shot container with its own
	// network namespace and hard CPU/memory/process limits.
	SandboxContainer SandboxMode = "container"
)

// Sandbox is the resolved execution policy. Zero values mean "none".
type Sandbox struct {
	Mode   SandboxMode
	Image  string
	CPU    string
	Memory string
	Pids   int
	// User is the OS account name for SandboxUser.
	User string
}

// WithSandbox replaces the execution policy; callers wire it from config.
func (s *Service) WithSandbox(sandbox Sandbox) *Service {
	s.sandbox = sandbox
	return s
}

// commandForBuild returns the process to start for one build. workDir is the
// whole build workspace (mounted into a container) and execDir is where the
// command runs: the workspace itself, or its repo/ subdirectory after a clone.
//
// The container variant mounts only the build directory, with no network, no
// capabilities, a read-only root filesystem and runtime-enforced resource
// limits. The user variant wraps the shell in setpriv so the command cannot
// touch service-owned files.
func (s Sandbox) commandForBuild(ctx context.Context, containerName, workDir, execDir, command string) (*exec.Cmd, error) {
	switch s.Mode {
	case "", SandboxNone:
		return shellCommand(ctx, command, execDir), nil
	case SandboxUser:
		return userCommand(ctx, s.User, command, execDir)
	case SandboxContainer:
		if s.Image == "" || s.CPU == "" || s.Memory == "" || s.Pids < 1 {
			return nil, errors.New("container sandbox is missing its image or resource limits")
		}
		if _, err := exec.LookPath("docker"); err != nil {
			return nil, fmt.Errorf("container sandbox requires the docker CLI: %w", err)
		}
		inside := "/workspace"
		if rel, err := filepath.Rel(workDir, execDir); err == nil && rel != "." {
			inside = "/workspace/" + filepath.ToSlash(rel)
		}
		cmd := exec.CommandContext(ctx, "docker",
			"run", "--rm", "--name", containerName,
			"--network", "none",
			"--cpus", s.CPU,
			"--memory", s.Memory,
			"--pids-limit", strconv.Itoa(s.Pids),
			"--cap-drop", "ALL",
			"--security-opt", "no-new-privileges",
			"--read-only",
			"--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
			"-v", workDir+":/workspace",
			"-w", inside,
			"-e", "HOME=/workspace",
			"-e", "TMPDIR=/tmp",
			s.Image,
			"sh", "-c", command,
		)
		cmd.Dir = workDir
		cmd.Env = dockerClientEnv()
		return cmd, nil
	default:
		return nil, fmt.Errorf("unsupported build sandbox mode %q", s.Mode)
	}
}

// toolCommand runs a host-side helper (git clone) under the same identity the
// build command will use, so a cloned repository is owned by the build account.
// Container mode deliberately clones on the host: the container has no network,
// and the checkout is handed to it through the workspace mount.
func (s Sandbox) toolCommand(ctx context.Context, name string, args []string, dir string) (*exec.Cmd, error) {
	switch s.Mode {
	case SandboxUser:
		if runtime.GOOS != "linux" {
			return nil, errors.New("user sandbox mode is supported on Linux only")
		}
		account, err := lookupSandboxUser(s.User)
		if err != nil {
			return nil, err
		}
		wrapped := append([]string{"--reuid", account.Uid, "--regid", account.Gid, "--clear-groups", name}, args...)
		cmd := exec.CommandContext(ctx, "setpriv", wrapped...)
		cmd.Dir = dir
		cmd.Env = toolEnv(dir)
		return cmd, nil
	default:
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Env = toolEnv(dir)
		return cmd, nil
	}
}

// prepareUserWorkspace hands the fresh 0700 workspace to the build account.
// Only root can chown, which is exactly the privilege user mode already
// requires; anything else fails closed before a command starts.
func (s Sandbox) prepareUserWorkspace(dir string) error {
	if runtime.GOOS != "linux" {
		return errors.New("user sandbox mode is supported on Linux only")
	}
	account, err := lookupSandboxUser(s.User)
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return fmt.Errorf("build user %q has a non-numeric uid", s.User)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return fmt.Errorf("build user %q has a non-numeric gid", s.User)
	}
	if err = os.Chown(dir, uid, gid); err != nil {
		return fmt.Errorf("hand the workspace to %q (the service must run as root): %w", s.User, err)
	}
	return nil
}

func lookupSandboxUser(name string) (*user.User, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("user sandbox mode requires AEGIS_BUILD_USER")
	}
	account, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("look up build user %q: %w", name, err)
	}
	return account, nil
}

// userCommand runs the build shell through setpriv: target uid/gid, no
// supplementary groups, and the same environment allowlist as the other modes.
func userCommand(ctx context.Context, name, command, dir string) (*exec.Cmd, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("user sandbox mode is supported on Linux only; use container or none")
	}
	account, err := lookupSandboxUser(name)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "setpriv",
		"--reuid", account.Uid, "--regid", account.Gid, "--clear-groups",
		"/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = buildCommandEnv(dir)
	return cmd, nil
}

// stop terminates a container that outlived its docker CLI client. Killing the
// attached client (timeout, cancel, shutdown) does not stop the container, so
// the executor calls this after any abnormal exit.
func (s Sandbox) stop(containerName string) {
	if s.Mode != SandboxContainer || containerName == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", "kill", containerName).Run()
}

// shellCommand is the unisolated path: the system shell running as the service
// account with the environment allowlist.
func shellCommand(ctx context.Context, command, workDir string) *exec.Cmd {
	shell, flag := "sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/c"
	}
	cmd := exec.CommandContext(ctx, shell, flag, command)
	cmd.Dir = workDir
	cmd.Env = buildCommandEnv(workDir)
	return cmd
}

// toolEnv extends the allowlist with non-interactive Git settings: a clone must
// fail instead of prompting for credentials, because this batch deliberately
// supports only credential-free repositories.
func toolEnv(dir string) []string {
	env := buildCommandEnv(dir)
	return append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes",
	)
}

// dockerClientEnv passes the operator's Docker configuration (host, TLS,
// config dir) to the CLI, but never the service's own credentials: an image
// entrypoint must not be able to infer AEGIS_* values from its parent process.
func dockerClientEnv() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "AEGIS_") {
			continue
		}
		env = append(env, entry)
	}
	return env
}

// ProbeContainerRuntime fails closed when the sandbox mode is enabled but no
// usable container runtime is present. Called during startup.
func ProbeContainerRuntime(ctx context.Context) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker CLI not found; install Docker or use AEGIS_BUILD_SANDBOX=none in development: %w", err)
	}
	out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(out))
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("docker runtime is not reachable: %s", message)
	}
	return nil
}
