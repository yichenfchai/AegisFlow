package builds

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// TestDirectorySizeReportsWalkError pins the fail-closed change: an unreadable
// or vanishing workspace used to be reported as zero bytes, which made the
// watchdog believe the build was far below its cap.
func TestDirectorySizeReportsWalkError(t *testing.T) {
	if _, err := directorySize(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("a walk over a missing workspace reported no error")
	}
}

// fakeFileInfo and fakeDirEntry back the dirMeasure table tests. The walk/stat
// seams are needed because an unreadable directory cannot be created portably
// on Windows, so the error paths are exercised with injected failures.
type fakeFileInfo struct{ size int64 }

func (f fakeFileInfo) Name() string       { return "fake" }
func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) Mode() fs.FileMode  { return 0o644 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

type fakeDirEntry struct {
	name    string
	regular bool
	size    int64
	infoErr error
}

func (e fakeDirEntry) Name() string { return e.name }
func (e fakeDirEntry) IsDir() bool  { return !e.regular }
func (e fakeDirEntry) Type() fs.FileMode {
	if e.regular {
		return 0
	}
	return fs.ModeDir
}
func (e fakeDirEntry) Info() (fs.FileInfo, error) {
	if e.infoErr != nil {
		return nil, e.infoErr
	}
	return fakeFileInfo{size: e.size}, nil
}

// TestDirMeasureReportsEveryError is the P2-11 regression: walk callback
// errors (unreadable directory), root errors and entry.Info failures must all
// abort the measurement instead of being skipped.
func TestDirMeasureReportsEveryError(t *testing.T) {
	walkErr := errors.New("permission denied")
	statErr := errors.New("stat failed")
	entry := func(name string, regular bool, size int64) fakeDirEntry {
		return fakeDirEntry{name: name, regular: regular, size: size}
	}
	statEntry := func(e fs.DirEntry) (fs.FileInfo, error) { return e.Info() }
	for _, tc := range []struct {
		name     string
		measure  dirMeasure
		wantSize int64
		wantErr  bool
	}{
		{
			name: "sums regular files and skips other entries",
			measure: dirMeasure{
				walk: func(root string, fn fs.WalkDirFunc) error {
					for _, e := range []fs.DirEntry{entry("a", true, 10), entry("sub", false, 0), entry("b", true, 32)} {
						if err := fn(filepath.Join(root, e.Name()), e, nil); err != nil {
							return err
						}
					}
					return nil
				},
				stat: statEntry,
			},
			wantSize: 42,
		},
		{
			name: "root walk error",
			measure: dirMeasure{
				walk: func(root string, fn fs.WalkDirFunc) error { return fn(root, nil, walkErr) },
				stat: statEntry,
			},
			wantErr: true,
		},
		{
			name: "child walk error",
			measure: dirMeasure{
				walk: func(root string, fn fs.WalkDirFunc) error {
					return fn(filepath.Join(root, "locked"), nil, walkErr)
				},
				stat: statEntry,
			},
			wantErr: true,
		},
		{
			name: "entry stat error",
			measure: dirMeasure{
				walk: func(root string, fn fs.WalkDirFunc) error {
					return fn(filepath.Join(root, "gone"), entry("gone", true, 1), nil)
				},
				stat: func(fs.DirEntry) (fs.FileInfo, error) { return nil, statErr },
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			size, err := tc.measure.size("workspace")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("size=%d, want an error so the watchdog can fail closed", size)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if size != tc.wantSize {
				t.Fatalf("size=%d want %d", size, tc.wantSize)
			}
		})
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
	fired := make(chan diskStopReason, 1)
	go watchDiskUsage(ctx, dir, 100, func(reason diskStopReason) { fired <- reason })
	select {
	case reason := <-fired:
		if !reason.Exceeded || reason.Err != nil {
			t.Fatalf("watchdog reason = %+v, want a measured breach", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disk watchdog did not fire")
	}
}

// TestWatchDiskUsageFailsClosedWhenSizeUnknown is the P2-11 regression: when the
// workspace size cannot be measured the watchdog must stop the build instead of
// waiting for a later poll that might never succeed.
func TestWatchDiskUsageFailsClosedWhenSizeUnknown(t *testing.T) {
	old := diskPollInterval
	diskPollInterval = 10 * time.Millisecond
	defer func() { diskPollInterval = old }()

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fired := make(chan diskStopReason, 1)
	go watchDiskUsage(ctx, missing, 1<<20, func(reason diskStopReason) { fired <- reason })
	select {
	case reason := <-fired:
		if reason.Exceeded {
			t.Fatalf("reason = %+v, an unreadable workspace is not a measured breach", reason)
		}
		if reason.Err == nil {
			t.Fatalf("reason = %+v, want the measurement error", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disk watchdog kept failing open on an unmeasurable workspace")
	}
}

// TestDiskStopMessage checks that the two failure modes stay distinguishable in
// the system log: a measured breach names the cap, an unverifiable workspace
// names the cause and says it failed closed.
func TestDiskStopMessage(t *testing.T) {
	exceeded := diskStopMessage(&diskStopReason{Exceeded: true}, 1024, "")
	if !strings.Contains(exceeded, "disk limit") || !strings.Contains(exceeded, "1.0 KiB") {
		t.Fatalf("breach message = %q", exceeded)
	}
	unknown := diskStopMessage(&diskStopReason{Err: errors.New("permission denied")}, 1024, " during clone")
	for _, want := range []string{"could not be verified", "permission denied", "fail closed", "during clone"} {
		if !strings.Contains(unknown, want) {
			t.Fatalf("unverifiable message %q missing %q", unknown, want)
		}
	}
}
