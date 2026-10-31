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

// watchDiskUsage cancels a build once its workspace grows past limit. It returns
// when the build ends or after firing once; the caller owns cancellation.
func watchDiskUsage(ctx context.Context, dir string, limit int64, exceeded func()) {
	ticker := time.NewTicker(diskPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if size, err := directorySize(dir); err == nil && size > limit {
				exceeded()
				return
			}
		}
	}
}

// directorySize sums regular-file sizes. Unreadable or vanishing entries are
// skipped: the build is still writing while the check runs.
func directorySize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total, err
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
