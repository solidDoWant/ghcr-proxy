// Command ghcr-proxy is the authenticating reverse proxy for ghcr.io.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	"github.com/solidDoWant/ghcr-proxy/api/v1alpha1"
	"github.com/solidDoWant/ghcr-proxy/internal/audit"
	"github.com/solidDoWant/ghcr-proxy/internal/authn"
	"github.com/solidDoWant/ghcr-proxy/internal/authz"
	"github.com/solidDoWant/ghcr-proxy/internal/config"
	"github.com/solidDoWant/ghcr-proxy/internal/mirror"
	"github.com/solidDoWant/ghcr-proxy/internal/registry"
	"github.com/solidDoWant/ghcr-proxy/internal/server"
	"github.com/solidDoWant/ghcr-proxy/internal/upstream"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

// defaultConfigPath is where the chart mounts the rendered config file.
const defaultConfigPath = "/etc/ghcr-proxy/config.yaml"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run parses args, serves until ctx ends, and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
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

	cfg, err := config.Load(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ghcr-proxy: %v\n", err)
		return 1
	}

	logger := slog.New(slog.NewJSONHandler(stderr, nil))

	if err := serve(ctx, cfg, logger); err != nil {
		logger.Error("ghcr-proxy stopped", "error", err.Error())
		return 1
	}

	return 0
}

// serve wires the proxy together and runs it until ctx ends.
func serve(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	upstreamTransport, err := newUpstreamTransport(cfg.Upstream.CAFile)
	if err != nil {
		return err
	}

	pat := upstream.NewPATFile(cfg.Upstream.PATFile, logger)
	certificate := server.NewCertificate(cfg.TLS.CertFile, cfg.TLS.KeyFile, logger)

	// A mirror whose files are missing or invalid is unavailable, not fatal: its pulls take the fallback rule.
	var mirrorFiles []*mirror.Files
	if cfg.Mirrors.Policy != nil {
		mirrorFiles = append(mirrorFiles, mirror.NewFiles("policy", cfg.Mirrors.Policy, logger))
	}

	if cfg.Mirrors.Anonymous != nil {
		mirrorFiles = append(mirrorFiles, mirror.NewFiles("anonymous", cfg.Mirrors.Anonymous, logger))
	}

	tokens, err := upstream.NewTokens(upstream.TokensConfig{
		URL:          cfg.Upstream.URL,
		TokenHosts:   cfg.Upstream.TokenHosts,
		Transport:    upstreamTransport,
		TokenTimeout: cfg.Upstream.TokenTimeout,
		PAT:          pat,
		Logger:       logger,
	})
	if err != nil {
		return fmt.Errorf("upstream tokens: %w", err)
	}

	forwarder := registry.NewForwarder(registry.ForwarderConfig{
		Upstream:  cfg.Upstream.URL,
		Transport: upstreamTransport,
		Tokens:    tokens,
		Timeout:   cfg.Upstream.Timeout,
		Logger:    logger,
	})

	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubernetes client config: %w", err)
	}

	reviewer, err := authn.NewReviewerForConfig(restConfig, authn.Options{
		Audience:      cfg.Authentication.Audience,
		CacheTTL:      cfg.Authentication.CacheTTL,
		ReviewTimeout: cfg.Authentication.ReviewTimeout,
	})
	if err != nil {
		return err
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}

	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}

	// The index has its own informer, apart from the status controller's manager: losing the status lease stops the
	// manager, and serving must carry on with live policies.
	policyCache, err := cache.New(restConfig, cache.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("policy cache: %w", err)
	}

	index := authz.NewIndex(cfg.Grants)
	if err := index.Watch(ctx, policyCache); err != nil {
		return err
	}

	statusController := &statusController{
		restConfig: restConfig,
		scheme:     scheme,
		namespace:  os.Getenv("POD_NAMESPACE"),
		grants:     cfg.Grants,
		logger:     logger,
	}

	httpsListener, err := net.Listen("tcp", cfg.Listen.HTTPS)
	if err != nil {
		return err
	}

	healthListener, err := net.Listen("tcp", cfg.Listen.Health)
	if err != nil {
		_ = httpsListener.Close()
		return err
	}

	// The informer, the status controller and the file watchers outlive the listeners' drain, so requests still
	// being served during shutdown are decided against live policies.
	background, stopBackground := context.WithCancel(context.WithoutCancel(ctx))
	defer stopBackground()

	var components sync.WaitGroup

	backgroundErrs := make(chan error, 3+len(mirrorFiles))

	start := func(run func(context.Context) error) {
		components.Go(func() { backgroundErrs <- run(background) })
	}

	start(policyCache.Start)
	start(pat.Watch)
	start(certificate.Watch)

	for _, files := range mirrorFiles {
		start(files.Watch)
	}
	// The status controller never fails the proxy; it retries until the proxy stops.
	components.Go(func() { statusController.run(background) })

	readiness := server.NewReadiness(index.Synced, pat.Loaded, certificate.Loaded)
	handler := &server.Handler{
		Reviewer:  reviewer,
		Decider:   &authz.Authorizer{Index: index},
		Forwarder: forwarder,
		Ready:     readiness.Ready,
		Logger:    logger,
	}

	if cfg.Audit.Enabled {
		handler.Audit = audit.New(os.Stdout)
	}

	serveCtx, stopServing := context.WithCancel(ctx)
	defer stopServing()

	// A background component that fails stops the proxy; a restart is better than serving with stale state.
	go func() {
		select {
		case err := <-backgroundErrs:
			if err != nil {
				logger.Error("background component failed", "error", err.Error())
			}

			stopServing()
		case <-serveCtx.Done():
		}
	}()

	// Stop the background components after the listeners drain, and wait for them: the status controller releases
	// its lease on the way out, and the process must not exit first.
	defer components.Wait()
	defer stopBackground()

	return server.Run(serveCtx, server.Config{
		HTTPS:               httpsListener,
		TLSConfig:           certificate.TLSConfig(),
		Health:              healthListener,
		Handler:             handler,
		Readiness:           readiness,
		ReadHeaderTimeout:   cfg.Server.ReadHeaderTimeout,
		IdleTimeout:         cfg.Server.IdleTimeout,
		MaxHeaderBytes:      cfg.Server.MaxHeaderBytes,
		ShutdownGracePeriod: cfg.Server.ShutdownGracePeriod,
		Logger:              logger,
	})
}

// newUpstreamTransport returns the transport for upstream requests: the system roots plus caFile when set. It never
// follows redirects; the client follows them itself.
func newUpstreamTransport(caFile string) (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	if caFile == "" {
		return transport, nil
	}

	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("upstream CA file: %w", err)
	}

	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}

	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("upstream CA file holds no PEM certificate")
	}

	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	return transport, nil
}
