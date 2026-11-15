// Package upstream holds the proxy's personal access token and turns it, or
// no credential at all, into registry tokens for the upstream. It is the only
// package that reads the PAT.
package upstream

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// patRestatInterval is the backstop for missed file events.
const patRestatInterval = 60 * time.Second

// errPATEmpty is a PAT file holding only whitespace.
var errPATEmpty = errors.New("PAT file is empty")

// PATFile holds the PAT read from a file and reloads it when the file changes.
// A failed reload keeps the previous PAT.
type PATFile struct {
	path   string
	logger *slog.Logger

	mu         sync.RWMutex
	pat        string
	generation uint64
}

// NewPATFile reads the PAT from path. A missing, unreadable or empty file is
// not an error: the PAT is then not loaded until a later reload succeeds.
func NewPATFile(path string, logger *slog.Logger) *PATFile {
	if logger == nil {
		logger = slog.Default()
	}

	holder := &PATFile{path: path, logger: logger}
	holder.Reload()

	return holder
}

// Get returns the current PAT and its generation. ok is false while no PAT
// has been loaded.
func (p *PATFile) Get() (pat string, generation uint64, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.pat, p.generation, p.pat != ""
}

// Loaded reports whether a PAT has been loaded.
func (p *PATFile) Loaded() bool {
	_, _, ok := p.Get()
	return ok
}

// Reload reads the file again. A changed PAT starts a new generation; a
// failed read keeps the previous PAT and logs an error without the contents.
func (p *PATFile) Reload() {
	pat, err := readPAT(p.path)
	if err != nil {
		p.logger.Error("PAT file could not be loaded; keeping the previous PAT", "path", p.path, "error", err.Error())
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if pat != p.pat {
		p.pat = pat
		p.generation++
		p.logger.Info("PAT loaded", "path", p.path, "generation", p.generation)
	}
}

func readPAT(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			// The error text names only the operation and the path.
			return "", pathErr
		}

		return "", errors.New("PAT file could not be read")
	}

	pat := strings.TrimSpace(string(raw))
	if pat == "" {
		return "", errPATEmpty
	}

	return pat, nil
}

// Watch reloads the PAT whenever the file's directory changes, which catches
// the symlink swap Kubernetes uses to update Secret volumes, and re-reads it
// every minute in case an event is missed. It returns when ctx ends.
func (p *PATFile) Watch(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(filepath.Dir(p.path)); err != nil {
		return err
	}

	ticker := time.NewTicker(patRestatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case _, open := <-watcher.Events:
			if !open {
				return nil
			}

			p.Reload()
		case watchErr, open := <-watcher.Errors:
			if !open {
				return nil
			}

			p.logger.Error("PAT file watch error", "path", p.path, "error", watchErr.Error())
		case <-ticker.C:
			p.Reload()
		}
	}
}
