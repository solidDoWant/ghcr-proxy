// Command ghcr-proxy is the authenticating reverse proxy for ghcr.io. This is the skeleton: it parses its flags and
// prints its version; it serves nothing yet.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

// defaultConfigPath is where the chart mounts the rendered config file.
const defaultConfigPath = "/etc/ghcr-proxy/config.yaml"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses args and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("ghcr-proxy", flag.ContinueOnError)
	flags.SetOutput(stderr)

	configPath := flags.String("config", defaultConfigPath, "Path to the YAML config file.")
	showVersion := flags.Bool("version", false, "Print the version and exit.")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	_, _ = fmt.Fprintf(stdout, "ghcr-proxy %s\n", version)

	if *showVersion {
		return 0
	}

	_, _ = fmt.Fprintf(stderr, "ghcr-proxy: config %s: serving is not implemented yet\n", *configPath)

	return 1
}
