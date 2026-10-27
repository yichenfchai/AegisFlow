package builds

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

// run is the in-process worker loop: it waits for a wake signal (or a slow
// poll), claims queued builds one at a time with FOR UPDATE SKIP LOCKED, and
// executes them. Only the lease holder may call it (see RunWorker): a standby
// replica must never claim or recover another replica's builds. Cancel the
// context to stop; a running build is finalized as interrupted on shutdown.
func (s *Service) run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.notify:
		case <-ticker.C:
		}
		for {
			build, err := s.claimNext(ctx)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				if ctx.Err() == nil {
					time.Sleep(time.Second)
				}
				break
			}
			s.execute(ctx, build)
		}
	}
}

// claimNext atomically flips the oldest queued build to running. SKIP LOCKED
// means concurrent instances never take the same build.
func (s *Service) claimNext(ctx context.Context) (*Build, error) {
	row := s.db.QueryRowContext(ctx, `
	WITH next AS (SELECT id FROM builds WHERE status=$1 ORDER BY created_at,id LIMIT 1 FOR UPDATE SKIP LOCKED)
	UPDATE builds b SET status=$2, started_at=$3 FROM next WHERE b.id=next.id
	RETURNING b.id,b.project_id,b.build_number,b.status,b.branch,b.command,b.triggered_by,b.created_at,b.started_at,b.finished_at`, StatusQueued, StatusRunning, time.Now().Unix())
	build, err := scanBuild(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	return build, err
}

// execute runs one claimed build to a terminal state. System log lines record
// the lifecycle so the timeline survives even without process output.
func (s *Service) execute(ctx context.Context, build *Build) {
	execCtx, stop := context.WithTimeout(ctx, s.timeout)
	defer stop()

	workDir, err := newBuildWorkDir()
	if err != nil {
		s.finish(build, StatusFailed)
		return
	}
	defer os.RemoveAll(workDir)

	writer := newLogWriter(s, build.ID, s.maxEvents)
	defer writer.close()
	writer.systemf("build #%d started (branch=%q)", build.BuildNumber, build.Branch)

	shell, flag := "sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/c"
	}
	cmd := exec.CommandContext(execCtx, shell, flag, build.Command)
	cmd.Dir = workDir
	cmd.Env = buildCommandEnv(workDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.finish(build, StatusFailed)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.finish(build, StatusFailed)
		return
	}
	if err = cmd.Start(); err != nil {
		writer.systemf("start failed: %v", err)
		s.finish(build, StatusFailed)
		return
	}
	// Closing the parent read ends unblocks the stream readers. Cancelling a
	// shell command may leave grandchildren (on Windows `cmd /c ping` does)
	// holding the pipes open, so shutdown must not wait for them.
	var closeOnce sync.Once
	closeStreams := func() {
		closeOnce.Do(func() {
			_ = stdout.Close()
			_ = stderr.Close()
		})
	}
	ctxWatch := make(chan struct{})
	defer close(ctxWatch)
	go func() {
		select {
		case <-execCtx.Done():
			closeStreams()
		case <-ctxWatch:
		}
	}()
	// The watchdog terminates the process when the API cancels the build or
	// the server shuts down.
	watchCtx, stopWatch := context.WithCancel(context.Background())
	var watchWG sync.WaitGroup
	watchWG.Add(1)
	go func() {
		defer watchWG.Done()
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
				current, err := s.getByID(context.Background(), build.ID)
				if err != nil || current.Status != StatusRunning {
					_ = cmd.Process.Kill()
					closeStreams()
					return
				}
			}
		}
	}()

	var streamWG sync.WaitGroup
	streamWG.Add(2)
	go func() { defer streamWG.Done(); writer.stream(stdout, "stdout") }()
	go func() { defer streamWG.Done(); writer.stream(stderr, "stderr") }()
	// Drain both pipes before Wait: Wait closes the parent ends of the pipes,
	// so reaping first would drop output that a fast command already wrote.
	streamWG.Wait()
	waitErr := cmd.Wait()
	stopWatch()
	watchWG.Wait()

	switch {
	case execCtx.Err() == context.DeadlineExceeded:
		writer.systemf("build timed out after %s", s.timeout)
		s.finish(build, StatusFailed)
	case ctx.Err() != nil:
		// Server shutdown killed the command; the record must not look
		// healthy on the next boot.
		writer.systemf("interrupted by server shutdown")
		s.finish(build, StatusInterrupted)
	default:
		if current, err := s.getByID(context.Background(), build.ID); err == nil && current.Status == StatusCancelled {
			writer.systemf("build cancelled")
			return
		}
		if waitErr != nil {
			writer.systemf("build failed: %v", waitErr)
			s.finish(build, StatusFailed)
			return
		}
		writer.systemf("build succeeded")
		s.finish(build, StatusSucceeded)
	}
}

// finish writes the terminal state; it uses a fresh context so the final
// update lands even during shutdown races.
func (s *Service) finish(build *Build, status string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.db.ExecContext(ctx, `UPDATE builds SET status=$2, finished_at=$3 WHERE id=$1 AND status=$4`,
		build.ID, status, time.Now().Unix(), StatusRunning)
}

// logWriter batches output lines into the log table and caps the volume.
type logWriter struct {
	s         *Service
	buildID   string
	max       int
	count     int
	truncated bool
	done      chan struct{}
	mu        sync.Mutex
	pending   []LogEvent
}

func newLogWriter(s *Service, buildID string, max int) *logWriter {
	w := &logWriter{s: s, buildID: buildID, max: max, done: make(chan struct{})}
	go w.flushLoop()
	return w
}

func (w *logWriter) flushLoop() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-ticker.C:
			w.flush()
		}
	}
}

func (w *logWriter) stream(reader io.Reader, stream string) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes+1024)
	for scanner.Scan() {
		w.append(stream, scanner.Text())
	}
}

func (w *logWriter) systemf(format string, args ...any) {
	w.append("system", fmt.Sprintf(format, args...))
}

// append caps total events and per-line length; output past the cap drops.
func (w *logWriter) append(stream, message string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.count >= w.max {
		if !w.truncated {
			w.truncated = true
			w.count++
			w.pending = append(w.pending, LogEvent{BuildID: w.buildID, Stream: "system", Message: "log truncated: output past the event cap is dropped", CreatedAt: time.Now().Unix()})
		}
		return
	}
	if len(message) > maxLineBytes {
		message = truncateBytes(message, maxLineBytes)
	}
	w.count++
	w.pending = append(w.pending, LogEvent{BuildID: w.buildID, Stream: stream, Message: message, CreatedAt: time.Now().Unix()})
}

// flush persists the pending batch; failures are dropped rather than killing
// a running build for log-storage hiccups.
func (w *logWriter) flush() {
	w.mu.Lock()
	batch := w.pending
	w.pending = nil
	w.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	_ = w.s.appendEvents(batch)
}

// close stops the flush loop and flushes the tail.
func (w *logWriter) close() {
	close(w.done)
	w.flush()
}

func (s *Service) appendEvents(events []LogEvent) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range events {
		if _, err = tx.ExecContext(ctx, `INSERT INTO build_log_events(build_id,stream,message,created_at) VALUES($1,$2,$3,$4)`, e.BuildID, e.Stream, e.Message, e.CreatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// truncateBytes cuts at the byte limit on a rune boundary.
func truncateBytes(value string, limit int) string {
	cut := value[:limit]
	for len(cut) > 0 && cut[len(cut)-1]&0xC0 == 0x80 {
		cut = cut[:len(cut)-1]
	}
	return cut + " …[truncated]"
}
