package builds

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/dbmigrate"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

var testActor = auth.Actor{ID: "actor-000000000000001", Username: "tester", RequestID: "req-builds"}

func testService(t *testing.T) (*Service, *projects.Service) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	if err := dbmigrate.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	registry := projects.New(db)
	return New(db, registry), registry
}

// startWorker runs the worker loop directly; lease behaviour is covered by
// lease_test.go, so these execution tests do not contend for the lease.
func startWorker(t *testing.T, s *Service) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go s.run(ctx)
	t.Cleanup(cancel)
	return cancel
}

// sleepCommand is a portable way to keep a build busy.
func sleepCommand(seconds int) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("ping -n %d 127.0.0.1 >nul", seconds+1)
	}
	return fmt.Sprintf("sleep %d", seconds)
}

// floodCommand prints numbered lines. A fast, chatty process is the case where
// reaping it before draining its pipes used to lose buffered output.
func floodCommand(lines int) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("for /l %%i in (1,1,%d) do @echo line%%i", lines)
	}
	return fmt.Sprintf("for i in $(seq 1 %d); do echo line$i; done", lines)
}

func createProject(t *testing.T, registry *projects.Service, name, command string) *projects.Project {
	t.Helper()
	project, err := registry.Create(context.Background(), testActor, projects.Input{Name: name, BuildCommand: command, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func waitForStatus(t *testing.T, s *Service, id string, want ...string) *Build {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		build, err := s.Get(context.Background(), testActor, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, status := range want {
			if build.Status == status {
				return build
			}
		}
		if build.Status == StatusSucceeded || build.Status == StatusFailed || build.Status == StatusCancelled || build.Status == StatusInterrupted {
			t.Fatalf("build settled in %s, want %v", build.Status, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %v, status=%s", want, build.Status)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestTriggerValidationAndSequence(t *testing.T) {
	s, registry := testService(t)
	ctx := context.Background()
	project := createProject(t, registry, "seq-project", "echo ok")
	other, err := registry.Create(ctx, testActor, projects.Input{Name: "other-project"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Trigger(ctx, testActor, "missing-0000000000000001", ""); err != ErrNotFound {
		t.Fatalf("unknown project error = %v", err)
	}
	if _, err = s.Trigger(ctx, testActor, other.ID, ""); err == nil || !strings.Contains(err.Error(), "no build command") {
		t.Fatalf("missing command error = %v", err)
	}
	if _, err = registry.Update(ctx, testActor, project.ID, projects.Patch{Archived: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Trigger(ctx, testActor, project.ID, ""); err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("archived project error = %v", err)
	}
	if _, err = registry.Update(ctx, testActor, project.ID, projects.Patch{Archived: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	for want := int64(1); want <= 3; want++ {
		build, err := s.Trigger(ctx, testActor, project.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if build.BuildNumber != want || build.Status != StatusQueued {
			t.Fatalf("build #%d = %+v", want, build)
		}
		if want == 1 && build.Branch != "main" {
			t.Fatalf("branch default = %q", build.Branch)
		}
	}
	if _, err = s.Trigger(ctx, testActor, project.ID, "feature/x"); err != nil {
		t.Fatal(err)
	}
}

func boolPtr(value bool) *bool { return &value }

func TestWorkerSuccessAndLogs(t *testing.T) {
	s, registry := testService(t)
	startWorker(t, s)
	project := createProject(t, registry, "echo-project", "echo hello-aegis")
	build, err := s.Trigger(context.Background(), testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	done := waitForStatus(t, s, build.ID, StatusSucceeded)
	if done.StartedAt < done.CreatedAt || done.FinishedAt < done.StartedAt {
		t.Fatalf("bad timestamps: %+v", done)
	}
	joined := waitForLog(t, s, build.ID, "hello-aegis")
	for _, want := range []string{"build succeeded", "build #1 started"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("logs missing %q: %s", want, joined)
		}
	}
	// Cursor paging: everything after the first event still contains output.
	events, err := s.Logs(context.Background(), testActor, build.ID, 0, 1000)
	if err != nil || len(events) < 3 {
		t.Fatalf("expected system+stdout+system events: %+v err=%v", events, err)
	}
	rest, err := s.Logs(context.Background(), testActor, build.ID, events[0].ID, 1000)
	if err != nil || len(rest) != len(events)-1 {
		t.Fatalf("cursor page = %d events, err %v", len(rest), err)
	}
}

func logText(events []LogEvent) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString(e.Message)
		b.WriteString("\n")
	}
	return b.String()
}

// waitForLog polls until the substring lands; the worker flushes logs after
// the terminal status update, so an immediate read races the final flush.
func waitForLog(t *testing.T, s *Service, id, want string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		events, err := s.Logs(context.Background(), testActor, id, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		text := logText(events)
		if strings.Contains(text, want) {
			return text
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing %q in logs: %s", want, text)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestWorkerCapturesChattyOutput(t *testing.T) {
	s, registry := testService(t)
	startWorker(t, s)
	// Stay below the 1000-event log page so one read sees the last line.
	project := createProject(t, registry, "flood-project", floodCommand(900))
	build, err := s.Trigger(context.Background(), testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, build.ID, StatusSucceeded)
	text := waitForLog(t, s, build.ID, "line900")
	if !strings.Contains(text, "\nline1\n") {
		t.Fatalf("first output line missing: %s", text[:min(len(text), 200)])
	}
}

func TestWorkerFailureExitCode(t *testing.T) {
	s, registry := testService(t)
	startWorker(t, s)
	project := createProject(t, registry, "fail-project", "exit 3")
	build, err := s.Trigger(context.Background(), testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, build.ID, StatusFailed)
	waitForLog(t, s, build.ID, "build failed")
}

func TestWorkerTimeout(t *testing.T) {
	db := testutil.OpenPostgres(t)
	if err := dbmigrate.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	registry := projects.New(db)
	s := newWithLimits(db, registry, 700*time.Millisecond, maxLogEvents)
	startWorker(t, s)
	project := createProject(t, registry, "slow-project", sleepCommand(10))
	build, err := s.Trigger(context.Background(), testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, build.ID, StatusFailed)
	waitForLog(t, s, build.ID, "timed out")
}

func TestCancelRunningBuild(t *testing.T) {
	s, registry := testService(t)
	startWorker(t, s)
	project := createProject(t, registry, "cancel-project", sleepCommand(30))
	build, err := s.Trigger(context.Background(), testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, build.ID, StatusRunning)
	cancelled, err := s.Cancel(context.Background(), testActor, build.ID)
	if err != nil || cancelled.Status != StatusCancelled {
		t.Fatalf("cancel = %+v err=%v", cancelled, err)
	}
	waitForStatus(t, s, build.ID, StatusCancelled)
	waitForLog(t, s, build.ID, "cancelled")
	// Cancelling a terminal build is rejected; repeating is idempotent.
	again, err := s.Cancel(context.Background(), testActor, build.ID)
	if err != nil || again.Status != StatusCancelled {
		t.Fatalf("idempotent cancel = %+v err=%v", again, err)
	}
	succeeded := createProject(t, registry, "quick-project", "echo fast")
	quick, err := s.Trigger(context.Background(), testActor, succeeded.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, quick.ID, StatusSucceeded)
	if _, err = s.Cancel(context.Background(), testActor, quick.ID); err == nil || !strings.Contains(err.Error(), "queued or running") {
		t.Fatalf("cancel finished build error = %v", err)
	}
}

func TestRecoverInterruptedAndList(t *testing.T) {
	s, registry := testService(t)
	ctx := context.Background()
	project := createProject(t, registry, "recover-project", "echo x")
	build, err := s.Trigger(ctx, testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-execution: running row without a worker.
	list, total, err := s.List(ctx, testActor, "", 20, 0)
	if err != nil || total != 1 || len(list) != 1 || list[0].Status != StatusQueued {
		t.Fatalf("list = %+v total=%d err=%v", list, total, err)
	}
	if _, err = s.db.Exec(`UPDATE builds SET status=$1 WHERE id=$2`, StatusRunning, build.ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.RecoverInterrupted(ctx)
	if err != nil || recovered != 1 {
		t.Fatalf("recovered=%d err=%v", recovered, err)
	}
	got, err := s.Get(ctx, testActor, build.ID)
	if err != nil || got.Status != StatusInterrupted {
		t.Fatalf("after recovery = %+v err=%v", got, err)
	}
	if list, total, err = s.List(ctx, testActor, project.ID, 20, 0); err != nil || total != 1 || list[0].Status != StatusInterrupted {
		t.Fatalf("project filter = %+v err=%v", list, err)
	}
}

// outsiderActor is a global member who is not a member of the private project.
var outsiderActor = auth.Actor{ID: "outsider-0000000000001", Username: "otto", Role: "member", RequestID: "req-outsider"}

// adminActor bypasses project visibility entirely.
var adminActor = auth.Actor{ID: "admin-0000000000000001", Username: "root", Role: "admin", RequestID: "req-admin"}

func TestBuildReadsFollowProjectVisibility(t *testing.T) {
	s, registry := testService(t)
	startWorker(t, s)
	ctx := context.Background()
	project := createProject(t, registry, "private-builds", "echo secret-output")
	build, err := s.Trigger(ctx, testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, build.ID, StatusSucceeded)

	private := projects.VisibilityPrivate
	if _, err = registry.Update(ctx, testActor, project.ID, projects.Patch{Visibility: &private}); err != nil {
		t.Fatal(err)
	}
	// An outsider sees neither the build nor its logs, and cannot touch it.
	if list, total, err := s.List(ctx, outsiderActor, "", 20, 0); err != nil || total != 0 || len(list) != 0 {
		t.Fatalf("outsider list = %+v total=%d err=%v", list, total, err)
	}
	if _, err := s.Get(ctx, outsiderActor, build.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider get error = %v", err)
	}
	if _, err := s.Logs(ctx, outsiderActor, build.ID, 0, 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider logs error = %v", err)
	}
	if _, err := s.Trigger(ctx, outsiderActor, project.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider trigger error = %v", err)
	}
	if _, err := s.Cancel(ctx, outsiderActor, build.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider cancel error = %v", err)
	}
	// The creator still reads everything, including the log stream.
	if _, err := s.Get(ctx, testActor, build.ID); err != nil {
		t.Fatal(err)
	}
	events, err := s.Logs(ctx, testActor, build.ID, 0, 1000)
	if err != nil || len(events) == 0 {
		t.Fatalf("creator logs = %d err=%v", len(events), err)
	}
	if list, total, err := s.List(ctx, testActor, "", 20, 0); err != nil || total != 1 || list[0].ID != build.ID {
		t.Fatalf("creator list = %+v total=%d err=%v", list, total, err)
	}
	// A global admin is never filtered.
	if _, err := s.Get(ctx, adminActor, build.ID); err != nil {
		t.Fatalf("admin get err=%v", err)
	}
	if list, total, err := s.List(ctx, adminActor, "", 20, 0); err != nil || total != 1 || len(list) != 1 {
		t.Fatalf("admin list = %+v total=%d err=%v", list, total, err)
	}
}

// secretCommand prints three credential shapes on one line: an environment
// style assignment, a bearer header and a database URL with a password.
func secretCommand() string {
	return `echo AEGIS_JWT_SECRET=super-secret-value Bearer abcdefghijklmnop postgres://aegis:hunter2@127.0.0.1:5432/aegisflow`
}

func TestBuildOutputIsRedacted(t *testing.T) {
	s, registry := testService(t)
	startWorker(t, s)
	project := createProject(t, registry, "redaction-project", secretCommand())
	build, err := s.Trigger(context.Background(), testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, build.ID, StatusSucceeded)
	waitForLog(t, s, build.ID, "[redacted]")
	// The tally is written when the writer closes, which can trail the terminal
	// status by one flush, so wait for it instead of reading once.
	joined := waitForLog(t, s, build.ID, "sensitive value(s)")
	for _, secret := range []string{"super-secret-value", "abcdefghijklmnop", "hunter2"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("build log leaked %q: %s", secret, joined)
		}
	}
}

func TestRetryEnqueuesANewBuild(t *testing.T) {
	s, registry := testService(t)
	startWorker(t, s)
	ctx := context.Background()
	project := createProject(t, registry, "retry-project", "echo retry-me")
	build, err := s.Trigger(ctx, testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, build.ID, StatusSucceeded)

	retried, err := s.Retry(ctx, testActor, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.BuildNumber != build.BuildNumber+1 || retried.Status != StatusQueued || retried.Branch != build.Branch {
		t.Fatalf("retry payload = %+v", retried)
	}
	waitForStatus(t, s, retried.ID, StatusSucceeded)

	// A retry runs the project's current command, not the original snapshot:
	// fixing the command and retrying is the point of the operation.
	changed := "echo changed-command"
	if _, err = registry.Update(ctx, testActor, project.ID, projects.Patch{BuildCommand: &changed}); err != nil {
		t.Fatal(err)
	}
	third, err := s.Retry(ctx, testActor, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if third.Command != changed {
		t.Fatalf("retry command = %q, want %q", third.Command, changed)
	}
	waitForStatus(t, s, third.ID, StatusSucceeded)

	// Only terminal builds can be retried.
	long := sleepCommand(30)
	if _, err = registry.Update(ctx, testActor, project.ID, projects.Patch{BuildCommand: &long}); err != nil {
		t.Fatal(err)
	}
	running, err := s.Trigger(ctx, testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, running.ID, StatusRunning)
	if _, err = s.Retry(ctx, testActor, running.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry of a running build error = %v", err)
	}
	if _, err = s.Cancel(ctx, testActor, running.ID); err != nil {
		t.Fatal(err)
	}

	if _, err = s.Retry(ctx, testActor, "missing-0000000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown build error = %v", err)
	}

	// Retries are audited with their provenance and never the command.
	var rows int
	var detail string
	if err = s.db.QueryRowContext(ctx, `SELECT count(*), coalesce(max(detail),'') FROM auth_audit WHERE action='build.retry'`).Scan(&rows, &detail); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("build.retry audit rows = %d", rows)
	}
	if !strings.Contains(detail, "retry_of=") {
		t.Fatalf("retry detail = %q", detail)
	}
	if strings.Contains(detail, "echo") {
		t.Fatalf("retry detail leaked the command: %q", detail)
	}
}
