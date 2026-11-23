package builds

import (
	"context"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// streamOnlyWriter returns a logWriter that records in memory only: the flush
// loop is not started because these unit tests inspect the captured batch.
func streamOnlyWriter() *logWriter {
	return &logWriter{buildID: "build", max: maxLogEvents, done: make(chan struct{})}
}

// TestLogStreamKeepsReadingAfterOverlongLine is the regression for a scanner
// that stopped at the first line over its buffer limit: every later line was
// lost and the child could block on a full pipe. The scanner's effective token
// cap is max(maxLineBytes+1024, the 64KiB buffer cap), so the line must be
// larger than 64 KiB to reproduce the stop. The reader must truncate it, keep
// reading, and record the lines that follow as-is.
func TestLogStreamKeepsReadingAfterOverlongLine(t *testing.T) {
	w := streamOnlyWriter()
	long := strings.Repeat("界", 30000) // 90000 bytes: over the scanner's 64 KiB cap
	w.stream(strings.NewReader(long+"\n"+"after-long-line\n"+"tail-without-newline"), "stdout")

	if len(w.pending) != 3 {
		t.Fatalf("captured %d events, want 3: %+v", len(w.pending), w.pending)
	}
	first := w.pending[0].Message
	if !strings.HasSuffix(first, "…[truncated]") {
		t.Fatalf("overlong line was not marked as truncated: %q", first)
	}
	if !utf8.ValidString(first) {
		t.Fatalf("truncated line is not valid UTF-8: %q", first)
	}
	// 4096 bytes is 4095 bytes of whole 3-byte runes plus a split rune, so the
	// safe cut keeps exactly 1365 runes.
	if prefix := strings.Repeat("界", 1365); !strings.HasPrefix(first, prefix) {
		t.Fatalf("truncated line lost its valid prefix: %q", first)
	}
	if len(first) > maxLineBytes+len(" …[truncated]") {
		t.Fatalf("truncated line is %d bytes, want at most %d", len(first), maxLineBytes+len(" …[truncated]"))
	}
	if w.pending[1].Message != "after-long-line" {
		t.Fatalf("line after the overlong one = %q, want %q", w.pending[1].Message, "after-long-line")
	}
	if w.pending[2].Message != "tail-without-newline" {
		t.Fatalf("final partial line = %q, want %q", w.pending[2].Message, "tail-without-newline")
	}
}

// TestLogStreamKeepsLineAtLimit pins the boundary: exactly maxLineBytes is not
// a truncation, one byte more is.
func TestLogStreamKeepsLineAtLimit(t *testing.T) {
	atLimit := strings.Repeat("x", maxLineBytes)
	w := streamOnlyWriter()
	w.stream(strings.NewReader(atLimit+"\nnext"), "stdout")
	if len(w.pending) != 2 || w.pending[0].Message != atLimit || w.pending[1].Message != "next" {
		t.Fatalf("line at the limit was not preserved: %+v", w.pending)
	}
}

// TestTruncateBytesKeepsValidUTF8 covers the rune-boundary cut directly; the
// old byte loop could leave a dangling multi-byte lead byte at the cut.
func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	out := truncateBytes(strings.Repeat("界", 2000), maxLineBytes)
	if !utf8.ValidString(out) {
		t.Fatalf("truncateBytes returned invalid UTF-8: %q", out)
	}
	if !strings.HasSuffix(out, "…[truncated]") {
		t.Fatalf("truncateBytes did not mark the cut: %q", out)
	}
	if short := "short"; truncateBytes(short, maxLineBytes) != short {
		t.Fatalf("truncateBytes changed a value below the limit")
	}
}

// overlongOutputCommand prints one 80 KiB line, then enough output that a
// reader which stopped at that line would fill the pipe and block the shell,
// then a final marker line. The line has to exceed 64 KiB because that is the
// old scanner's effective token cap (the larger of its max and buffer cap).
// The command must stay under the 512-byte project command limit and must not
// embed quotes: os/exec escapes them for cmd.exe, whose parser does not
// understand the escape.
func overlongOutputCommand(t *testing.T) string {
	t.Helper()
	long := strings.Repeat("A", 100)
	flood := strings.Repeat("B", 100)
	if runtime.GOOS == "windows" {
		// Parenthesized groups keep each loop inside one command; an ungrouped
		// `&` would become part of the for body.
		return "(for /l %i in (1,1,800) do @<nul set /p =" + long + ") & echo. & " +
			"(for /l %i in (1,1,700) do @echo " + flood + ") & echo after-long-line"
	}
	return "i=0; while [ $i -lt 800 ]; do printf '" + long + "'; i=$((i+1)); done; echo; " +
		"i=0; while [ $i -lt 700 ]; do echo " + flood + "; i=$((i+1)); done; echo after-long-line"
}

// TestBuildKeepsReadingAfterOverlongLine is the end-to-end regression: before
// the fix the build timed out with the later output lost.
func TestBuildKeepsReadingAfterOverlongLine(t *testing.T) {
	s, registry := testService(t)
	// Also exercises the public timeout setter: a blocking read must surface as
	// a successful build well inside this deadline.
	s.WithTimeout(4 * time.Second)
	startWorker(t, s)
	project := createProject(t, registry, "overlong-line", overlongOutputCommand(t))
	build, err := s.Trigger(context.Background(), testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, build.ID, StatusSucceeded)
	text := waitForLog(t, s, build.ID, "after-long-line")
	if !strings.Contains(text, "…[truncated]") {
		t.Fatalf("overlong line missing its truncation marker: %s", text)
	}
}

// TestBuildSurvivesDatabaseErrorInStatusWatchdog is the P2-12 regression: a
// failed status read must be retried on the next tick instead of being treated
// as an operator cancellation, and the failed finalize must be reported.
//
// The outage is simulated by renaming the builds table from an independent
// pool: the worker's status reads fail, while build_log_events stays readable
// so the test can observe whether the command was allowed to finish.
func TestBuildSurvivesDatabaseErrorInStatusWatchdog(t *testing.T) {
	s, registry := testService(t)
	previous := slog.Default()
	capture := &logRecorder{}
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(previous) })
	observer := testutil.ClonePostgres(t, s.db)
	startWorker(t, s)

	project := createProject(t, registry, "watchdog-db-error", sleepCommand(2))
	build, err := s.Trigger(context.Background(), testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, build.ID, StatusRunning)
	if _, err := observer.Exec(`ALTER TABLE builds RENAME TO builds_status_offline`); err != nil {
		t.Fatal(err)
	}
	capture.waitFor(t, "build status watchdog read failed", 5*time.Second)

	// The command needs about two seconds. A watchdog that killed on the read
	// error would do so within 300ms and record a failure instead.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var succeeded int
		if err = observer.QueryRow(`SELECT COUNT(*) FROM build_log_events WHERE build_id=$1 AND message='build succeeded'`, build.ID).Scan(&succeeded); err != nil {
			t.Fatal(err)
		}
		if succeeded > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("build did not run to completion after the status read failed; the watchdog treated the database error as cancellation")
		}
		time.Sleep(100 * time.Millisecond)
	}
	capture.waitFor(t, "finalize build status failed", 5*time.Second)
}
