package registry_test

import (
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/internal/config"
	"github.com/solidDoWant/registrygate/internal/mirror"
	"github.com/solidDoWant/registrygate/internal/registry"
	"github.com/solidDoWant/registrygate/test/fakeghcr"
)

const mirrorPrefix = "ghcr"

// Ways a test mirror's front answers registry requests other than the ping
// and the token endpoint.
const (
	mirrorServes          = ""
	mirrorFails           = "fail"
	mirrorForbids         = "forbid"
	mirrorRedirectsInside = "redirect-inside"
	mirrorRedirectsOut    = "redirect-outside"
)

// testMirror is a fake GHCR used as a mirror, behind a recording front on its
// own host so the token realm and Location headers are on the mirror's host.
type testMirror struct {
	fake   *fakeghcr.Fake
	front  *httptest.Server
	client *mirror.Client
	mode   atomic.Value
	// public makes pushed copies pullable without credentials.
	public bool

	mu       sync.Mutex
	requests []recordedRequest
}

type testMirrorOptions struct {
	fallback       bool
	anonymous      bool
	badCredentials bool
}

func newTestMirror(t *testing.T, options testMirrorOptions) *testMirror {
	t.Helper()

	fake := fakeghcr.New(t, fakeghcr.Options{})
	tm := &testMirror{fake: fake, public: options.anonymous}
	tm.mode.Store(mirrorServes)

	backend, err := url.Parse("https://" + fake.RegistryHost())
	require.NoError(t, err)

	proxy := &httputil.ReverseProxy{
		Rewrite: func(proxyReq *httputil.ProxyRequest) {
			proxyReq.SetURL(backend)
			proxyReq.Out.Host = backend.Host
		},
		Transport: fake.Transport(),
	}

	tm.front = httptest.NewTLSServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		tm.mu.Lock()
		tm.requests = append(tm.requests, recordedRequest{url: req.URL, header: req.Header.Clone()})
		tm.mu.Unlock()

		if req.URL.Path != "/v2/" && req.URL.Path != "/token" {
			switch tm.mode.Load().(string) {
			case mirrorFails:
				resp.WriteHeader(http.StatusInternalServerError)
				return
			case mirrorForbids:
				resp.WriteHeader(http.StatusForbidden)
				return
			case mirrorRedirectsInside:
				resp.Header().Set("Location", "/v2/"+mirrorPrefix+"/owner/app/blobs/uploads/elsewhere?state=1")
				resp.WriteHeader(http.StatusTemporaryRedirect)

				return
			case mirrorRedirectsOut:
				resp.Header().Set("Location", tm.frontURL()+"/v2/other/owner/app/manifests/v1")
				resp.WriteHeader(http.StatusTemporaryRedirect)

				return
			}
		}

		proxy.ServeHTTP(resp, req)
	}))
	t.Cleanup(tm.front.Close)

	fake.SetRealm(tm.frontURL() + "/token")

	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	// The fake's own CA is trusted too, so a token realm on the fake's host
	// is reachable and only the realm check keeps credentials from it.
	frontPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tm.front.Certificate().Raw})
	require.NoError(t, os.WriteFile(caPath, append(frontPEM, fake.CAPEM()...), 0o600))

	frontURL, err := url.Parse(tm.frontURL())
	require.NoError(t, err)

	mirrorConfig := &config.Mirror{
		URL: frontURL, PathPrefix: mirrorPrefix, CAFile: caPath,
		FallbackToUpstream: options.fallback, ResponseHeaderTimeout: 10 * time.Second,
	}

	if !options.anonymous {
		password := fake.PAT()
		if options.badCredentials {
			password = "wrong"
		}

		mirrorConfig.CredentialsFile = filepath.Join(dir, "credentials")
		require.NoError(t, os.WriteFile(mirrorConfig.CredentialsFile, []byte("mirror-user:"+password+"\n"), 0o600))
	}

	tm.client, err = mirror.NewClient(mirror.ClientConfig{
		Mirror: mirrorConfig, Files: mirror.NewFiles("test", mirrorConfig, nil), TokenTimeout: 10 * time.Second,
	})
	require.NoError(t, err)

	return tm
}

func (tm *testMirror) frontURL() string { return tm.front.URL }

func (tm *testMirror) recorded() []recordedRequest {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	return append([]recordedRequest(nil), tm.requests...)
}

// registryRequests are the requests the mirror saw beyond the ping and the
// token endpoint.
func (tm *testMirror) registryRequests() []string {
	var paths []string

	for _, request := range tm.recorded() {
		if request.url.Path != "/v2/" && request.url.Path != "/token" {
			paths = append(paths, request.url.Path)
		}
	}

	return paths
}

// mirrorImage is one image pushed to the upstream and, under the prefix, to
// mirrors.
type mirrorImage struct {
	manifest v1.Hash
	layer    v1.Hash
}

func pushMirrorImage(t *testing.T, repo string, upstreamFake *fakeghcr.Fake, mirrors ...*testMirror) mirrorImage {
	t.Helper()

	image, err := random.Image(256, 1)
	require.NoError(t, err)

	manifest, err := image.Digest()
	require.NoError(t, err)

	layers, err := image.Layers()
	require.NoError(t, err)

	layer, err := layers[0].Digest()
	require.NoError(t, err)

	if upstreamFake != nil {
		require.NoError(t, upstreamFake.Push(repo+":v1", image))
	}

	for _, tm := range mirrors {
		// Push straight to the fake, with its own realm for the moment.
		realm := tm.fake.Realm()
		tm.fake.SetRealm("https://" + tm.fake.RegistryHost() + "/token")
		require.NoError(t, tm.fake.Push(mirrorPrefix+"/"+repo+":v1", image))
		tm.fake.SetRealm(realm)

		if tm.public {
			tm.fake.MakePublic(mirrorPrefix + "/" + repo)
		}
	}

	return mirrorImage{manifest: manifest, layer: layer}
}

// decideByOwner stands in for authorization: repositories under "anon/" are
// pulled by a caller with no credential, under "denied/" by a caller no
// policy allows, and everything else is allowed.
func decideByOwner(request registry.Request) registry.Forward {
	switch {
	case strings.HasPrefix(request.Name, "anon/"):
		return registry.Forward{Request: request, Credential: registry.CredentialAnonymous, AnonymousCaller: true}
	case strings.HasPrefix(request.Name, "denied/"):
		return registry.Forward{Request: request, Credential: registry.CredentialAnonymous}
	}

	return registry.Forward{Request: request, Credential: registry.CredentialPAT}
}

func withMirrors(policy, anonymous *testMirror) func(*registry.ForwarderConfig) {
	return func(cfg *registry.ForwarderConfig) {
		if policy != nil {
			cfg.PolicyMirror = policy.client
		}

		if anonymous != nil {
			cfg.AnonymousMirror = anonymous.client
		}
	}
}

// do sends one request through the proxy and returns the status, headers and
// body.
func (h *proxyHarness) do(t *testing.T, method, path string) (int, http.Header, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, h.server.URL+path, nil)
	require.NoError(t, err)

	resp, err := h.client().Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp.StatusCode, resp.Header, string(body)
}

// upstreamPulls are the manifest and blob requests the upstream saw.
func upstreamPulls(fake *fakeghcr.Fake) []string {
	var paths []string

	for _, entry := range fake.Log() {
		if entry.Server == "registry" && (strings.Contains(entry.Path, "/manifests/") || strings.Contains(entry.Path, "/blobs/")) {
			paths = append(paths, entry.Method+" "+entry.Path)
		}
	}

	return paths
}

func TestForwarderServesAllowedPullsFromThePolicyMirror(t *testing.T) {
	upstreamFake := fakeghcr.New(t, fakeghcr.Options{})
	policy := newTestMirror(t, testMirrorOptions{fallback: true})
	// The mirror's copy is private: only the mirror's own credentials reach it.
	image := pushMirrorImage(t, "owner/app", upstreamFake, policy)
	policy.fake.ResetLog()
	upstreamFake.ResetLog()

	harness := newProxyHarness(t, upstreamFake, upstreamFake.PAT(), decideByOwner, withMirrors(policy, nil))

	status, _, _ := harness.do(t, http.MethodGet, "/v2/owner/app/manifests/v1")
	assert.Equal(t, http.StatusOK, status)

	status, _, _ = harness.do(t, http.MethodHead, "/v2/owner/app/manifests/"+image.manifest.String())
	assert.Equal(t, http.StatusOK, status)

	status, header, _ := harness.do(t, http.MethodGet, "/v2/owner/app/blobs/"+image.layer.String())
	assert.Equal(t, http.StatusTemporaryRedirect, status)
	assert.Contains(t, header.Get("Location"), policy.fake.StorageHost(), "blob redirects to the mirror's storage pass through")

	assert.Empty(t, upstreamPulls(upstreamFake), "the upstream sees no manifest or blob request")

	served := 0

	for _, entry := range policy.fake.Log() {
		if strings.HasPrefix(entry.Path, "/v2/"+mirrorPrefix+"/owner/app/") {
			served++

			assert.Equal(t, fakeghcr.CredPATToken, entry.Credential, "the mirror's own credentials are used: %s %s", entry.Method, entry.Path)
		}
	}

	assert.Equal(t, 3, served)

	for _, request := range policy.recorded() {
		if request.url.Path == "/token" {
			assert.Equal(t, []string{"repository:" + mirrorPrefix + "/owner/app:pull"}, request.url.Query()["scope"])

			user, _, ok := (&http.Request{Header: request.header}).BasicAuth()
			require.True(t, ok)
			assert.Equal(t, "mirror-user", user)
		}
	}
}

func TestForwarderServesAnonymousPullsFromTheAnonymousMirror(t *testing.T) {
	upstreamFake := fakeghcr.New(t, fakeghcr.Options{})
	policy := newTestMirror(t, testMirrorOptions{fallback: true})
	anonymous := newTestMirror(t, testMirrorOptions{fallback: true, anonymous: true})

	harness := newProxyHarness(t, upstreamFake, upstreamFake.PAT(), decideByOwner, withMirrors(policy, anonymous))

	for _, repo := range []string{"anon/app", "denied/app"} {
		t.Run(repo, func(t *testing.T) {
			image := pushMirrorImage(t, repo, upstreamFake, anonymous)
			upstreamFake.ResetLog()

			status, _, _ := harness.do(t, http.MethodGet, "/v2/"+repo+"/manifests/v1")
			assert.Equal(t, http.StatusOK, status)

			status, _, _ = harness.do(t, http.MethodGet, "/v2/"+repo+"/blobs/"+image.layer.String())
			assert.Equal(t, http.StatusTemporaryRedirect, status)

			assert.Empty(t, upstreamPulls(upstreamFake))
			assert.Contains(t, anonymous.registryRequests(), "/v2/"+mirrorPrefix+"/"+repo+"/manifests/v1")
			assert.Empty(t, policy.recorded(), "the policy mirror is never used for these pulls")
		})
	}

	t.Run("a refused anonymous pull gets the proxy's challenge", func(t *testing.T) {
		anonymous.mode.Store(mirrorForbids)
		t.Cleanup(func() { anonymous.mode.Store(mirrorServes) })

		status, header, _ := harness.do(t, http.MethodGet, "/v2/anon/app/manifests/v1")
		assert.Equal(t, http.StatusUnauthorized, status)
		assert.Equal(t, []string{registry.BasicChallenge}, header.Values("WWW-Authenticate"))
	})
}

func TestForwarderFallsBackToTheUpstreamWhenTheMirrorFails(t *testing.T) {
	for _, test := range []struct {
		name     string
		fallback bool
		prepare  func(*testMirror)
		// notOnMirror leaves the image off the mirror.
		notOnMirror bool
		wantStatus  int
	}{
		{name: "mirror down", fallback: true, prepare: func(tm *testMirror) { tm.front.Close() }, wantStatus: http.StatusOK},
		{name: "mirror answers 5xx", fallback: true, prepare: func(tm *testMirror) { tm.mode.Store(mirrorFails) }, wantStatus: http.StatusOK},
		{name: "image not on the mirror", fallback: true, notOnMirror: true, wantStatus: http.StatusOK},
		{name: "token realm off the mirror's host", fallback: true, prepare: func(tm *testMirror) {
			// A host name, not an IP literal: the token library already refuses
			// a loopback IP realm on another port, which would hide the check.
			tm.fake.SetRealm("https://" + strings.Replace(tm.fake.RegistryHost(), "127.0.0.1", "localhost", 1) + "/token")
		}, wantStatus: http.StatusOK},
		{name: "Location outside the path prefix", fallback: true, prepare: func(tm *testMirror) { tm.mode.Store(mirrorRedirectsOut) }, wantStatus: http.StatusOK},
		{name: "mirror down without fallback", prepare: func(tm *testMirror) { tm.front.Close() }, wantStatus: http.StatusBadGateway},
		{name: "mirror answers 5xx without fallback", prepare: func(tm *testMirror) { tm.mode.Store(mirrorFails) }, wantStatus: http.StatusBadGateway},
		{name: "Location outside the path prefix without fallback", prepare: func(tm *testMirror) { tm.mode.Store(mirrorRedirectsOut) }, wantStatus: http.StatusBadGateway},
		{name: "policy mirror forbids", fallback: true, prepare: func(tm *testMirror) { tm.mode.Store(mirrorForbids) }, wantStatus: http.StatusBadGateway},
		{name: "policy mirror rejects its credentials", fallback: true, prepare: func(tm *testMirror) {}, wantStatus: http.StatusBadGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstreamFake := fakeghcr.New(t, fakeghcr.Options{})
			policy := newTestMirror(t, testMirrorOptions{fallback: test.fallback, badCredentials: test.name == "policy mirror rejects its credentials"})

			var onMirror []*testMirror
			if !test.notOnMirror {
				onMirror = append(onMirror, policy)
			}

			pushMirrorImage(t, "owner/app", upstreamFake, onMirror...)
			upstreamFake.ResetLog()
			policy.fake.ResetLog()

			if test.prepare != nil {
				test.prepare(policy)
			}

			harness := newProxyHarness(t, upstreamFake, upstreamFake.PAT(), decideByOwner, withMirrors(policy, nil))

			status, header, body := harness.do(t, http.MethodGet, "/v2/owner/app/manifests/v1")
			assert.Equal(t, test.wantStatus, status, body)
			assert.NotContains(t, header.Get("Location"), policy.front.Listener.Addr().String())

			for _, entry := range policy.fake.Log() {
				if entry.Path == "/token" {
					assert.NotEqual(t, "token realm off the mirror's host", test.name, "no token request goes to a realm off the mirror's host")
				}
			}

			if test.wantStatus == http.StatusOK {
				assert.NotEmpty(t, upstreamPulls(upstreamFake), "the pull fell back to the upstream")
			} else {
				assert.Contains(t, body, "mirror unavailable")
				assert.Empty(t, upstreamPulls(upstreamFake), "no fallback")
			}
		})
	}
}

func TestForwarderMapsMirrorLocationsBackToTheClientHost(t *testing.T) {
	upstreamFake := fakeghcr.New(t, fakeghcr.Options{})
	policy := newTestMirror(t, testMirrorOptions{fallback: true})
	pushMirrorImage(t, "owner/app", upstreamFake, policy)
	policy.mode.Store(mirrorRedirectsInside)

	harness := newProxyHarness(t, upstreamFake, upstreamFake.PAT(), decideByOwner, withMirrors(policy, nil))

	status, header, _ := harness.do(t, http.MethodGet, "/v2/owner/app/manifests/v1")
	assert.Equal(t, http.StatusTemporaryRedirect, status)
	assert.Equal(t, "https://"+harness.host()+"/v2/owner/app/blobs/uploads/elsewhere?state=1", header.Get("Location"))
}

func TestForwarderSendsBlobChecksAndWritesToTheUpstreamOnly(t *testing.T) {
	upstreamFake := fakeghcr.New(t, fakeghcr.Options{})
	policy := newTestMirror(t, testMirrorOptions{fallback: true})
	anonymous := newTestMirror(t, testMirrorOptions{fallback: true, anonymous: true})
	image := pushMirrorImage(t, "owner/app", upstreamFake, policy, anonymous)
	anonImage := pushMirrorImage(t, "anon/app", upstreamFake, policy, anonymous)
	upstreamFake.MakePublic("anon/app")

	harness := newProxyHarness(t, upstreamFake, upstreamFake.PAT(), decideByOwner, withMirrors(policy, anonymous),
		withFakeAPI(upstreamFake))

	for _, request := range []struct{ method, path string }{
		{http.MethodHead, "/v2/owner/app/blobs/" + image.layer.String()},
		{http.MethodHead, "/v2/anon/app/blobs/" + anonImage.layer.String()},
		{http.MethodGet, "/v2/owner/app/tags/list"},
		{http.MethodGet, "/v2/anon/app/tags/list"},
		{http.MethodPost, "/v2/owner/other/blobs/uploads/?mount=" + image.layer.String() + "&from=owner/app"},
		{http.MethodPost, "/v2/owner/app/blobs/uploads/"},
		{http.MethodDelete, "/v2/owner/app/manifests/" + image.manifest.String()},
	} {
		upstreamFake.ResetLog()

		status, _, body := harness.do(t, request.method, request.path)
		assert.Less(t, status, 300, "%s %s: %s", request.method, request.path, body)

		sawUpstream := false

		for _, entry := range upstreamFake.Log() {
			// A delete by digest goes to the upstream's REST API.
			if entry.Method == request.method && (strings.HasPrefix(request.path, entry.Path) || entry.Server == "api") {
				sawUpstream = true
			}
		}

		assert.True(t, sawUpstream, "%s %s goes upstream", request.method, request.path)
	}

	assert.Empty(t, policy.registryRequests())
	assert.Empty(t, anonymous.registryRequests())
}

func TestForwarderNeverSendsThePATToAMirror(t *testing.T) {
	upstreamFake := fakeghcr.New(t, fakeghcr.Options{})
	policy := newTestMirror(t, testMirrorOptions{fallback: true})
	anonymous := newTestMirror(t, testMirrorOptions{fallback: true, anonymous: true})
	// The allowed image is upstream only, so its pulls fall back and use
	// PAT-backed tokens while both mirrors are in use.
	ownerImage := pushMirrorImage(t, "owner/app", upstreamFake)
	anonImage := pushMirrorImage(t, "anon/app", upstreamFake, anonymous)
	upstreamFake.MakePublic("anon/app")

	harness := newProxyHarness(t, upstreamFake, upstreamFake.PAT(), decideByOwner, withMirrors(policy, anonymous))

	for _, path := range []string{
		"/v2/owner/app/manifests/v1", "/v2/owner/app/blobs/" + ownerImage.layer.String(),
		"/v2/anon/app/manifests/v1", "/v2/anon/app/blobs/" + anonImage.layer.String(),
		"/v2/owner/app/manifests/v1",
	} {
		status, _, body := harness.do(t, http.MethodGet, path)
		assert.Less(t, status, 400, "%s: %s", path, body)
	}

	secrets := []string{upstreamFake.PAT()}

	for _, request := range harness.recorder.recorded() {
		if token, found := strings.CutPrefix(request.header.Get("Authorization"), "Bearer "); found {
			secrets = append(secrets, token)
		}
	}

	require.Greater(t, len(secrets), 1, "PAT-backed tokens were used upstream")

	for name, tm := range map[string]*testMirror{"policy": policy, "anonymous": anonymous} {
		require.NotEmpty(t, tm.registryRequests(), "%s mirror was tried", name)

		for _, request := range tm.recorded() {
			if _, password, ok := (&http.Request{Header: request.header}).BasicAuth(); ok {
				assert.NotContains(t, secrets, password, "%s mirror got the PAT as its password", name)
			}

			for header, values := range request.header {
				for _, value := range values {
					for _, secret := range secrets {
						assert.NotContains(t, value, secret, "%s mirror got a PAT or upstream token in %s", name, header)
					}
				}
			}
		}
	}
}
