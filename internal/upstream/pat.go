// Package upstream holds the proxy's personal access token and turns it, or
// no credential at all, into registry tokens for the upstream. It is the only
// package that reads the PAT.
package upstream

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/solidDoWant/registrygate/internal/filewatch"
	"github.com/solidDoWant/registrygate/internal/metrics"
)

// errPATEmpty is a PAT file holding only whitespace.
var errPATEmpty = errors.New("PAT file is empty")

// PATFile holds the PAT read from a file and reloads it when the file changes.
// A failed reload keeps the previous PAT.
type PATFile struct {
	path   string
	logger *slog.Logger

	// Metrics counts reloads after construction; nil turns it off. Set it before Watch starts.
	Metrics *metrics.Metrics

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
	p.Metrics.CredentialReload(metrics.KindPAT, err)

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

// Watch reloads the PAT whenever the file changes, with the watcher shared by
// every reloaded file. It returns when ctx ends.
func (p *PATFile) Watch(ctx context.Context) error {
	return filewatch.Watch(ctx, p.logger, []string{p.path}, p.Reload)
}
