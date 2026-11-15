// Command fakeghcr runs the GHCR test double (test/fakeghcr) as a server for
// end-to-end tests. It is never part of a release.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/solidDoWant/ghcr-proxy/test/fakeghcr"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("fakeghcr: %v", err)
	}
}

func run() error {
	var (
		opts     fakeghcr.Options
		dnsNames string
		public   string
		caFile   string
		patFile  string
	)
	flag.StringVar(&opts.RegistryAddr, "registry-addr", ":8443", "listen address of the registry")
	flag.StringVar(&opts.StorageAddr, "storage-addr", ":8444", "listen address of the blob storage")
	flag.StringVar(&opts.RegistryHost, "registry-host", "", "host:port clients use for the registry (default: the listen address)")
	flag.StringVar(&opts.StorageHost, "storage-host", "", "host:port blob redirects point to (default: the listen address)")
	flag.StringVar(&dnsNames, "dns-names", "", "comma-separated extra DNS names for the server certificate")
	flag.StringVar(&opts.BlobDir, "blob-dir", "", "directory for blob data (default: a temporary directory)")
	flag.DurationVar(&opts.TokenTTL, "token-ttl", 0, "token lifetime (default 5m)")
	flag.StringVar(&public, "public", "", "comma-separated repositories anonymous clients may pull")
	flag.StringVar(&caFile, "ca-file", "", "write the CA certificate (PEM) here")
	flag.StringVar(&patFile, "pat-file", "", "read the accepted PAT from here (default: random, written to this file if it is missing)")
	flag.Parse()

	if dnsNames != "" {
		opts.DNSNames = strings.Split(dnsNames, ",")
	}

	if patFile != "" {
		if raw, err := os.ReadFile(patFile); err == nil {
			opts.PAT = strings.TrimSpace(string(raw))
		}
	}

	fake, err := fakeghcr.Start(opts)
	if err != nil {
		return err
	}

	if patFile != "" && opts.PAT == "" {
		if err := os.WriteFile(patFile, []byte(fake.PAT()), 0o600); err != nil {
			return err
		}
	}

	if caFile != "" {
		if err := os.WriteFile(caFile, fake.CAPEM(), 0o644); err != nil {
			return err
		}
	}

	for _, repo := range strings.Split(public, ",") {
		if repo != "" {
			fake.MakePublic(repo)
		}
	}

	fmt.Println("fakeghcr: registry", fake.RegistryHost(), "storage", fake.StorageHost())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()

	closed := make(chan error, 1)
	go func() { closed <- fake.Close() }()

	select {
	case err := <-closed:
		return err
	case <-time.After(10 * time.Second):
		return nil
	}
}
