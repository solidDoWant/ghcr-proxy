// Command docker-credential-ghcr-proxy is the Docker credential helper that hands the pod's ServiceAccount token to
// Docker for ghcr.io. This is the skeleton: it prints its usage and fails.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
	os.Exit(run(os.Stderr))
}

// run prints the usage to stderr and returns the process exit code.
func run(stderr io.Writer) int {
	_, _ = fmt.Fprintf(stderr, "docker-credential-ghcr-proxy %s\nUsage: docker-credential-ghcr-proxy get|store|erase|list\n", version)

	return 1
}
