package server_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/audit"
	"github.com/solidDoWant/registrygate/internal/authz"
	"github.com/solidDoWant/registrygate/internal/metrics"
	"github.com/solidDoWant/registrygate/internal/mirror"
	"github.com/solidDoWant/registrygate/internal/model"
	"github.com/solidDoWant/registrygate/internal/registry"
	"github.com/solidDoWant/registrygate/internal/secrets"
	"github.com/solidDoWant/registrygate/internal/server"
	"github.com/solidDoWant/registrygate/internal/uploadtoken"
	"github.com/solidDoWant/registrygate/internal/upstream"
	"github.com/solidDoWant/registrygate/test/fakeregistry"
)

const (
	// registryHost is the host the CI pods reach the proxy at, and the TLS server name.
	registryHost   = "registry.test"
	adminNamespace = "registrygate"
	ciNamespace    = "ci-foo"
)

type harnessOptions struct {
	// fake configures the upstream registry; the zero value is GHCR-like (Bearer, singular upload path,
	// no delete).
	fake fakeregistry.Options
	// noSigningKey leaves the upload signing key unloaded.
	noSigningKey bool
	// configure changes the objects and Secrets before the snapshot is built.
	configure func(*objects)
	// audit receives the audit lines.
	audit io.Writer
	// reviewer replaces the stub token reviewer.
	reviewer server.Reviewer
	// metrics counts what the pipeline does.
	metrics *metrics.Metrics
}

// objects are the custom resources and Secrets a snapshot is built from.
type objects struct {
	upstream   *v1alpha1.UpstreamRegistry
	credential *v1alpha1.UpstreamCredential
	mirrors    []v1alpha1.RegistryMirror
	policy     *v1alpha1.RegistryAccessPolicy
	secrets    secretMap
}

type harness struct {
	fake      *fakeregistry.Fake
	server    *httptest.Server
	readiness *server.Readiness
	objects   *objects

	snapshot atomic.Pointer[model.Snapshot]
	mu       sync.Mutex
}

// rebuild builds a new snapshot from the harness's objects, as the model does after a change.
func (h *harness) rebuild(t *testing.T) {
	t.Helper()

	h.mu.Lock()
	defer h.mu.Unlock()

	in := model.Inputs{
		Upstreams:       []v1alpha1.UpstreamRegistry{*h.objects.upstream},
		Credentials:     []v1alpha1.UpstreamCredential{*h.objects.credential},
		Mirrors:         h.objects.mirrors,
		Policies:        []v1alpha1.RegistryAccessPolicy{*h.objects.policy},
		Namespaces:      map[string]labels.Set{adminNamespace: {}, ciNamespace: {}},
		Secrets:         h.objects.secrets,
		AdminNamespaces: []string{adminNamespace},
	}

	snapshot := model.Build(in)
	upstreamStatus := snapshot.Upstreams[types.NamespacedName{Namespace: adminNamespace, Name: "upstream-a"}].Status
	require.True(t, upstreamStatus.Ready, "upstream not ready: %s", upstreamStatus.Message)

	h.snapshot.Store(snapshot)
}

func ref(name string) *v1alpha1.ObjectRef {
	return &v1alpha1.ObjectRef{Namespace: adminNamespace, Name: name}
}

func defaultObjects(t *testing.T, fake *fakeregistry.Fake) *objects {
	t.Helper()

	certificate, key := servingPair(t, registryHost)
	admits := v1alpha1.AllowedNamespaces{Names: []string{ciNamespace}}

	return &objects{
		upstream: &v1alpha1.UpstreamRegistry{
			ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: "upstream-a", UID: "upstream-a-uid", Generation: 1},
			Spec: v1alpha1.UpstreamRegistrySpec{
				Hosts:             []string{registryHost},
				URL:               "https://" + fake.RegistryHost(),
				CASecretRef:       &v1alpha1.LocalRef{Name: "upstream-a-ca"},
				TLS:               v1alpha1.ServingTLS{SecretRef: v1alpha1.LocalRef{Name: "upstream-a-tls"}},
				AllowedNamespaces: admits,
			},
		},
		credential: &v1alpha1.UpstreamCredential{
			ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: "shared", UID: "shared-uid", Generation: 1},
			Spec: v1alpha1.UpstreamCredentialSpec{
				UpstreamRef: *ref("upstream-a"),
				SecretRef:   v1alpha1.CredentialsSecretRef{Name: "shared-credential"},
				Ceiling: []v1alpha1.AccessGrant{{
					Repositories: []string{"soliddowant/foo/**"},
					Actions:      []v1alpha1.Action{v1alpha1.ActionPull, v1alpha1.ActionPush, v1alpha1.ActionDelete},
				}},
				AllowedNamespaces: admits,
			},
		},
		policy: &v1alpha1.RegistryAccessPolicy{
			ObjectMeta: metav1.ObjectMeta{Namespace: ciNamespace, Name: "build", Generation: 1},
			Spec: v1alpha1.RegistryAccessPolicySpec{
				ServiceAccounts: []string{"builder"},
				Rules: []v1alpha1.AccessRule{
					{
						Name: "app", CredentialRef: ref("shared"),
						Repositories: []string{"soliddowant/foo/app"}, Actions: []v1alpha1.Action{v1alpha1.ActionPush},
					},
					{
						Name: "source", CredentialRef: ref("shared"),
						Repositories: []string{"soliddowant/foo/source"}, Actions: []v1alpha1.Action{v1alpha1.ActionPull},
					},
					{
						Name: "disposable", CredentialRef: ref("shared"),
						Repositories: []string{"soliddowant/foo/disposable"}, Actions: []v1alpha1.Action{v1alpha1.ActionDelete},
					},
				},
			},
		},
		secrets: secretMap{
			{Namespace: adminNamespace, Name: "upstream-a-tls"}: {
				State: secrets.Present, ResourceVersion: "1",
				Data: secrets.Data{"tls.crt": []byte(certificate), "tls.key": []byte(key)},
			},
			{Namespace: adminNamespace, Name: "upstream-a-ca"}: {
				State: secrets.Present, ResourceVersion: "1", Data: secrets.Data{"ca.crt": fake.CAPEM()},
			},
			{Namespace: adminNamespace, Name: "shared-credential"}: {
				State: secrets.Present, ResourceVersion: "1",
				Data: secrets.Data{"username": []byte("registrygate"), "password": []byte(fake.PAT())},
			},
		},
	}
}

// signingKey is a fixed upload signing key; tests never depend on its value.
var signingKey = []byte("0123456789abcdef0123456789abcdef")

func newHarness(t *testing.T, options harnessOptions) *harness {
	t.Helper()

	fakeOptions := options.fake
	if fakeOptions.Auth == "" {
		fakeOptions.Auth = fakeregistry.Bearer
	}

	if fakeOptions.UploadPath == "" {
		fakeOptions.UploadPath = fakeregistry.Upload
	}

	if fakeOptions.Delete == "" {
		fakeOptions.Delete = fakeregistry.Unsupported
	}

	fake := fakeregistry.New(t, fakeOptions)

	proxy := &harness{fake: fake, objects: defaultObjects(t, fake)}
	if options.configure != nil {
		options.configure(proxy.objects)
	}

	proxy.rebuild(t)

	codec, err := uploadtoken.New(signingKey)
	require.NoError(t, err)

	uploadTokens := func() *uploadtoken.Codec { return codec }
	if options.noSigningKey {
		uploadTokens = func() *uploadtoken.Codec { return nil }
	}

	forwarder := registry.NewForwarder(registry.ForwarderConfig{
		Upstreams:      upstream.NewClients(upstream.ClientOptions{Metrics: options.metrics}),
		RuleMirrors:    mirror.NewCache(mirror.ClientOptions{Metrics: options.metrics, Target: metrics.TargetPolicyMirror}),
		DefaultMirrors: mirror.NewCache(mirror.ClientOptions{Metrics: options.metrics, Target: metrics.TargetAnonymousMirror}),
		UploadTokens:   uploadTokens,
		Metrics:        options.metrics,
	})

	var reviewer server.Reviewer = stubReviewer{}
	if options.reviewer != nil {
		reviewer = options.reviewer
	}

	readiness := server.NewReadiness(
		func() bool { snapshot := proxy.snapshot.Load(); return snapshot != nil && !snapshot.PendingSecrets() },
		func() bool { return uploadTokens() != nil },
	)
	handler := &server.Handler{
		Reviewer: reviewer, Decider: authz.Resolver{Snapshot: proxy.snapshot.Load}, Forwarder: forwarder,
		Ready: readiness.Ready, Metrics: options.metrics,
	}

	if options.audit != nil {
		handler.Audit = audit.New(options.audit)
	}

	httpsServer := httptest.NewUnstartedServer(handler)
	httpsServer.TLS = server.SNICertificates(proxy.snapshot.Load)
	httpsServer.StartTLS()
	t.Cleanup(httpsServer.Close)

	proxy.server = httpsServer
	proxy.readiness = readiness

	return proxy
}

// url is the proxy's base URL as CI pods use it.
func (h *harness) url() string { return "https://" + registryHost }

// transport reaches the proxy at registryHost (the proxy's serving certificate is for that name) and the fake's
// blob storage directly.
func (h *harness) transport() *http.Transport {
	transport := h.fake.Transport().Clone()
	proxyAddr := h.server.Listener.Addr().String()
	dialer := &net.Dialer{}

	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == registryHost+":443" {
			addr = proxyAddr
		}

		return dialer.DialContext(ctx, network, addr)
	}

	fakeTLS := transport.TLSClientConfig

	transport.TLSClientConfig = nil
	transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}

		config := fakeTLS.Clone()
		config.ServerName = host

		if host == registryHost {
			// The serving certificate is self-signed for registryHost; the tests check what it serves, not who signed it.
			config = &tls.Config{ServerName: registryHost, InsecureSkipVerify: true} //nolint:gosec // test client
		}

		raw, err := transport.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}

		conn := tls.Client(raw, config)
		if err := conn.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, err
		}

		return conn, nil
	}

	return transport
}

// client is an HTTP client for the proxy that follows no redirects.
func (h *harness) client() *http.Client {
	return &http.Client{
		Transport:     h.transport(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func objectMeta(namespace, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(namespace + "-" + name + "-uid"), Generation: 1}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()

	parsed, err := url.Parse(raw)
	require.NoError(t, err)

	return parsed
}
