package server

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// certRestatInterval is how often the certificate files are re-read in case a change event is missed.
const certRestatInterval = 60 * time.Second

var errNoCertificate = errors.New("no TLS certificate loaded")

// Certificate is the serving certificate, read from a key pair on disk and reloaded when the files change. A failed
// reload keeps the previous pair.
type Certificate struct {
	certFile string
	keyFile  string
	logger   *slog.Logger

	current atomic.Pointer[tls.Certificate]
}

// NewCertificate reads the key pair. Missing or invalid files are not an error: the certificate is then not loaded
// until a later reload succeeds, and the replica is not ready.
func NewCertificate(certFile, keyFile string, logger *slog.Logger) *Certificate {
	if logger == nil {
		logger = slog.Default()
	}

	certificate := &Certificate{certFile: certFile, keyFile: keyFile, logger: logger}
	certificate.Reload()

	return certificate
}

// Loaded reports whether a key pair has been loaded.
func (c *Certificate) Loaded() bool {
	return c.current.Load() != nil
}

// GetCertificate serves the current pair to new TLS connections.
func (c *Certificate) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	current := c.current.Load()
	if current == nil {
		return nil, errNoCertificate
	}

	return current, nil
}

// TLSConfig returns a server TLS configuration that serves the current pair, TLS 1.2 or later.
func (c *Certificate) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: c.GetCertificate}
}

// Reload reads the key pair again; on failure the previous pair stays and an error is logged.
func (c *Certificate) Reload() {
	pair, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		c.logger.Error("TLS key pair could not be loaded; keeping the previous one",
			"certFile", c.certFile, "keyFile", c.keyFile, "error", err.Error())

		return
	}

	c.current.Store(&pair)
}

// Watch reloads the pair whenever the files' directories change, which catches the symlink swap Kubernetes uses to
// update Secret volumes, and re-reads them every minute in case an event is missed. It returns when ctx ends.
func (c *Certificate) Watch(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = watcher.Close() }()

	for _, dir := range uniqueDirs(c.certFile, c.keyFile) {
		if err := watcher.Add(dir); err != nil {
			return err
		}
	}

	// A change between the first read and the watch being set up raised no event.
	c.Reload()

	ticker := time.NewTicker(certRestatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case _, open := <-watcher.Events:
			if !open {
				return nil
			}

			c.Reload()
		case watchErr, open := <-watcher.Errors:
			if !open {
				return nil
			}

			c.logger.Error("TLS file watch error", "error", watchErr.Error())
		case <-ticker.C:
			c.Reload()
		}
	}
}

func uniqueDirs(paths ...string) []string {
	var dirs []string

	seen := map[string]bool{}

	for _, path := range paths {
		dir := filepath.Dir(path)
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}

	return dirs
}
