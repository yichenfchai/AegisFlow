package builds

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
}

// WithSandbox replaces the execution policy; callers wire it from config.
func (s *Service) WithSandbox(sandbox Sandbox) *Service {
	s.sandbox = sandbox
	return s
}

// commandForBuild returns the process to start for one build. The container
// variant runs the command inside a disposable container: no network, no
// capabilities, a read-only root filesystem, and only the build directory
// mounted writable. Resource limits are enforced by the runtime, not by the
// command.
func (s Sandbox) commandForBuild(ctx context.Context, containerName, workDir, command string) (*exec.Cmd, error) {
	if s.Mode == "" || s.Mode == SandboxNone {
		return shellCommand(ctx, command, workDir), nil
	}
	if s.Mode != SandboxContainer {
		return nil, fmt.Errorf("unsupported build sandbox mode %q", s.Mode)
	}
	if s.Image == "" || s.CPU == "" || s.Memory == "" || s.Pids < 1 {
		return nil, errors.New("container sandbox is missing its image or resource limits")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("container sandbox requires the docker CLI: %w", err)
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
		"-w", "/workspace",
		"-e", "HOME=/workspace",
		"-e", "TMPDIR=/tmp",
		s.Image,
		"sh", "-c", command,
	)
	cmd.Dir = workDir
	cmd.Env = dockerClientEnv()
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
