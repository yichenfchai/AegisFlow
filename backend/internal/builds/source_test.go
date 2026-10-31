package builds

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
)

// requireGit fails rather than skipping: clone behaviour is the point of these
// tests, and every supported CI image ships git.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git CLI is required for clone tests: %v", err)
	}
}

func gitRepoWithFile(t *testing.T, name, content string) string {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "-c", "user.name=aegis-test", "-c", "user.email=aegis@example.invalid", "commit", "-q", "-m", "init")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		t.Fatalf("read branch: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// fileURL builds the file:/// form the project validator accepts, on both
// Windows (file:///C:/...) and Unix (file:///tmp/...).
func fileURL(path string) string {
	return "file:///" + strings.TrimPrefix(filepath.ToSlash(path), "/")
}

func readFileCommand(name string) string {
	if runtime.GOOS == "windows" {
		return "type " + name
	}
	return "cat " + name
}

func TestLocalRepoURL(t *testing.T) {
	cases := map[string]bool{
		"https://github.com/a/b.git": false,
		"git@github.com:a/b.git":     false,
		"file:///srv/repos/app.git":  true,
		"/srv/repos/app":             true,
		`C:\repos\app`:               true,
		`C:/repos/app`:               true,
	}
	for input, want := range cases {
		if got := localRepoURL(input); got != want {
			t.Errorf("localRepoURL(%q)=%v want %v", input, got, want)
		}
	}
}

func TestDisplayRepoURLStripsCredentials(t *testing.T) {
	display := displayRepoURL("https://user:secret@example.com/a.git")
	if strings.Contains(display, "secret") || !strings.Contains(display, "redacted") {
		t.Fatalf("credentials survived in %q", display)
	}
	if got := displayRepoURL("https://example.com/a.git"); got != "https://example.com/a.git" {
		t.Fatalf("plain URL changed: %q", got)
	}
}

func TestSummarizeCloneOutputRedacts(t *testing.T) {
	raw := "Cloning into 'repo'...\n" +
		"fatal: could not read Username\n" +
		"warning: postgres://user:pass@db/app is not reachable\n"
	summary := summarizeCloneOutput(raw)
	if strings.Contains(summary, "pass") {
		t.Fatalf("summarizeCloneOutput leaked: %s", summary)
	}
	if !strings.Contains(summary, "fatal: could not read Username") {
		t.Fatalf("ordinary line dropped: %s", summary)
	}
	// A value that survives redaction drops the whole line (fail closed).
	if strings.Contains(summarizeCloneOutput("token ghp_abcdefghijklmnopqrstuvwxyz0123456789"), "ghp_") {
		t.Fatal("token survived summarization")
	}
}

func TestLimitedBufferTruncates(t *testing.T) {
	b := &limitedBuffer{limit: 4}
	if _, err := b.Write([]byte("abcdefgh")); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); !strings.HasPrefix(got, "abcd") || !strings.Contains(got, "truncated") {
		t.Fatalf("buffer=%q", got)
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{512: "512 B", 512 << 10: "512.0 KiB", 2 << 30: "2.0 GiB"} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d)=%q want %q", n, got, want)
		}
	}
}

func TestCloneLocalRepoRequiresOptIn(t *testing.T) {
	repo := gitRepoWithFile(t, "marker.txt", "repo-content")
	work := t.TempDir()
	service := &Service{}
	if err := service.cloneRepository(context.Background(), fileURL(repo), "", work); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("local clone without opt-in: err=%v", err)
	}
	service.WithSource(true, true)
	if err := service.cloneRepository(context.Background(), fileURL(repo), "", work); err != nil {
		t.Fatalf("local clone with opt-in: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(work, "repo", "marker.txt"))
	if err != nil || string(data) != "repo-content" {
		t.Fatalf("clone content=%q err=%v", data, err)
	}
}

func TestCloneRejectsCredentialURL(t *testing.T) {
	service := (&Service{}).WithSource(true, true)
	err := service.cloneRepository(context.Background(), "https://user:pass@example.com/a.git", "", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("credential URL accepted: %v", err)
	}
}

// TestBuildClonesRepository is the ④c acceptance path: a real git repository is
// cloned and the command runs inside the checkout, not in an empty directory.
func TestBuildClonesRepository(t *testing.T) {
	repo := gitRepoWithFile(t, "marker.txt", "from-repository")
	branch := currentBranch(t, repo)
	service, registry := testService(t)
	service.WithSource(true, true)
	startWorker(t, service)
	ctx := context.Background()
	project, err := registry.Create(ctx, testActor, projects.Input{
		Name:          "clone-project",
		RepoURL:       fileURL(repo),
		DefaultBranch: branch,
		BuildCommand:  readFileCommand("marker.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	build, err := service.Trigger(ctx, testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if build.RepoURL != fileURL(repo) {
		t.Fatalf("build repoUrl snapshot = %q", build.RepoURL)
	}
	waitForStatus(t, service, build.ID, StatusSucceeded)
	logs, err := service.Logs(ctx, testActor, build.ID, 0, maxLogPageSize)
	if err != nil {
		t.Fatal(err)
	}
	output := joinLogs(logs)
	if !strings.Contains(output, "from-repository") {
		t.Fatalf("command did not run inside the clone:\n%s", output)
	}
	if !strings.Contains(output, "repository cloned") {
		t.Fatalf("missing clone system line:\n%s", output)
	}
}

func TestBuildCloneDisabledRunsEmptyWorkspace(t *testing.T) {
	service, registry := testService(t)
	startWorker(t, service)
	ctx := context.Background()
	project, err := registry.Create(ctx, testActor, projects.Input{Name: "no-clone-project", RepoURL: "https://example.com/a.git", DefaultBranch: "main", BuildCommand: "echo ok"})
	if err != nil {
		t.Fatal(err)
	}
	build, err := service.Trigger(ctx, testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, service, build.ID, StatusSucceeded)
	logs, err := service.Logs(ctx, testActor, build.ID, 0, maxLogPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(joinLogs(logs), "cloning is disabled") {
		t.Fatal("expected an explicit system line about cloning being disabled")
	}
}

func TestBuildCloneFailureFailsTheBuild(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	service, registry := testService(t)
	service.WithSource(true, true)
	startWorker(t, service)
	ctx := context.Background()
	project, err := registry.Create(ctx, testActor, projects.Input{Name: "clone-failure-project", RepoURL: fileURL(missing), DefaultBranch: "main", BuildCommand: "echo ok"})
	if err != nil {
		t.Fatal(err)
	}
	build, err := service.Trigger(ctx, testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, service, build.ID, StatusFailed)
	logs, err := service.Logs(ctx, testActor, build.ID, 0, maxLogPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(joinLogs(logs), "git clone failed") {
		t.Fatal("expected a clone failure system line")
	}
}

// TestBuildDiskLimitStopsBuild clones a repository larger than the cap and
// checks that the worker cancels the command and reports the limit.
func TestBuildDiskLimitStopsBuild(t *testing.T) {
	repo := gitRepoWithFile(t, "big.bin", strings.Repeat("x", 2<<20))
	service, registry := testService(t)
	service.WithSource(true, true).WithDiskLimit(256 << 10)
	startWorker(t, service)
	ctx := context.Background()
	project, err := registry.Create(ctx, testActor, projects.Input{Name: "disk-limit-project", RepoURL: fileURL(repo), DefaultBranch: currentBranch(t, repo), BuildCommand: sleepCommand(20)})
	if err != nil {
		t.Fatal(err)
	}
	build, err := service.Trigger(ctx, testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, service, build.ID, StatusFailed)
	logs, err := service.Logs(ctx, testActor, build.ID, 0, maxLogPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(joinLogs(logs), "disk limit") {
		t.Fatal("expected a disk limit system line")
	}
}

func TestBuildSnapshotsRepoURL(t *testing.T) {
	service, registry := testService(t)
	ctx := context.Background()
	project, err := registry.Create(ctx, testActor, projects.Input{Name: "snapshot-project", RepoURL: "https://example.com/first.git", DefaultBranch: "main", BuildCommand: "echo ok"})
	if err != nil {
		t.Fatal(err)
	}
	build, err := service.Trigger(ctx, testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	second := "https://example.com/second.git"
	if _, err = registry.Update(ctx, testActor, project.ID, projects.Patch{RepoURL: &second}); err != nil {
		t.Fatal(err)
	}
	stored, err := service.Get(ctx, testActor, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RepoURL != "https://example.com/first.git" {
		t.Fatalf("snapshot changed after project update: %q", stored.RepoURL)
	}
}

func joinLogs(events []LogEvent) string {
	var joined strings.Builder
	for _, event := range events {
		joined.WriteString(event.Message)
		joined.WriteString("\n")
	}
	return joined.String()
}
