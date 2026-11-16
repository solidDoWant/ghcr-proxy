// Command fakeghcr runs the GHCR test double (test/fakeghcr) as a server for
// end-to-end tests. It is never part of a release.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/solidDoWant/registrygate/test/fakeghcr"
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
		admin    string
	)
	flag.StringVar(&opts.RegistryAddr, "registry-addr", ":8443", "listen address of the registry")
	flag.StringVar(&opts.StorageAddr, "storage-addr", ":8444", "listen address of the blob storage")
	flag.StringVar(&opts.RegistryHost, "registry-host", "", "host:port clients use for the registry (default: the listen address)")
	flag.StringVar(&opts.StorageHost, "storage-host", "", "host:port blob redirects point to (default: the listen address)")
	flag.StringVar(&dnsNames, "dns-names", "", "comma-separated extra DNS names for the server certificate")
	flag.StringVar(&opts.BlobDir, "blob-dir", "", "directory for blob data (default: a temporary directory)")
	flag.DurationVar(&opts.TokenTTL, "token-ttl", 0, "token lifetime (default 5m)")
	flag.StringVar(&public, "public", "", "comma-separated repositories anonymous clients may pull")
	flag.StringVar(&caFile, "ca-file", "", "write the CA certificate (PEM) here, or to the standard output when -")
	flag.StringVar(&patFile, "pat-file", "", "read the accepted PAT from here (default: random, written to this file if it is missing)")
	flag.StringVar(&admin, "admin-addr", "", "serve the request log over plain HTTP here: GET /log lists the requests, DELETE /log forgets them")
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

	if caFile == "-" {
		// The e2e test reads the CA from the pod log: the image has no shell to copy a file out.
		fmt.Printf("%s", fake.CAPEM())
	} else if caFile != "" {
		if err := os.WriteFile(caFile, fake.CAPEM(), 0o644); err != nil {
			return err
		}
	}

	for _, repo := range strings.Split(public, ",") {
		if repo != "" {
			fake.MakePublic(repo)
		}
	}

	if admin != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/log", func(resp http.ResponseWriter, req *http.Request) {
			if req.Method == http.MethodDelete {
				fake.ResetLog()
				resp.WriteHeader(http.StatusNoContent)

				return
			}

			resp.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(resp).Encode(fake.Log())
		})

		server := &http.Server{Addr: admin, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

		go func() { _ = server.ListenAndServe() }()
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
