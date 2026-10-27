package builds

import (
	"context"
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
		build, err := s.Get(context.Background(), id)
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
	events, err := s.Logs(context.Background(), build.ID, 0, 1000)
	if err != nil || len(events) < 3 {
		t.Fatalf("expected system+stdout+system events: %+v err=%v", events, err)
	}
	rest, err := s.Logs(context.Background(), build.ID, events[0].ID, 1000)
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
		events, err := s.Logs(context.Background(), id, 0, 1000)
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
	list, total, err := s.List(ctx, "", 20, 0)
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
	got, err := s.Get(ctx, build.ID)
	if err != nil || got.Status != StatusInterrupted {
		t.Fatalf("after recovery = %+v err=%v", got, err)
	}
	if list, total, err = s.List(ctx, project.ID, 20, 0); err != nil || total != 1 || list[0].Status != StatusInterrupted {
		t.Fatalf("project filter = %+v err=%v", list, err)
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