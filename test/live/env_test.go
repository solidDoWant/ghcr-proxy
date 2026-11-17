//go:build live

// Package live runs client flows and probes against the real ghcr.io through
// the proxy started in-process. Only the operator runs these tests, by hand,
// with the PAT in the file GHCR_LIVE_PAT_FILE and its owner in
// GHCR_LIVE_OWNER; without them, or without envtest's binaries, every test
// skips.
//
// Everything the tests create lives under one run prefix,
// <owner>/registrygate-live-<run id>, with a random run id. The proxy's only
// grant is that prefix, and cleanup deletes only packages under it that the
// run pushed. The tests print status codes and registry error codes, never
// the PAT or a token.
package live

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
)

const (
	registryURL = "https://ghcr.io"
	apiURL      = "https://api.github.com/"
	// publicImage is the one image outside the run prefix the tests touch: an
	// already-public image, pulled anonymously and read-only, by its digest.
	publicImage = "ghcr.io/soliddowant/ghcr-proxy:0.2.0@sha256:e3a1e8f2dd0f78b659622cb7ee4654c29e26a972f6202c4e4c70dfa68efdcb8f"

	ciNamespace    = "ci-live"
	serviceAccount = "builder"
	proxyAudience  = "registrygate"
	// The pod is never scheduled in envtest; the image is never pulled.
	unusedPodImage = "ghcr.io/soliddowant/unused@sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

// ownerPattern is a GitHub login; anything else is refused before any request.
var ownerPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,37}[a-z0-9])?$`)

// liveEnv is one run: the proxy in front of ghcr.io and what the run pushed.
type liveEnv struct {
	owner string
	// prefix is <owner>/registrygate-live-<run id>; every repository the run
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
		prefix: owner + "/registrygate-live-" + runID,
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

// errNotPorted stops a live run until the environment builds the custom resources the proxy now reads; the
// proxy no longer takes an upstream, a PAT file or grants from its config file.
var errNotPorted = errors.New("the live environment does not build UpstreamRegistry, UpstreamCredential and " +
	"RegistryAccessPolicy objects yet")

// startProxy would start the proxy for the run. It refuses until the environment is ported.
func (e *liveEnv) startProxy(context.Context, *rest.Config, string) error {
	return errNotPorted
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
			Rules: []v1alpha1.GHCRAccessRule{{
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
