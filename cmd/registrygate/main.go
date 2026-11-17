// Command registrygate is the authenticating reverse proxy for container registries.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/audit"
	"github.com/solidDoWant/registrygate/internal/authn"
	"github.com/solidDoWant/registrygate/internal/authz"
	"github.com/solidDoWant/registrygate/internal/config"
	"github.com/solidDoWant/registrygate/internal/controller"
	"github.com/solidDoWant/registrygate/internal/metrics"
	"github.com/solidDoWant/registrygate/internal/mirror"
	"github.com/solidDoWant/registrygate/internal/model"
	"github.com/solidDoWant/registrygate/internal/registry"
	"github.com/solidDoWant/registrygate/internal/secrets"
	"github.com/solidDoWant/registrygate/internal/server"
	"github.com/solidDoWant/registrygate/internal/upstream"
	"github.com/solidDoWant/registrygate/internal/webhook"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

// defaultConfigPath is where the chart mounts the rendered config file.
const defaultConfigPath = "/etc/registrygate/config.yaml"

// defaultUploadSigningSecret is the upload signing Secret the chart creates.
const defaultUploadSigningSecret = "registrygate-upload-signing"

// runFlags are the command-line settings serve needs.
type runFlags struct {
	uploadSigningSecret string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run parses args, serves until ctx ends, and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("registrygate", flag.ContinueOnError)
	flags.SetOutput(stderr)

	configPath := flags.String("config", defaultConfigPath, "Path to the YAML config file.")
	showVersion := flags.Bool("version", false, "Print the version and exit.")
	uploadSigningSecret := flags.String("upload-signing-secret", defaultUploadSigningSecret,
		"Name of the Secret in the proxy's namespace whose key \"key\" signs upload session tokens.")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	_, _ = fmt.Fprintf(stdout, "registrygate %s\n", version)

	if *showVersion {
		return 0
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "registrygate: %v\n", err)
		return 1
	}

	logger := slog.New(slog.NewJSONHandler(stderr, nil))

	if err := serve(ctx, cfg, runFlags{uploadSigningSecret: *uploadSigningSecret}, logger); err != nil {
		logger.Error("registrygate stopped", "error", err.Error())
		return 1
	}

	return 0
}

// serve wires the proxy together and runs it until ctx ends.
func serve(ctx context.Context, cfg *config.Config, flags runFlags, logger *slog.Logger) error {
	proxyMetrics := metrics.New()

	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubernetes client config: %w", err)
	}

	reviewer, err := authn.NewReviewerForConfig(restConfig, authn.Options{
		Audience:      cfg.Authentication.Audience,
		CacheTTL:      cfg.Authentication.CacheTTL,
		ReviewTimeout: cfg.Authentication.ReviewTimeout,
		Metrics:       proxyMetrics,
	})
	if err != nil {
		return err
	}

	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}

	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}

	namespace := os.Getenv("POD_NAMESPACE")

	adminNamespaces := cfg.AdminNamespaces
	if len(adminNamespaces) == 0 && namespace != "" {
		adminNamespaces = []string{namespace}
	}

	// The model has its own informers, apart from the status controller's manager: losing the status lease stops
	// the manager, and serving must carry on with live objects.
	objectCache, err := cache.New(restConfig, cache.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("object cache: %w", err)
	}

	status := &controller.Status{}
	upstreamClients := upstream.NewClients(upstream.ClientOptions{Logger: logger, Metrics: proxyMetrics})
	ruleMirrors := mirror.NewCache(mirror.ClientOptions{Logger: logger, Metrics: proxyMetrics, Target: metrics.TargetPolicyMirror})
	defaultMirrors := mirror.NewCache(mirror.ClientOptions{Logger: logger, Metrics: proxyMetrics, Target: metrics.TargetAnonymousMirror})

	var objects *model.Model

	secretStore := secrets.New(kubeClient, secrets.Options{
		AdminNamespaces: adminNamespaces,
		OnChange:        func() { objects.MarkDirty() },
		Logger:          logger,
	})

	objects = model.New(model.Options{
		Reader:          objectCache,
		Informers:       objectCache,
		Secrets:         secretStore,
		AdminNamespaces: adminNamespaces,
		Logger:          logger,
		OnRebuild: func(snapshot *model.Snapshot) {
			status.Notify(snapshot)
			retainClients(snapshot, upstreamClients, ruleMirrors, defaultMirrors)
		},
	})
	status.Snapshot = objects.Snapshot

	signing := &uploadSigning{
		store:  secretStore,
		key:    secrets.Key{Namespace: namespace, Name: flags.uploadSigningSecret},
		logger: logger,
	}

	forwarder := registry.NewForwarder(registry.ForwarderConfig{
		Upstreams:      upstreamClients,
		RuleMirrors:    ruleMirrors,
		DefaultMirrors: defaultMirrors,
		UploadTokens:   signing.Codec,
		Logger:         logger,
		Metrics:        proxyMetrics,
	})

	statusController := &statusController{
		restConfig: restConfig,
		scheme:     scheme,
		namespace:  namespace,
		logger:     logger,
		setup:      status.SetupWithManager,
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

	// The informers, the Secret store and the status controller outlive the listeners' drain, so requests still
	// being served during shutdown are decided against live objects.
	background, stopBackground := context.WithCancel(context.WithoutCancel(ctx))
	defer stopBackground()

	var components sync.WaitGroup

	backgroundErrs := make(chan error, 5)

	start := func(run func(context.Context) error) {
		components.Go(func() { backgroundErrs <- run(background) })
	}

	start(objectCache.Start)
	start(secretStore.Run)
	start(objects.Run)

	if cfg.Webhook.Enabled {
		webhookServer, err := webhook.NewServer(cfg.Webhook, scheme, adminNamespaces, objects.Snapshot)
		if err != nil {
			return err
		}

		start(webhookServer.Start)
	}

	// The status controller never fails the proxy; it retries until the proxy stops.
	components.Go(func() { statusController.run(background) })

	readiness := server.NewReadiness(objects.Ready, signing.Loaded)
	handler := &server.Handler{
		Reviewer:  reviewer,
		Decider:   authz.Resolver{Snapshot: objects.Snapshot},
		Forwarder: forwarder,
		Ready:     readiness.Ready,
		Logger:    logger,
		Metrics:   proxyMetrics,
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
		TLSConfig:           server.SNICertificates(objects.Snapshot),
		Health:              healthListener,
		Handler:             handler,
		Readiness:           readiness,
		Metrics:             proxyMetrics,
		ReadHeaderTimeout:   cfg.Server.ReadHeaderTimeout,
		IdleTimeout:         cfg.Server.IdleTimeout,
		MaxHeaderBytes:      cfg.Server.MaxHeaderBytes,
		ShutdownGracePeriod: cfg.Server.ShutdownGracePeriod,
		Logger:              logger,
	})
}

// retainClients drops the upstream and mirror clients of objects the snapshot no longer has.
func retainClients(snapshot *model.Snapshot, upstreams *upstream.Clients, mirrorCaches ...*mirror.Cache) {
	liveUpstreams := map[types.UID]struct{}{}
	for _, item := range snapshot.Upstreams {
		liveUpstreams[item.Object.UID] = struct{}{}
	}

	upstreams.Retain(func(uid types.UID) bool {
		_, live := liveUpstreams[uid]
		return live
	})

	liveMirrors := map[types.UID]struct{}{}
	for _, item := range snapshot.Mirrors {
		liveMirrors[item.Object.UID] = struct{}{}
	}

	for _, mirrorCache := range mirrorCaches {
		mirrorCache.Retain(liveMirrors)
	}
}
