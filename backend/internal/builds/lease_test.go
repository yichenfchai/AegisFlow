package builds

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/dbmigrate"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// testTwoInstances builds two independent pools over one schema, the way two
// replicas of the same deployment see the same database but different sessions.
func testTwoInstances(t *testing.T) (*Service, *Service, *projects.Service) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	if err := dbmigrate.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	registry := projects.New(db)
	first := New(db, registry)
	second := New(testutil.ClonePostgres(t, db), registry)
	second.leaseRetry = 25 * time.Millisecond
	return first, second, registry
}

// logRecorder captures worker log messages so standby behaviour is observable
// instead of inferred from timing.
type logRecorder struct {
	mu       sync.Mutex
	messages []string
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, record slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, record.Message)
	return nil
}

func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *logRecorder) WithGroup(string) slog.Handler      { return r }

func (r *logRecorder) waitFor(t *testing.T, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		r.mu.Lock()
		found := false
		for _, message := range r.messages {
			if strings.Contains(message, substr) {
				found = true
				break
			}
		}
		r.mu.Unlock()
		if found {
			return
		}
		if time.Now().After(deadline) {
			r.mu.Lock()
			joined := strings.Join(r.messages, " | ")
			r.mu.Unlock()
			t.Fatalf("log %q not seen within %v; messages: %s", substr, timeout, joined)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWorkerLeaseIsExclusiveAcrossInstances(t *testing.T) {
	first, second, _ := testTwoInstances(t)
	ctx := context.Background()

	lease, held, err := first.AcquireWorkerLease(ctx)
	if err != nil || !held {
		t.Fatalf("first acquire: held=%v err=%v", held, err)
	}
	if other, held, err := second.AcquireWorkerLease(ctx); err != nil || held {
		if other != nil {
			other.Release()
		}
		t.Fatalf("second acquire while held: held=%v err=%v", held, err)
	}

	lease.Release()
	next, held, err := second.AcquireWorkerLease(ctx)
	if err != nil || !held {
		t.Fatalf("acquire after release: held=%v err=%v", held, err)
	}
	next.Release()

	again, held, err := first.AcquireWorkerLease(ctx)
	if err != nil || !held {
		t.Fatalf("re-acquire after release: held=%v err=%v", held, err)
	}
	again.Release()
}

func TestStandbyDoesNotRecoverAnotherInstancesBuild(t *testing.T) {
	first, second, registry := testTwoInstances(t)
	ctx := context.Background()
	project := createProject(t, registry, "lease-project", sleepCommand(60))
	build, err := first.Trigger(ctx, testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the first instance executing the build when the second starts.
	if _, err := first.db.Exec(`UPDATE builds SET status=$1 WHERE id=$2`, StatusRunning, build.ID); err != nil {
		t.Fatal(err)
	}
	lease, held, err := first.AcquireWorkerLease(ctx)
	if err != nil || !held {
		t.Fatalf("holder acquire: held=%v err=%v", held, err)
	}
	defer lease.Release()

	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	recorder := &logRecorder{}
	go second.RunWorker(workerCtx, slog.New(recorder))
	recorder.waitFor(t, "stand-by", 5*time.Second)

	if got, err := first.Get(ctx, testActor, build.ID); err != nil || got.Status != StatusRunning {
		t.Fatalf("standby touched a running build: %+v err=%v", got, err)
	}

	// When the holder stops, the standby takes over and only then recovers.
	lease.Release()
	waitForStatus(t, first, build.ID, StatusInterrupted)
	recorder.waitFor(t, "interrupted builds recovered", 5*time.Second)
}
