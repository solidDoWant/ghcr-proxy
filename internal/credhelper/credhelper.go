// Package credhelper implements the Docker credential helper for ghcr.io: it hands the pod's ServiceAccount token,
// read fresh from a file on every call, to Docker.
package credhelper

import (
	"fmt"
	"os"
	"strings"

	"github.com/docker/docker-credential-helpers/credentials"
)

const (
	// DefaultTokenFile is where the pod's projected ServiceAccount token is mounted.
	DefaultTokenFile = "/var/run/secrets/registrygate/token"
	// TokenFileEnv names the environment variable that overrides DefaultTokenFile.
	TokenFileEnv = "REGISTRYGATE_TOKEN_FILE"
	// Username is the username returned with the token; the proxy ignores it.
	Username = "registrygate"

	registryHost = "ghcr.io"
)

// Helper is a credentials.Helper that serves only ghcr.io.
type Helper struct {
	// TokenFile is the path of the token file, read on every Get.
	TokenFile string
}

var _ credentials.Helper = Helper{}

// Get returns the token for ghcr.io, or "credentials not found" for any other server.
func (h Helper) Get(serverURL string) (string, string, error) {
	if !isGHCR(serverURL) {
		return "", "", credentials.NewErrCredentialsNotFound()
	}

	raw, err := os.ReadFile(h.TokenFile)
	token := strings.TrimSpace(string(raw))

	// The message carries only the path: never the file's contents or the underlying error.
	if err != nil || token == "" {
		return "", "", fmt.Errorf("registrygate credential helper: token file %s is missing or empty", h.TokenFile)
	}

	return Username, token, nil
}

// Add does nothing: the token always comes from the file.
func (Helper) Add(*credentials.Credentials) error { return nil }

// Delete does nothing.
func (Helper) Delete(string) error { return nil }

// List returns no stored credentials.
func (Helper) List() (map[string]string, error) { return map[string]string{}, nil }

// isGHCR reports whether serverURL is ghcr.io, with or without a scheme and path.
func isGHCR(serverURL string) bool {
	rest := strings.TrimSpace(serverURL)
	rest = strings.TrimPrefix(rest, "https://")
	host, _, _ := strings.Cut(rest, "/")

	return host == registryHost
}
