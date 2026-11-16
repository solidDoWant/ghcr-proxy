// Package filewatch reloads files mounted from Kubernetes Secrets when they
// change.
package filewatch

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/fsnotify/fsnotify"
)

// RestatInterval is the backstop for missed file events.
const RestatInterval = 60 * time.Second

// Watch calls reload whenever a directory holding one of paths changes, which
// catches the symlink swap Kubernetes uses to update Secret volumes, and every
// RestatInterval in case an event is missed or a directory can't be watched. It calls reload once after the
// watch is set up, because a change between the caller's first read and that
// point raised no event. It returns when ctx ends.
func Watch(ctx context.Context, logger *slog.Logger, paths []string, reload func()) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = watcher.Close() }()

	var dirs []string
	for _, path := range paths {
		if dir := filepath.Dir(path); !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}

	for _, dir := range dirs {
		// A directory that can't be watched (it doesn't exist yet, for
		// example) is still re-read on every tick.
		if err := watcher.Add(dir); err != nil {
			logger.Error("file watch could not be set up; re-reading periodically", "dir", dir, "error", err.Error())
		}
	}

	reload()

	ticker := time.NewTicker(RestatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case _, open := <-watcher.Events:
			if !open {
				return nil
			}

			reload()
		case watchErr, open := <-watcher.Errors:
			if !open {
				return nil
			}

			logger.Error("file watch error", "paths", paths, "error", watchErr.Error())
		case <-ticker.C:
			reload()
		}
	}
}
