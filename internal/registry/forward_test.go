package registry_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/ghcr-proxy/internal/registry"
	"github.com/solidDoWant/ghcr-proxy/internal/upstream"
	"github.com/solidDoWant/ghcr-proxy/test/fakeghcr"
)

// recordingTransport keeps a copy of every outbound request's URL and headers.
type recordingTransport struct {
	inner http.RoundTripper

	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	url    *url.URL
	header http.Header
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.requests = append(r.requests, recordedRequest{url: req.URL, header: req.Header.Clone()})
	r.mu.Unlock()

	return r.inner.RoundTrip(req)
}

func (r *recordingTransport) recorded() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]recordedRequest(nil), r.requests...)
}

// tokenScopes returns every scope requested from the token endpoint.
func (r *recordingTransport) tokenScopes() []string {
	var scopes []string

	for _, request := range r.recorded() {
		if request.url.Path == "/token" {
			scopes = append(scopes, request.url.Query()["scope"]...)
		}
	}

	return scopes
}

// proxyHarness is the forwarder behind a TLS listener, with a fixed
// credential decision standing in for authentication and authorization.
type proxyHarness struct {
	fake     *fakeghcr.Fake
	server   *httptest.Server
	recorder *recordingTransport
	pat      *upstream.PATFile
	patPath  string
	logs     *syncBuffer
	// clientTransport trusts both the proxy and the fake (blob redirects go
	// straight to the fake's storage).
	clientTransport *http.Transport
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func newProxyHarness(
	t *testing.T, fake *fakeghcr.Fake, pat string, forward func(registry.Request) registry.Forward,
	configure ...func(*registry.ForwarderConfig),
) *proxyHarness {
	t.Helper()

	patPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(patPath, []byte(pat+"\n"), 0o600))

	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	recorder := &recordingTransport{inner: fake.Transport()}
	patFile := upstream.NewPATFile(patPath, logger)

	upstreamURL := &url.URL{Scheme: "https", Host: fake.RegistryHost()}
	tokens, err := upstream.NewTokens(upstream.TokensConfig{
		URL: upstreamURL, Transport: recorder, TokenTimeout: 10 * time.Second, PAT: patFile, Logger: logger,
	})
	require.NoError(t, err)

	forwarderConfig := registry.ForwarderConfig{
		Upstream: upstreamURL, Transport: recorder, Tokens: tokens, Timeout: time.Minute, Logger: logger,
	}
	for _, apply := range configure {
		apply(&forwarderConfig)
	}

	if forwarderConfig.API.URL != nil && forwarderConfig.API.PAT == nil {
		forwarderConfig.API.PAT = patFile
	}

	forwarder := registry.NewForwarder(forwarderConfig)

	server := httptest.NewTLSServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		classified, err := registry.Classify(req)
		if err != nil {
			var registryErr *registry.Error
			require.ErrorAs(t, err, &registryErr)
			registry.WriteError(resp, registryErr)

			return
		}

		if classified.Route == registry.RouteBase {
			resp.WriteHeader(http.StatusOK)
			return
		}

		forwarder.ServeForward(resp, req, forward(classified))
	}))
	t.Cleanup(server.Close)

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(fake.CAPEM())
	pool.AddCert(server.Certificate())

	return &proxyHarness{
		fake: fake, server: server, recorder: recorder, pat: patFile, patPath: patPath, logs: logs,
		clientTransport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
}

func (h *proxyHarness) host() string {
	return strings.TrimPrefix(h.server.URL, "https://")
}

func (h *proxyHarness) client() *http.Client {
	return &http.Client{
		Transport:     h.clientTransport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func withPAT(request registry.Request) registry.Forward {
	return registry.Forward{Request: request, Credential: registry.CredentialPAT}
}

func anonymousCaller(request registry.Request) registry.Forward {
	return registry.Forward{Request: request, Credential: registry.CredentialAnonymous, AnonymousCaller: true}
}

func TestForwarderPushUsesOnlyPATTokensScopedToTheRepository(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})
	harness := newProxyHarness(t, fake, fake.PAT(), withPAT)

	image, err := random.Image(1024, 2)
	require.NoError(t, err)

	ref, err := name.ParseReference(harness.host() + "/owner/app:v1")
	require.NoError(t, err)

	require.NoError(t, remote.Write(ref, image, remote.WithAuth(authn.Anonymous), remote.WithTransport(harness.clientTransport)))

	sawPush := false

	for _, entry := range fake.Log() {
		// The token dance's ping and token requests carry no bearer token.
		if entry.Server != "registry" || entry.Path == "/token" || entry.Path == "/v2/" {
			continue
		}

		assert.Equal(t, fakeghcr.CredPATToken, entry.Credential, "%s %s", entry.Method, entry.Path)

		if entry.Method == http.MethodPut || entry.Method == http.MethodPost || entry.Method == http.MethodPatch {
			sawPush = true
		}
	}

	assert.True(t, sawPush, "the push reached the upstream")

	scopes := harness.recorder.tokenScopes()
	require.NotEmpty(t, scopes)
	assert.Contains(t, scopes, "repository:owner/app:pull,push")

	for _, scope := range scopes {
		assert.Contains(t, []string{"repository:owner/app:pull", "repository:owner/app:pull,push"}, scope)
	}
}

func TestForwarderAnonymousPullNeverUsesThePAT(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})
	harness := newProxyHarness(t, fake, fake.PAT(), anonymousCaller)

	image, err := random.Image(512, 1)
	require.NoError(t, err)
	require.NoError(t, fake.Push("owner/public:v1", image))
	require.NoError(t, fake.Push("owner/private:v1", image))
	fake.MakePublic("owner/public")
	fake.ResetLog()

	t.Run("public image", func(t *testing.T) {
		ref, err := name.ParseReference(harness.host() + "/owner/public:v1")
		require.NoError(t, err)

		pulled, err := remote.Image(ref, remote.WithAuth(authn.Anonymous), remote.WithTransport(harness.clientTransport))
		require.NoError(t, err)

		layers, err := pulled.Layers()
		require.NoError(t, err)

		for _, layer := range layers {
			reader, err := layer.Compressed()
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, reader)
			require.NoError(t, err)
			require.NoError(t, reader.Close())
		}

		sawAnonymousToken := false

		for _, entry := range fake.Log() {
			assert.NotEqual(t, fakeghcr.CredPATToken, entry.Credential, "%s %s", entry.Method, entry.Path)
			assert.NotEqual(t, fakeghcr.CredPATBasic, entry.Credential, "%s %s", entry.Method, entry.Path)

			if entry.Credential == fakeghcr.CredAnonymousToken {
				sawAnonymousToken = true
			}
		}

		assert.True(t, sawAnonymousToken)
	})

	t.Run("private image gets the proxy's challenge", func(t *testing.T) {
		resp, err := harness.client().Get(harness.server.URL + "/v2/owner/private/manifests/v1")
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Equal(t, []string{`Basic realm="ghcr-proxy"`}, resp.Header.Values("WWW-Authenticate"))

		for _, entry := range fake.Log() {
			assert.NotEqual(t, fakeghcr.CredPATToken, entry.Credential)
			assert.NotEqual(t, fakeghcr.CredPATBasic, entry.Credential)
		}
	})
}

func TestForwarderRewritesUploadLocationToTheClientHost(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})
	harness := newProxyHarness(t, fake, fake.PAT(), withPAT)

	image, err := random.Image(256, 1)
	require.NoError(t, err)
	require.NoError(t, fake.Push("owner/app:v1", image))

	layers, err := image.Layers()
	require.NoError(t, err)
	layerDigest, err := layers[0].Digest()
	require.NoError(t, err)

	t.Run("upload location points at the client's host", func(t *testing.T) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, harness.server.URL+"/v2/owner/app/blobs/uploads/", nil)
		require.NoError(t, err)

		req.Host = "ghcr.io"
		req.Header.Set("Authorization", "Basic Y2xpZW50OnNlY3JldA==")
		req.Header.Set("Cookie", "session=client")
		req.Header.Set("X-Forwarded-For", "10.0.0.1")

		resp, err := harness.client().Do(req)
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		require.Equal(t, http.StatusAccepted, resp.StatusCode)
		assert.True(t, strings.HasPrefix(resp.Header.Get("Location"), "https://ghcr.io/v2/owner/app/blobs/upload/"),
			"Location %q", resp.Header.Get("Location"))
		assert.Empty(t, resp.Header.Values("WWW-Authenticate"))

		for _, request := range harness.recorder.recorded() {
			if request.url.Path == "/token" || request.url.Path == "/v2/" {
				continue
			}

			assert.True(t, strings.HasPrefix(request.header.Get("Authorization"), "Bearer "), "upstream got the proxy's bearer token")
			assert.Empty(t, request.header.Values("Cookie"))
			assert.Empty(t, request.header.Values("X-Forwarded-For"))
		}
	})

	t.Run("blob redirect to storage passes unchanged", func(t *testing.T) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, harness.server.URL+"/v2/owner/app/blobs/"+layerDigest.String(), nil)
		require.NoError(t, err)

		req.Host = "ghcr.io"

		resp, err := harness.client().Do(req)
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		require.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
		assert.True(t, strings.HasPrefix(resp.Header.Get("Location"), "https://"+fake.StorageHost()+"/"+layerDigest.String()+"?sig="),
			"Location %q", resp.Header.Get("Location"))
	})
}

func TestForwarderUsesAReplacedPATWithoutRestart(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})
	harness := newProxyHarness(t, fake, "ghp_previous_token", withPAT)

	image, err := random.Image(256, 1)
	require.NoError(t, err)
	require.NoError(t, fake.Push("owner/app:v1", image))

	manifestURL := harness.server.URL + "/v2/owner/app/manifests/v1"

	resp, err := harness.client().Get(manifestURL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, "the old PAT is rejected")

	watchDone := make(chan struct{})
	ctx := t.Context()

	go func() {
		defer close(watchDone)

		assert.NoError(t, harness.pat.Watch(ctx))
	}()

	// Kubernetes updates Secret volumes by swapping a symlinked directory;
	// a rename into place is the same event for the watcher.
	staged := harness.patPath + ".new"
	require.NoError(t, os.WriteFile(staged, []byte(fake.PAT()), 0o600))
	require.NoError(t, os.Rename(staged, harness.patPath))

	statusIs := func(want int) func() bool {
		return func() bool {
			resp, err := harness.client().Get(manifestURL)
			if err != nil {
				return false
			}

			_ = resp.Body.Close()

			return resp.StatusCode == want
		}
	}

	require.Eventually(t, statusIs(http.StatusOK), 10*time.Second, 50*time.Millisecond)

	// The watch is running now, so this change is seen through file events.
	require.NoError(t, os.WriteFile(staged, []byte("ghp_rotated_away"), 0o600))
	require.NoError(t, os.Rename(staged, harness.patPath))

	require.Eventually(t, statusIs(http.StatusBadGateway), 10*time.Second, 50*time.Millisecond)
}

func TestForwarderAnswers502WhenThePATIsRejectedAndNeverLeaksIt(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})

	const rejectedPAT = "ghp_rejected_token_value"

	harness := newProxyHarness(t, fake, rejectedPAT, withPAT)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, harness.server.URL+"/v2/owner/app/blobs/uploads/", nil)
	require.NoError(t, err)

	resp, err := harness.client().Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	assert.JSONEq(t, `{"errors":[{"code":"UNAVAILABLE","message":"upstream rejected the proxy's credential"}]}`, string(body))
	assert.NotContains(t, string(body), rejectedPAT)

	for headerName, values := range resp.Header {
		for _, value := range values {
			assert.NotContains(t, value, rejectedPAT, headerName)
		}
	}

	assert.NotEmpty(t, harness.logs.String(), "the failure is logged")
	assert.NotContains(t, harness.logs.String(), rejectedPAT)

	for _, entry := range fake.Log() {
		if entry.Path != "/token" {
			assert.NotEqual(t, http.MethodPost, entry.Method, "no upload reached the upstream")
		}
	}
}
