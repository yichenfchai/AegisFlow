package builds

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDirectorySize(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b"), make([]byte, 50), 0o600); err != nil {
		t.Fatal(err)
	}
	size, err := directorySize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if size != 150 {
		t.Fatalf("directorySize=%d want 150", size)
	}
}

// TestWatchDiskUsageFires proves the watchdog calls back once the workspace
// crosses the cap; the build-level wiring is covered by TestBuildDiskLimitStopsBuild.
func TestWatchDiskUsageFires(t *testing.T) {
	old := diskPollInterval
	diskPollInterval = 10 * time.Millisecond
	defer func() { diskPollInterval = old }()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "big"), make([]byte, 200), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fired := make(chan struct{})
	go watchDiskUsage(ctx, dir, 100, func() { close(fired) })
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("disk watchdog did not fire")
	}
}
