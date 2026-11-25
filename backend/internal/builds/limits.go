package builds

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"
)

// diskPollInterval bounds how far a workspace can overshoot its cap: this is a
// best-effort watchdog, not a filesystem quota, so a build can exceed the limit
// by whatever it writes within one interval before it is stopped. A variable so
// tests can shrink it without waiting two seconds per check.
var diskPollInterval = 2 * time.Second

// diskStopReason explains why the disk watchdog stopped a build.
type diskStopReason struct {
	// Exceeded reports that the workspace provably grew past the configured
	// cap.
	Exceeded bool
	// Err is set when the workspace size could not be determined (unreadable
	// directory, failed stat, vanished entry). The watchdog fails closed:
	// "cannot verify" is treated as "over the cap" rather than assuming the
	// build is within it.
	Err error
}

// watchDiskUsage cancels a build once its workspace grows past limit, or as soon
// as its size cannot be measured (fail closed). It returns after firing once;
// the caller owns cancellation.
func watchDiskUsage(ctx context.Context, dir string, limit int64, stopped func(diskStopReason)) {
	ticker := time.NewTicker(diskPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			size, err := directorySize(dir)
			switch {
			case err != nil:
				stopped(diskStopReason{Err: err})
				return
			case size > limit:
				stopped(diskStopReason{Exceeded: true})
				return
			}
		}
	}
}

// dirMeasure is the injectable filesystem walk used to size a workspace. The
// real implementation wraps filepath.WalkDir; tests substitute stubs to cover
// the error paths, which are hard to reproduce portably (an unreadable
// directory cannot always be created on Windows).
type dirMeasure struct {
	walk func(root string, fn fs.WalkDirFunc) error
	stat func(entry fs.DirEntry) (fs.FileInfo, error)
}

var defaultDirMeasure = dirMeasure{
	walk: filepath.WalkDir,
	stat: func(entry fs.DirEntry) (fs.FileInfo, error) { return entry.Info() },
}

// size sums regular-file sizes under dir. Any walk or stat error aborts the
// measurement and is returned: a partial total would look like a workspace
// under the cap, which is exactly the fail-open behaviour the watchdog must
// avoid.
func (m dirMeasure) size(dir string) (int64, error) {
	var total int64
	err := m.walk(dir, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := m.stat(entry)
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// directorySize sums regular-file sizes. Unreadable or vanishing entries are
// not skipped: the caller treats an error as an unverifiable cap and fails
// closed.
func directorySize(dir string) (int64, error) {
	return defaultDirMeasure.size(dir)
}

// diskStopMessage renders the system log line for a watchdog stop: a measured
// breach names the cap, an unverifiable workspace names the cause. phase
// distinguishes the clone from the command path.
func diskStopMessage(reason *diskStopReason, limit int64, phase string) string {
	if reason.Exceeded {
		return fmt.Sprintf("build exceeded the disk limit of %s%s", formatBytes(limit), phase)
	}
	return fmt.Sprintf("build workspace disk usage could not be verified%s (%v); stopping the build (fail closed)", phase, reason.Err)
}

// formatBytes renders a byte cap for log lines ("2.0 GiB").
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for value := n / unit; value >= unit && exp < 5; value /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
