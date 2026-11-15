// Command docker-credential-ghcr-proxy is the Docker credential helper that hands the pod's ServiceAccount token to
// Docker for ghcr.io.
package main

import (
	"os"

	"github.com/docker/docker-credential-helpers/credentials"

	"github.com/solidDoWant/ghcr-proxy/internal/credhelper"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
	credentials.Version = version

	credentials.Serve(credhelper.Helper{TokenFile: tokenFile()})
}

// tokenFile returns the token file path from the environment, or the default.
func tokenFile() string {
	if path := os.Getenv(credhelper.TokenFileEnv); path != "" {
		return path
	}

	return credhelper.DefaultTokenFile
}
