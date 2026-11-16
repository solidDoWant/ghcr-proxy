package server

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/solidDoWant/ghcr-proxy/internal/metrics"
)

// endpointRemovalDelay is how long the HTTPS listener keeps serving after SIGTERM with /readyz failing, so the
// replica is removed from the Service's endpoints before it stops accepting connections.
const endpointRemovalDelay = 5 * time.Second

// Readiness tracks whether the replica may serve: every check passes and shutdown hasn't begun.
type Readiness struct {
	checks       []func() bool
	shuttingDown atomic.Bool
}

// NewReadiness returns a Readiness that passes when every check passes.
func NewReadiness(checks ...func() bool) *Readiness {
	return &Readiness{checks: checks}
}

// Ready reports whether the replica is ready.
func (r *Readiness) Ready() bool {
	if r.shuttingDown.Load() {
		return false
	}

	for _, check := range r.checks {
		if !check() {
			return false
		}
	}

	return true
}

// ShutDown makes the replica not ready from now on.
func (r *Readiness) ShutDown() {
	r.shuttingDown.Store(true)
}

// NewHealthHandler serves /healthz (the process is alive), /readyz (ready reports true) and /metrics (the proxy's
// metrics, plus the trimmed Go and process collectors).
func NewHealthHandler(ready func() bool, proxyMetrics *metrics.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(resp http.ResponseWriter, _ *http.Request) {
		_, _ = resp.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(resp http.ResponseWriter, _ *http.Request) {
		if !ready() {
			http.Error(resp, "not ready", http.StatusServiceUnavailable)
			return
		}

		_, _ = resp.Write([]byte("ok\n"))
	})
	mux.Handle("GET /metrics", metrics.Handler(proxyMetrics.Gatherer()))

	return mux
}

// Config configures Run.
type Config struct {
	// HTTPS is the registry listener, served with TLSConfig (which must set GetCertificate).
	HTTPS     net.Listener
	TLSConfig *tls.Config
	// Health is the plain HTTP listener for /healthz, /readyz and /metrics.
	Health    net.Listener
	Handler   http.Handler
	Readiness *Readiness
	// Metrics is served at /metrics; nil serves only the Go and process collectors.
	Metrics *metrics.Metrics

	ReadHeaderTimeout   time.Duration
	IdleTimeout         time.Duration
	MaxHeaderBytes      int
	ShutdownGracePeriod time.Duration
	Logger              *slog.Logger
}

// Run serves until ctx ends, then shuts down: /readyz fails at once, the HTTPS listener keeps serving for a few
// seconds while the replica leaves the Service's endpoints, and in-flight requests get ShutdownGracePeriod to finish.
// There are no read or write timeouts, because multi-gigabyte uploads stream through.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	errorLog := slog.NewLogLogger(cfg.Logger.Handler(), slog.LevelError)

	registryServer := &http.Server{
		Handler:           cfg.Handler,
		TLSConfig:         cfg.TLSConfig,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		ErrorLog:          errorLog,
	}
	healthServer := &http.Server{
		Handler:           NewHealthHandler(cfg.Readiness.Ready, cfg.Metrics),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ErrorLog:          errorLog,
	}

	serveErrs := make(chan error, 2)

	go func() { serveErrs <- registryServer.ServeTLS(cfg.HTTPS, "", "") }()
	go func() { serveErrs <- healthServer.Serve(cfg.Health) }()

	select {
	case err := <-serveErrs:
		_ = registryServer.Close()
		_ = healthServer.Close()

		return err
	case <-ctx.Done():
	}

	cfg.Readiness.ShutDown()
	cfg.Logger.Info("shutting down: not ready, still serving while endpoints update")

	timer := time.NewTimer(endpointRemovalDelay)
	defer timer.Stop()

	select {
	case <-timer.C:
	case err := <-serveErrs:
		_ = healthServer.Close()
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownGracePeriod)
	defer cancel()

	err := registryServer.Shutdown(shutdownCtx)
	_ = healthServer.Close()

	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}
