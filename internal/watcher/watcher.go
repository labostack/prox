// Package watcher monitors config files for changes using stat-based polling.
package watcher

import (
	"context"
	"log/slog"
	"os"
	"time"
)

const pollInterval = 1 * time.Second

// Watch monitors one or more files for modifications and calls onChange when
// any file changes. It compares mtime + size on each tick. Blocks until ctx is cancelled.
func Watch(ctx context.Context, paths []string, onChange func()) {
	snapshots := make(map[string]fileSnapshot, len(paths))

	for _, p := range paths {
		s, err := snapshot(p)
		if err != nil {
			slog.Warn("file watcher: cannot stat file, skipping",
				"path", p,
				"error", err,
			)
			continue
		}
		snapshots[p] = s
	}

	if len(snapshots) == 0 {
		slog.Warn("file watcher: no files to watch, watcher disabled")
		return
	}

	slog.Info("file watcher started", "files", len(snapshots))

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for p, prev := range snapshots {
				cur, err := snapshot(p)
				if err != nil {
					slog.Debug("file watcher: stat error", "path", p, "error", err)
					continue
				}

				if cur != prev {
					slog.Info("config file changed, triggering reload", "path", p)
					snapshots[p] = cur
					onChange()
					break // one reload per tick is enough
				}
			}
		}
	}
}

// fileSnapshot captures the relevant metadata for change detection.
type fileSnapshot struct {
	modTime time.Time
	size    int64
}

func snapshot(path string) (fileSnapshot, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileSnapshot{}, err
	}
	return fileSnapshot{
		modTime: info.ModTime(),
		size:    info.Size(),
	}, nil
}
