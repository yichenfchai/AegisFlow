package builds

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
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
	RETURNING b.id,b.project_id,b.build_number,b.status,b.branch,b.command,b.repo_url,b.triggered_by,b.created_at,b.started_at,b.finished_at`, StatusQueued, StatusRunning, time.Now().Unix())
	build, err := scanBuild(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	return build, err
}

// execute runs one claimed build to a terminal state. System log lines record
// the lifecycle so the timeline survives even without process output.
func (s *Service) execute(ctx context.Context, build *Build) {
	execCtx, stopTimeout := context.WithTimeout(ctx, s.timeout)
	defer stopTimeout()

	workDir, err := newBuildWorkDir()
	if err != nil {
		s.finish(build, StatusFailed)
		return
	}
	defer func() {
		if err := os.RemoveAll(workDir); err != nil {
			slog.Default().Warn("build workspace cleanup failed", "build_id", build.ID, "path", workDir, "error", err)
		}
	}()

	writer := newLogWriter(s, build.ID, s.maxEvents)
	defer writer.close()
	writer.systemf("build #%d started (branch=%q)", build.BuildNumber, build.Branch)

	// runCtx ends on the wall-clock timeout, server shutdown, a cancelling
	// operator, or a disk-limit breach; the watchdog and clone share it.
	runCtx, cancelRun := context.WithCancel(execCtx)
	defer cancelRun()
	// diskStopped records the watchdog's verdict so the terminal path can log a
	// reason: a measured breach and an unverifiable workspace fail the build
	// for different reasons.
	var diskStopped atomic.Pointer[diskStopReason]
	if s.diskLimit > 0 {
		go watchDiskUsage(runCtx, workDir, s.diskLimit, func(reason diskStopReason) {
			diskStopped.Store(&reason)
			cancelRun()
		})
	}

	execDir := workDir
	// A cancel or shutdown must also interrupt a long clone: the command
	// watchdog below only starts once the shell has been launched.
	cloneWatchDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-cloneWatchDone:
				return
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if current, err := s.getByID(context.Background(), build.ID); err == nil && current.Status != StatusRunning {
					cancelRun()
					return
				}
			}
		}
	}()
	if s.sandbox.Mode == SandboxUser {
		if err = s.sandbox.prepareUserWorkspace(workDir); err != nil {
			writer.systemf("execution sandbox unavailable: %v", err)
			s.finish(build, StatusFailed)
			return
		}
	}
	if build.RepoURL != "" {
		if !s.cloneEnabled {
			writer.systemf("repository configured but cloning is disabled (AEGIS_BUILD_CLONE=false); running in an empty workspace")
		} else {
			writer.systemf("cloning %s (branch %q)", displayRepoURL(build.RepoURL), build.Branch)
			if err = s.cloneRepository(runCtx, build.RepoURL, build.Branch, workDir); err != nil {
				switch {
				case diskStopped.Load() != nil:
					writer.systemf("%s", diskStopMessage(diskStopped.Load(), s.diskLimit, " during clone"))
				case execCtx.Err() == context.DeadlineExceeded:
					writer.systemf("build timed out after %s during clone", s.timeout)
				case ctx.Err() != nil:
					writer.systemf("interrupted by server shutdown")
					s.finish(build, StatusInterrupted)
					return
				case s.cancelled(build.ID):
					writer.systemf("build cancelled")
					return
				default:
					writer.systemf("git clone failed: %v", err)
				}
				s.finish(build, StatusFailed)
				return
			}
			execDir = filepath.Join(workDir, "repo")
			writer.systemf("repository cloned; running the build command in repo/")
		}
	}

	containerName := "aegis-build-" + build.ID
	cmd, err := s.sandbox.commandForBuild(runCtx, containerName, workDir, execDir, build.Command)
	if err != nil {
		writer.systemf("execution sandbox unavailable: %v", err)
		s.finish(build, StatusFailed)
		return
	}
	// The command owns a fresh process group and a cancellation policy that
	// kills the whole group: a shell can background grandchildren that a plain
	// Process.Kill would orphan. WaitDelay bounds how long Wait keeps waiting
	// for output pipes inherited by a process it could not reap.
	startProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 10 * time.Second
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
	close(cloneWatchDone)
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
		case <-runCtx.Done():
			closeStreams()
		case <-ctxWatch:
		}
	}()
	// The watchdog terminates the process tree when the API cancels the build,
	// the execution deadline expires, the server shuts down or the disk limit
	// trips. A status read failure is not a cancellation: it is logged and the
	// next tick retries, so a transient database outage does not kill builds.
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
			case <-runCtx.Done():
				// Timeout, server shutdown, disk limit or a stop of the worker:
				// the command-side context cancellation also calls
				// cmd.Cancel; kill again so the group is addressed explicitly.
				_ = killProcessGroup(cmd)
				return
			case <-ticker.C:
				// Bound the read: a hung database must not wedge the watchdog
				// that also carries the cancel/timeout kills.
				readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
				current, err := s.getByID(readCtx, build.ID)
				readCancel()
				if err != nil {
					slog.Default().Error("build status watchdog read failed; retrying", "build_id", build.ID, "error", err)
					continue
				}
				if current.Status == StatusRunning {
					continue
				}
				_ = killProcessGroup(cmd)
				// Killing the attached docker client does not stop the
				// container, so remove it explicitly.
				s.sandbox.stop(containerName)
				closeStreams()
				return
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
	case diskStopped.Load() != nil:
		s.sandbox.stop(containerName)
		writer.systemf("%s", diskStopMessage(diskStopped.Load(), s.diskLimit, ""))
		s.finish(build, StatusFailed)
	case execCtx.Err() == context.DeadlineExceeded:
		s.sandbox.stop(containerName)
		writer.systemf("build timed out after %s", s.timeout)
		s.finish(build, StatusFailed)
	case ctx.Err() != nil:
		// Server shutdown killed the command; the record must not look
		// healthy on the next boot.
		s.sandbox.stop(containerName)
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
// update lands even during shutdown races. A failure is reported: an
// unrecovered running row is exactly what the next lease holder will mark
// interrupted, and silent loss of the terminal state hides that.
func (s *Service) finish(build *Build, status string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `UPDATE builds SET status=$2, finished_at=$3 WHERE id=$1 AND status=$4`,
		build.ID, status, time.Now().Unix(), StatusRunning); err != nil {
		slog.Default().Error("finalize build status failed", "build_id", build.ID, "status", status, "error", err)
	}
}

// cancelled reports whether an operator moved the build out of running while a
// clone was in flight; the status is authoritative, not the clone error.
func (s *Service) cancelled(id string) bool {
	current, err := s.getByID(context.Background(), id)
	return err == nil && current.Status == StatusCancelled
}

// logWriter batches output lines into the log table and caps the volume.
type logWriter struct {
	s         *Service
	buildID   string
	max       int
	count     int
	truncated bool
	// redacted counts credential-shaped values (and multi-line private-key
	// block lines) replaced in output; dropped counts lines withheld because a
	// matched value survived redaction.
	redacted int
	dropped  int
	done     chan struct{}
	mu       sync.Mutex
	pending  []LogEvent
	// pemOpen tracks, per stream, whether a PEM private-key block is open. The
	// block has no per-line credential shape, so every line until the matching
	// END marker must be withheld; per-stream state keeps one pipe's block from
	// swallowing the other pipe's output. Guarded by mu; nil means no block.
	pemOpen map[string]bool
}

// redactMessage applies the cross-line private-key state machine and then the
// single-line rules. It returns the message to store, the stream it belongs to
// (block lines become system events), the number of redacted values and the
// fail-closed flag. The caller must hold w.mu.
func (w *logWriter) redactMessage(stream, message string) (string, string, int, bool) {
	begin := privateKeyBeginPattern.MatchString(message)
	end := privateKeyEndPattern.MatchString(message)
	if w.pemOpen == nil {
		w.pemOpen = make(map[string]bool)
	}
	switch {
	case w.pemOpen[stream]:
		// Stay inside the block until the END marker; a missing END keeps
		// redacting to the end of the stream (fail closed).
		if end {
			delete(w.pemOpen, stream)
		}
		return pemRedactedMessage, "system", 1, true
	case begin:
		// A complete single-line block never opens the state, and a stray BEGIN
		// without END keeps the rest of that stream redacted.
		if !end {
			w.pemOpen[stream] = true
		}
		return pemRedactedMessage, "system", 1, true
	default:
		redacted, hits, ok := redactOutputLine(message)
		return redacted, stream, hits, ok
	}
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

// stream reads newline-delimited output from a build pipe. A line longer than
// maxLineBytes is truncated by append (on a rune boundary) and its remainder
// discarded, but reading continues to EOF: stopping at the first overlong line
// used to lose every later line and left the child blocked on a full pipe
// until the build timed out. ReadSlice keeps the consumed buffer bounded no
// matter how long a single line grows.
func (w *logWriter) stream(reader io.Reader, stream string) {
	buffered := bufio.NewReaderSize(reader, 32*1024)
	line := make([]byte, 0, 256)
	overlong := false
	appendChunk := func(chunk []byte) {
		if overlong || len(chunk) == 0 {
			return
		}
		// Keep one byte over the limit so append can tell the line was cut;
		// any further bytes of the same line are drained and dropped.
		room := maxLineBytes + 1 - len(line)
		if len(chunk) > room {
			line = append(line, chunk[:room]...)
			overlong = true
			return
		}
		line = append(line, chunk...)
	}
	emit := func(complete bool) {
		message := string(line)
		if complete {
			message = strings.TrimSuffix(message, "\n")
		}
		// bufio.ScanLines also dropped a trailing CR; keep that behaviour for
		// the final line without a newline too.
		message = strings.TrimSuffix(message, "\r")
		w.append(stream, message)
		line = line[:0]
		overlong = false
	}
	for {
		chunk, err := buffered.ReadSlice('\n')
		appendChunk(chunk)
		switch {
		case err == nil:
			emit(true)
		case errors.Is(err, bufio.ErrBufferFull):
			// The line is longer than the read buffer; keep draining it.
		default:
			// EOF, or the pipe was closed on cancellation: emit the final
			// partial line, if any, and stop.
			if len(line) > 0 || overlong {
				emit(false)
			}
			return
		}
	}
}

func (w *logWriter) systemf(format string, args ...any) {
	w.append("system", fmt.Sprintf(format, args...))
}

// append caps total events and per-line length; output past the cap drops.
// Every message is redacted before it is stored, including system lines: a line
// whose credential value survives redaction is dropped entirely rather than
// persisted (fail closed), and a multi-line private-key block is replaced with
// one marker per line until its END delimiter.
func (w *logWriter) append(stream, message string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	message, stream, hits, ok := w.redactMessage(stream, message)
	w.redacted += hits
	if !ok {
		w.dropped++
		return
	}
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

// close reports the redaction tally, then stops the flush loop and flushes the
// tail. The tally records counts only, never the replaced values.
func (w *logWriter) close() {
	w.mu.Lock()
	redacted, dropped := w.redacted, w.dropped
	w.mu.Unlock()
	if redacted > 0 {
		w.systemf("redacted %d sensitive value(s) from build output and system lines", redacted)
	}
	if dropped > 0 {
		w.systemf("dropped %d output line(s) that still matched a sensitive pattern", dropped)
	}
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

// truncateBytes cuts at the byte limit on a rune boundary and marks the cut.
func truncateBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := value[:limit]
	// Drop a trailing incomplete UTF-8 sequence. DecodeLastRuneInString
	// reports RuneError with size 1 for both an incomplete and an invalid
	// trailing byte, while a real U+FFFD has size 3.
	for len(cut) > 0 {
		if r, size := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError || size > 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut + " …[truncated]"
}
