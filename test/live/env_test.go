//go:build live

// Package live runs client flows and probes against the real ghcr.io through
// the proxy started in-process. Only the operator runs these tests, by hand,
// with the PAT in the file GHCR_LIVE_PAT_FILE and its owner in
// GHCR_LIVE_OWNER; without them, or without envtest's binaries, every test
// skips.
//
// Everything the tests create lives under one run prefix,
// <owner>/ghcr-proxy-live-<run id>, with a random run id. The proxy's only
// grant is that prefix, and cleanup deletes only packages under it that the
// run pushed. The tests print status codes and registry error codes, never
// the PAT or a token.
package live

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/solidDoWant/ghcr-proxy/api/v1alpha1"
	"github.com/solidDoWant/ghcr-proxy/internal/authn"
	"github.com/solidDoWant/ghcr-proxy/internal/authz"
	"github.com/solidDoWant/ghcr-proxy/internal/config"
	"github.com/solidDoWant/ghcr-proxy/internal/registry"
	"github.com/solidDoWant/ghcr-proxy/internal/server"
	"github.com/solidDoWant/ghcr-proxy/internal/upstream"
)

const (
	registryURL = "https://ghcr.io"
	apiURL      = "https://api.github.com/"
	// publicImage is the one image outside the run prefix the tests touch: an
	// already-public image, pulled anonymously and read-only, by its digest.
	publicImage = "ghcr.io/soliddowant/ghcr-proxy:0.1.1@sha256:bcb48dfd6f3a240dd12107f3ba8b65bab1b9cb465243bed368ef57c1a5409ef7"

	ciNamespace    = "ci-live"
	serviceAccount = "builder"
	proxyAudience  = "ghcr-proxy"
	// The pod is never scheduled in envtest; the image is never pulled.
	unusedPodImage = "ghcr.io/soliddowant/unused@sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

// ownerPattern is a GitHub login; anything else is refused before any request.
var ownerPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,37}[a-z0-9])?$`)

// liveEnv is one run: the proxy in front of ghcr.io and what the run pushed.
type liveEnv struct {
	owner string
	// prefix is <owner>/ghcr-proxy-live-<run id>; every repository the run
	// writes is prefix or below it.
	prefix string
	pat    string

	proxy *httptest.Server
	// clientTransport trusts the proxy's test certificate and the system roots
	// (GHCR redirects blob downloads to another host).
	clientTransport *http.Transport
	// callerToken is the CI ServiceAccount's token for the proxy.
	callerToken string

	mu     sync.Mutex
	pushed map[string]bool

	stop func()
}

var (
	envOnce   sync.Once
	sharedEnv *liveEnv
	envErr    error
	skipWhy   string
)

func TestMain(m *testing.M) {
	code := m.Run()

	if sharedEnv != nil {
		sharedEnv.cleanup()
		sharedEnv.stop()
	}

	os.Exit(code)
}

// requireLive returns the run's environment, starting it on first use, and
// skips the test when the live variables or envtest's binaries are missing.
func requireLive(t *testing.T) *liveEnv {
	t.Helper()

	envOnce.Do(func() {
		for _, variable := range []string{"GHCR_LIVE_PAT_FILE", "GHCR_LIVE_OWNER", "KUBEBUILDER_ASSETS"} {
			if os.Getenv(variable) == "" {
				skipWhy = variable + " is not set; the live tests skip"
				return
			}
		}

		sharedEnv, envErr = startEnv()
	})

	if skipWhy != "" {
		t.Skip(skipWhy)
	}

	if envErr != nil {
		t.Fatalf("live environment: %v", envErr)
	}

	return sharedEnv
}

func randomRunID() (string, error) {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}

	return hex.EncodeToString(raw), nil
}

func startEnv() (*liveEnv, error) {
	owner := strings.ToLower(os.Getenv("GHCR_LIVE_OWNER"))
	if !ownerPattern.MatchString(owner) {
		return nil, fmt.Errorf("GHCR_LIVE_OWNER is not a GitHub login")
	}

	patFile := os.Getenv("GHCR_LIVE_PAT_FILE")

	raw, err := os.ReadFile(patFile)
	if err != nil {
		// The error names only the file, never its content.
		return nil, fmt.Errorf("read GHCR_LIVE_PAT_FILE: %w", err)
	}

	runID, err := randomRunID()
	if err != nil {
		return nil, err
	}

	env := &liveEnv{
		owner:  owner,
		prefix: owner + "/ghcr-proxy-live-" + runID,
		pat:    strings.TrimSpace(string(raw)),
		pushed: map[string]bool{},
	}

	ctx, cancel := context.WithCancel(context.Background())

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	stops := []func(){cancel}
	env.stop = func() {
		for _, stop := range stops {
			stop()
		}
	}

	restConfig, err := testEnv.Start()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("envtest: %w", err)
	}

	stops = append(stops, func() { _ = testEnv.Stop() })

	if err := env.startProxy(ctx, restConfig, patFile); err != nil {
		env.stop()
		return nil, err
	}

	stops = append([]func(){env.proxy.Close}, stops...)

	if err := env.setUpCaller(ctx, restConfig); err != nil {
		env.stop()
		return nil, err
	}

	return env, nil
}

// startProxy wires the proxy's components the way the ghcr-proxy command
// does, pointed at ghcr.io, with the run prefix as the only grant.
func (e *liveEnv) startProxy(ctx context.Context, restConfig *rest.Config, patFile string) error {
	configYAML := fmt.Sprintf(`
upstream:
  url: %s
  apiURL: %s
  patFile: %s
grants:
  - namespaces: %s
    repositories: [%q, %q]
    actions: [pull, push, delete]
`, registryURL, apiURL, patFile, ciNamespace, e.prefix, e.prefix+"/**")

	cfg, err := config.Parse([]byte(configYAML))
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pat := upstream.NewPATFile(cfg.Upstream.PATFile, logger)

	tokens, err := upstream.NewTokens(upstream.TokensConfig{
		URL: cfg.Upstream.URL, TokenHosts: cfg.Upstream.TokenHosts, TokenTimeout: cfg.Upstream.TokenTimeout,
		PAT: pat, Logger: logger,
	})
	if err != nil {
		return err
	}

	forwarder := registry.NewForwarder(registry.ForwarderConfig{
		Upstream: cfg.Upstream.URL, Tokens: tokens, Timeout: cfg.Upstream.Timeout, Logger: logger,
		API: registry.APIConfig{URL: cfg.Upstream.APIURL, Timeout: cfg.Upstream.APITimeout, PAT: pat},
	})

	reviewer, err := authn.NewReviewerForConfig(restConfig, authn.Options{
		Audience: cfg.Authentication.Audience, CacheTTL: cfg.Authentication.CacheTTL,
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

	policyCache, err := cache.New(restConfig, cache.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	index := authz.NewIndex(cfg.Grants)
	if err := index.Watch(ctx, policyCache); err != nil {
		return err
	}

	go func() { _ = policyCache.Start(ctx) }()

	ready := func() bool { return index.Synced() && pat.Loaded() }

	e.proxy = httptest.NewTLSServer(&server.Handler{
		Reviewer: reviewer, Decider: &authz.Authorizer{Index: index}, Forwarder: forwarder, Ready: ready, Logger: logger,
	})

	roots, err := x509.SystemCertPool()
	if err != nil {
		return err
	}

	roots.AddCert(e.proxy.Certificate())
	e.clientTransport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}

	deadline := time.Now().Add(time.Minute)
	for !ready() {
		if time.Now().After(deadline) {
			return fmt.Errorf("proxy not ready: policies synced %t, PAT loaded %t", index.Synced(), pat.Loaded())
		}

		time.Sleep(100 * time.Millisecond)
	}

	return nil
}

// setUpCaller creates the CI namespace, its ServiceAccount, a pod token for
// the proxy and a policy allowing the run prefix.
func (e *liveEnv) setUpCaller(ctx context.Context, restConfig *rest.Config) error {
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	if _, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ciNamespace}}, metav1.CreateOptions{}); err != nil {
		return err
	}

	if _, err := clientset.CoreV1().ServiceAccounts(ciNamespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: serviceAccount}}, metav1.CreateOptions{}); err != nil {
		return err
	}

	pod, err := clientset.CoreV1().Pods(ciNamespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "build"},
		Spec: corev1.PodSpec{
			ServiceAccountName: serviceAccount,
			Containers:         []corev1.Container{{Name: "build", Image: unusedPodImage}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	request, err := clientset.CoreV1().ServiceAccounts(ciNamespace).CreateToken(ctx, serviceAccount, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{proxyAudience},
			ExpirationSeconds: ptr.To[int64](3600),
			BoundObjectRef:    &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	e.callerToken = request.Status.Token

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}

	k8s, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	return k8s.Create(ctx, &v1alpha1.GHCRAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ciNamespace, Name: "live"},
		Spec: v1alpha1.GHCRAccessPolicySpec{
			ServiceAccounts: []string{serviceAccount},
			Rules: []v1alpha1.AccessRule{{
				Repositories: []string{e.prefix, e.prefix + "/**"},
				Actions:      []v1alpha1.Action{v1alpha1.ActionPull, v1alpha1.ActionPush, v1alpha1.ActionDelete},
			}},
		},
	})
}

// repo returns the repository <prefix>/<name> for this run.
func (e *liveEnv) repo(name string) string { return e.prefix + "/" + name }

// inRunPrefix reports whether repo is the run prefix or below it. It guards
// everything that writes or deletes with the PAT.
func (e *liveEnv) inRunPrefix(repo string) bool {
	return repo == e.prefix || strings.HasPrefix(repo, e.prefix+"/")
}

// notePushed records that the run created repo, so cleanup removes it.
func (e *liveEnv) notePushed(t *testing.T, repo string) {
	t.Helper()

	if !e.inRunPrefix(repo) {
		t.Fatalf("refusing to note %q: outside the run prefix", repo)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.pushed[repo] = true
}
