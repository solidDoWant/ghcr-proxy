// Package mirror holds what the proxy needs to reach a pull mirror. Its
// credentials are a type of their own, read only from the mirror's
// credentials file; this package never reads the PAT.
package mirror

import (
	"context"
	"crypto/x509"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/solidDoWant/ghcr-proxy/internal/config"
	"github.com/solidDoWant/ghcr-proxy/internal/filewatch"
)

const redacted = "[redacted]"

// Credentials are a mirror's own user name and password. They are not the PAT
// and are never sent anywhere but the mirror's token endpoint.
type Credentials struct {
	Username string
	Password string
}

// String hides the credentials so they can't reach a log or an error by
// accident.
func (Credentials) String() string { return redacted }

// GoString hides the credentials from %#v.
func (Credentials) GoString() string { return redacted }

// LogValue hides the credentials from slog.
func (Credentials) LogValue() slog.Value { return slog.StringValue(redacted) }

// State is what the mirror's files hold right now.
type State struct {
	// Credentials is nil when the mirror has no credentials file and is
	// reached anonymously.
	Credentials *Credentials
	// RootCAs is nil when the mirror has no CA file and only the system roots
	// are trusted.
	RootCAs *x509.CertPool
	// Generation changes whenever the credentials change.
	Generation uint64
}

// Files holds a mirror's credentials and CA, read from its configured files
// and reloaded when they change. A failed reload keeps the previous value.
type Files struct {
	name            string
	credentialsPath string
	caPath          string
	logger          *slog.Logger

	mu                sync.RWMutex
	credentials       *Credentials
	credentialsLoaded bool
	rootCAs           *x509.CertPool
	caLoaded          bool
	generation        uint64
}

var (
	errCredentialsFormat = errors.New(`credentials file must hold "user:password" with both parts non-empty`)
	errNoCertificates    = errors.New("CA file holds no PEM certificate")
)

// NewFiles reads the files of the mirror called name. A missing or invalid
// file is not an error: the mirror is then unavailable until a reload
// succeeds.
func NewFiles(name string, mirror *config.Mirror, logger *slog.Logger) *Files {
	if logger == nil {
		logger = slog.Default()
	}

	files := &Files{
		name:            name,
		credentialsPath: mirror.CredentialsFile,
		caPath:          mirror.CAFile,
		logger:          logger,
	}
	files.Reload()

	return files
}

// Get returns the current state. ok is false while a configured file has
// never been loaded, which makes the mirror unavailable.
func (f *Files) Get() (state State, ok bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if (f.credentialsPath != "" && !f.credentialsLoaded) || (f.caPath != "" && !f.caLoaded) {
		return State{}, false
	}

	return State{Credentials: f.credentials, RootCAs: f.rootCAs, Generation: f.generation}, true
}

// Available reports whether every configured file has been loaded.
func (f *Files) Available() bool {
	_, ok := f.Get()
	return ok
}

// Reload reads the configured files again. Each file that fails keeps its
// previous value; errors are logged without the file contents.
func (f *Files) Reload() {
	if f.credentialsPath != "" {
		credentials, err := readCredentials(f.credentialsPath)
		if err != nil {
			f.logger.Error("mirror credentials file could not be loaded; keeping the previous credentials",
				"mirror", f.name, "path", f.credentialsPath, "error", err.Error())
		} else {
			f.setCredentials(credentials)
		}
	}

	if f.caPath != "" {
		pool, err := readCA(f.caPath)
		if err != nil {
			f.logger.Error("mirror CA file could not be loaded; keeping the previous CA",
				"mirror", f.name, "path", f.caPath, "error", err.Error())
		} else {
			f.mu.Lock()
			f.rootCAs = pool
			f.caLoaded = true
			f.mu.Unlock()
		}
	}
}

func (f *Files) setCredentials(credentials *Credentials) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.credentialsLoaded && *f.credentials == *credentials {
		return
	}

	f.credentials = credentials
	f.credentialsLoaded = true
	f.generation++
	f.logger.Info("mirror credentials loaded", "mirror", f.name, "path", f.credentialsPath, "generation", f.generation)
}

// Watch reloads the files whenever they change, with the same watcher as the
// PAT. It returns when ctx ends.
func (f *Files) Watch(ctx context.Context) error {
	paths := make([]string, 0, 2)

	for _, path := range []string{f.credentialsPath, f.caPath} {
		if path != "" {
			paths = append(paths, path)
		}
	}

	if len(paths) == 0 {
		<-ctx.Done()
		return nil
	}

	return filewatch.Watch(ctx, f.logger, paths, f.Reload)
}

func readFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			// The error text names only the operation and the path.
			return nil, pathErr
		}

		return nil, errors.New("file could not be read")
	}

	return data, nil
}

func readCredentials(path string) (*Credentials, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}

	username, password, found := strings.Cut(strings.TrimSpace(string(data)), ":")
	if !found || username == "" || password == "" {
		return nil, errCredentialsFormat
	}

	return &Credentials{Username: username, Password: password}, nil
}

func readCA(path string) (*x509.CertPool, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}

	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}

	if !pool.AppendCertsFromPEM(data) {
		return nil, errNoCertificates
	}

	return pool, nil
}
