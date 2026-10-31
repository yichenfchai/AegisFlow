package builds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	// cloneOutputLimit bounds the clone output kept for a failure summary; a
	// verbose git error must not flood the log table.
	cloneOutputLimit = 8 << 10
	// maxCloneLines caps the lines written for a failed clone.
	maxCloneLines = 12
)

// WithSource configures repository cloning. cloneEnabled is the AEGIS_BUILD_CLONE
// gray switch and allowLocalRepo is AEGIS_BUILD_ALLOW_LOCAL_REPO; both default
// to false so an existing deployment never starts cloning by surprise.
func (s *Service) WithSource(cloneEnabled, allowLocalRepo bool) *Service {
	s.cloneEnabled = cloneEnabled
	s.allowLocalRepo = allowLocalRepo
	return s
}

// WithDiskLimit caps the total workspace size per build; zero disables the
// check (tests that do not care about disk behaviour keep the old path).
func (s *Service) WithDiskLimit(limit int64) *Service {
	s.diskLimit = limit
	return s
}

// localRepoURL mirrors project validation: file:/// URLs, absolute POSIX paths
// and Windows drive paths all point at the host filesystem.
func localRepoURL(repoURL string) bool {
	if strings.HasPrefix(repoURL, "file://") || strings.HasPrefix(repoURL, "/") {
		return true
	}
	return len(repoURL) >= 3 && repoURL[1] == ':' && (repoURL[2] == '\\' || repoURL[2] == '/')
}

// displayRepoURL strips embedded credentials before a URL reaches a log line.
func displayRepoURL(repoURL string) string {
	parsed, err := url.Parse(repoURL)
	if err != nil || parsed.User == nil {
		return repoURL
	}
	parsed.User = url.User("redacted")
	return parsed.String()
}

// cloneRepository performs a shallow, single-branch checkout into workDir/repo.
// It never prompts and never receives service credentials: this batch supports
// only credential-free repositories, so an authenticated URL fails fast instead
// of waiting for input. Local paths and file:/// URLs require the explicit
// operator opt-in.
func (s *Service) cloneRepository(ctx context.Context, repoURL, branch, workDir string) error {
	if strings.HasPrefix(repoURL, "https://") {
		if parsed, err := url.Parse(repoURL); err == nil && parsed.User != nil {
			return errors.New("repository URLs must not embed credentials; private repositories are not supported yet")
		}
	}
	if localRepoURL(repoURL) && !s.allowLocalRepo {
		return errors.New("local repository paths are disabled (set AEGIS_BUILD_ALLOW_LOCAL_REPO=true only on a trusted host)")
	}
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git is required to clone repositories: %w", err)
	}
	args := []string{"clone", "--depth", "1", "--single-branch", "--no-tags", "--quiet"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	dest := filepath.Join(workDir, "repo")
	args = append(args, "--", repoURL, dest)
	cmd, err := s.sandbox.toolCommand(ctx, "git", args, workDir)
	if err != nil {
		return err
	}
	output := &limitedBuffer{limit: cloneOutputLimit}
	cmd.Stdout = output
	cmd.Stderr = output
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, summarizeCloneOutput(output.String()))
	}
	return nil
}

// summarizeCloneOutput keeps the tail of git's stderr, redacts every kept line
// and drops the ones that still match a sensitive pattern (fail closed). Only
// the summary reaches the log table, never the raw buffer.
func summarizeCloneOutput(raw string) string {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) > maxCloneLines {
		lines = lines[len(lines)-maxCloneLines:]
	}
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		redacted, _, ok := redactOutputLine(line)
		if !ok {
			continue
		}
		kept = append(kept, redacted)
	}
	if len(kept) == 0 {
		return "no output"
	}
	return strings.Join(kept, "; ")
}

// limitedBuffer keeps at most limit bytes and reports truncation, so a runaway
// clone cannot grow memory without bound.
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) <= remaining {
			b.buf.Write(p)
		} else {
			b.buf.Write(p[:remaining])
			b.truncated = true
		}
	} else {
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	if b.truncated {
		return b.buf.String() + "\n…[truncated]"
	}
	return b.buf.String()
}
