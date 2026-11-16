// Command docker-credential-registrygate is the Docker credential helper that hands the pod's ServiceAccount token to
// Docker for the hosts in REGISTRYGATE_HOSTS. The "configure" subcommand writes the matching credHelpers entries.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/docker/docker-credential-helpers/credentials"

	"github.com/solidDoWant/registrygate/internal/credhelper"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "configure" {
		if err := configure(); err != nil {
			fmt.Fprintln(os.Stderr, "docker-credential-registrygate configure:", err)
			os.Exit(1)
		}

		return
	}

	credentials.Version = version

	credentials.Serve(credhelper.Helper{TokenFile: tokenFile(), Hosts: hosts()})
}

// hosts returns the normalized hosts from the environment.
func hosts() []string {
	return credhelper.ParseHosts(os.Getenv(credhelper.HostsEnv))
}

// configure writes the credHelpers entries into $DOCKER_CONFIG/config.json (default $HOME/.docker/config.json).
func configure() error {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}

		dir = filepath.Join(home, ".docker")
	}

	return credhelper.ConfigureFile(filepath.Join(dir, "config.json"), hosts())
}

// tokenFile returns the token file path from the environment, or the default.
func tokenFile() string {
	if path := os.Getenv(credhelper.TokenFileEnv); path != "" {
		return path
	}

	return credhelper.DefaultTokenFile
}
