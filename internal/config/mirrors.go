package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/distribution/reference"
)

// parseMirrorURL accepts an http or https URL naming only a host: the mirror's
// repositories live under /v2/ on that host, and anything else in the URL
// would be silently ignored.
func parseMirrorURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("is required")
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("%q is not an http or https URL", raw)
	}

	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%q must name only a scheme, host and port; put credentials in credentialsFile and the project in pathPrefix", raw)
	}

	u.Path = ""

	return u, nil
}

// validatePathPrefix checks that prefix followed by a repository name is a
// valid repository path, so mirror names can be built by joining the two.
func validatePathPrefix(prefix string) error {
	if prefix == "" {
		return errors.New("is required")
	}

	named, err := reference.ParseNamed("mirror.invalid/" + prefix)
	if err != nil || reference.Path(named) != prefix {
		return fmt.Errorf("%q is not a repository path (lowercase components separated by /)", prefix)
	}

	return nil
}

// sameFile reports whether path and other name the same file, after cleaning
// the paths and following symlinks. Files that don't exist yet are compared by
// their resolved paths.
func sameFile(path, other string) bool {
	if resolvePath(path) == resolvePath(other) {
		return true
	}

	pathInfo, err := os.Stat(path)
	if err != nil {
		return false
	}

	otherInfo, err := os.Stat(other)
	if err != nil {
		return false
	}

	return os.SameFile(pathInfo, otherInfo)
}

func resolvePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}

	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}

	return filepath.Clean(path)
}
