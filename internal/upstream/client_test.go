package upstream_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/model"
	"github.com/solidDoWant/registrygate/internal/upstream"
	"github.com/solidDoWant/registrygate/test/fakeregistry"
)

func repoFor(t *testing.T) string {
	t.Helper()

	sum := sha256.Sum256([]byte(t.Name()))

	return "owner/r" + hex.EncodeToString(sum[:6])
}

func upstreamFor(t *testing.T, rawURL string, configure func(*model.Upstream)) *model.Upstream {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	require.NoError(t, err)

	u := &model.Upstream{
		Object: &v1alpha1.UpstreamRegistry{
			ObjectMeta: metav1.ObjectMeta{Namespace: "registrygate", Name: "up", UID: types.UID("uid-" + t.Name()), Generation: 1},
			Spec:       v1alpha1.UpstreamRegistrySpec{URL: rawURL},
		},
		URL: parsed,
	}
	if configure != nil {
		configure(u)
	}

	return u
}

func fakeUpstream(t *testing.T, fake *fakeregistry.Fake, configure func(*model.Upstream)) *model.Upstream {
	t.Helper()

	return upstreamFor(t, "https://"+fake.RegistryHost(), func(u *model.Upstream) {
		u.CAs = fake.CAPool()
		if configure != nil {
			configure(u)
		}
	})
}

func newClient(t *testing.T, u *model.Upstream) *upstream.Client {
	t.Helper()

	client, err := upstream.NewClient(u, upstream.ClientOptions{})
	require.NoError(t, err)

	return client
}

func get(t *testing.T, client *upstream.Client, path string, cred upstream.Credential, scopes ...string) (*http.Response, error) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, client.URL().String()+path, nil)
	require.NoError(t, err)

	resp, err := client.Do(req, cred, scopes)
	if resp != nil {
		t.Cleanup(func() { _ = resp.Body.Close() })
	}

	return resp, err
}

// throughClient sends requests for the upstream through client.Do and
// everything else (blob storage redirects) with no credential.
type throughClient struct {
	client *upstream.Client
	other  http.RoundTripper
	cred   upstream.Credential
	scopes []string
}

func (c throughClient) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == c.client.URL().Host {
		return c.client.Do(req, c.cred, c.scopes)
	}

	return c.other.RoundTrip(req)
}

func TestClientTrustsConfiguredCAAndFollowsNoRedirects(t *testing.T) {
	fake := fakeregistry.New(t, fakeregistry.Options{Auth: fakeregistry.None})
	repo := repoFor(t)
	image, err := random.Image(64, 1)
	require.NoError(t, err)
	require.NoError(t, fake.Push(repo, image))

	layers, err := image.Layers()
	require.NoError(t, err)
	digest, err := layers[0].Digest()
	require.NoError(t, err)

	t.Run("without the CA the upstream's certificate is refused", func(t *testing.T) {
		client := newClient(t, upstreamFor(t, "https://"+fake.RegistryHost(), nil))

		_, err := get(t, client, "/v2/", upstream.Credential{})
		require.Error(t, err)
	})

	t.Run("with the CA a redirect is returned, not followed", func(t *testing.T) {
		client := newClient(t, fakeUpstream(t, fake, nil))
		fake.ResetLog()

		resp, err := get(t, client, "/v2/"+repo+"/blobs/"+digest.String(), upstream.Credential{})
		require.NoError(t, err)
		assert.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
		assert.Contains(t, resp.Header.Get("Location"), fake.StorageHost())

		for _, entry := range fake.Log() {
			assert.NotEqual(t, "storage", entry.Server, "the redirect must not be followed")
		}
	})
}

func TestClientAuthenticatesPullAndPushPerChallenge(t *testing.T) {
	tests := []struct {
		auth fakeregistry.Auth
		// registry is the credential every registry request must carry.
		registry fakeregistry.Credential
	}{
		{auth: fakeregistry.None, registry: fakeregistry.CredNone},
		{auth: fakeregistry.Basic, registry: fakeregistry.CredPATBasic},
		{auth: fakeregistry.Bearer, registry: fakeregistry.CredPATToken},
	}

	for _, test := range tests {
		t.Run(string(test.auth), func(t *testing.T) {
			fake := fakeregistry.New(t, fakeregistry.Options{Auth: test.auth})
			client := newClient(t, fakeUpstream(t, fake, nil))
			repo := repoFor(t)

			cred := upstream.Credential{ID: "cred", Version: "1", Username: "registrygate", Password: fake.PAT()}
			through := throughClient{
				client: client, other: fake.Transport(), cred: cred,
				scopes: []string{"repository:" + repo + ":pull,push"},
			}
			options := []remote.Option{remote.WithAuth(authn.Anonymous), remote.WithTransport(through)}

			ref, err := name.ParseReference(fake.RegistryHost() + "/" + repo + ":latest")
			require.NoError(t, err)

			image, err := random.Image(128, 2)
			require.NoError(t, err)

			fake.ResetLog()
			require.NoError(t, remote.Write(ref, image, options...))

			pulled, err := remote.Image(ref, options...)
			require.NoError(t, err)

			_, err = pulled.Manifest()
			require.NoError(t, err)

			wantDigest, err := image.Digest()
			require.NoError(t, err)
			gotDigest, err := pulled.Digest()
			require.NoError(t, err)
			assert.Equal(t, wantDigest, gotDigest)

			exchanges := 0

			for _, entry := range fake.Log() {
				// The client's own ping is unauthenticated by design.
				if entry.Server != "registry" || entry.Path == "/v2/" {
					continue
				}

				if entry.Path == "/token" {
					exchanges++

					assert.Equal(t, fakeregistry.CredPATBasic, entry.Credential, "the token exchange uses the credential")

					continue
				}

				assert.Equal(t, test.registry, entry.Credential, "%s %s", entry.Method, entry.Path)
			}

			if test.auth == fakeregistry.Bearer {
				assert.Positive(t, exchanges)
			} else {
				assert.Zero(t, exchanges)
			}
		})
	}
}

func TestClientSendsNoAuthorizationHeaderWithoutAChallenge(t *testing.T) {
	var (
		mu      sync.Mutex
		headers []string
	)

	server := httptest.NewTLSServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		mu.Lock()

		headers = append(headers, req.Header.Get("Authorization"))
		mu.Unlock()
	}))
	t.Cleanup(server.Close)

	client := newClient(t, upstreamFor(t, server.URL, func(u *model.Upstream) {
		u.CAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	}))

	cred := upstream.Credential{ID: "cred", Version: "1", Username: "user", Password: "secret"}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/v2/a/manifests/latest", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer client-token")

	resp, err := client.Do(req, cred, []string{"repository:a:pull"})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, headers, 2, "a ping and the request")
	assert.Equal(t, []string{"", ""}, headers)
}

func TestClientRefusesTokenRealmOutsideAllowedHosts(t *testing.T) {
	tests := []struct {
		name string
		// allowOther lists the other fake's host in tokenRealmHosts.
		allowOther   bool
		errAssertion require.ErrorAssertionFunc
	}{
		{name: "realm on another host is refused", errAssertion: func(t require.TestingT, err error, _ ...any) {
			require.ErrorIs(t, err, upstream.ErrRealmNotAllowed)
		}},
		{name: "realm on a listed token realm host is used", allowOther: true, errAssertion: func(require.TestingT, error, ...any) {}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			other := fakeregistry.New(t, fakeregistry.Options{})
			fake := fakeregistry.New(t, fakeregistry.Options{PAT: other.PAT()})
			_, port, found := strings.Cut(other.RegistryHost(), ":")
			require.True(t, found)

			otherHost := "localhost:" + port
			fake.SetRealm("https://" + otherHost + "/token")

			client := newClient(t, fakeUpstream(t, fake, func(u *model.Upstream) {
				u.CAs = fake.CAPool().Clone()
				u.CAs.AppendCertsFromPEM(other.CAPEM())

				if test.allowOther {
					u.Object.Spec.TokenRealmHosts = []string{otherHost}
				}
			}))

			cred := upstream.Credential{ID: "cred", Version: "1", Username: "registrygate", Password: fake.PAT()}
			_, err := get(t, client, "/v2/"+repoFor(t)+"/manifests/latest", cred, "repository:"+repoFor(t)+":pull")
			test.errAssertion(t, err)

			if test.allowOther {
				assert.NotEmpty(t, other.Log(), "the allowed realm gets the token request")
			} else {
				assert.Empty(t, other.Log(), "nothing, and so no credential, goes to the refused realm")
			}
		})
	}
}

// openPingServer answers /v2/ with 200 but challenges every other path, as
// an upstream whose repositories need a token while its version check is open.
type openPingServer struct {
	mu       sync.Mutex
	requests []string
	server   *httptest.Server
}

func newOpenPingServer(t *testing.T) *openPingServer {
	t.Helper()

	s := &openPingServer{}
	s.server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)

	return s
}

func (s *openPingServer) serve(resp http.ResponseWriter, req *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, req.URL.Path)
	s.mu.Unlock()

	switch {
	case req.URL.Path == "/v2/":
		return
	case req.URL.Path == "/token":
		resp.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(resp, `{"token":"repo-token","expires_in":300}`)
	case req.Header.Get("Authorization") == "Bearer repo-token":
		_, _ = fmt.Fprint(resp, "ok")
	default:
		resp.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="open"`, s.server.URL))
		resp.WriteHeader(http.StatusUnauthorized)
	}
}

func (s *openPingServer) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	requests := s.requests
	s.requests = nil

	return requests
}

func TestClientReplacesCachedChallengeWithPerRequestChallenge(t *testing.T) {
	s := newOpenPingServer(t)
	client := newClient(t, upstreamFor(t, s.server.URL, func(u *model.Upstream) {
		u.CAs = s.server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	}))

	resp, err := get(t, client, "/v2/a/manifests/latest", upstream.Credential{}, "repository:a:pull")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the request is retried with the new challenge")
	assert.Equal(t, []string{"/v2/", "/v2/a/manifests/latest", "/token", "/v2/a/manifests/latest"}, s.take())

	resp, err = get(t, client, "/v2/a/manifests/latest", upstream.Credential{}, "repository:a:pull")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, []string{"/v2/a/manifests/latest"}, s.take(),
		"the new challenge replaced the ping's, so neither a ping nor an unauthenticated try is made")
}

func TestClientRetriesOnlyOnceAfterAChallenge(t *testing.T) {
	var (
		mu    sync.Mutex
		tries int
	)

	server := httptest.NewTLSServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v2/" {
			return
		}

		mu.Lock()
		tries++
		mu.Unlock()
		resp.Header().Set("WWW-Authenticate", `Basic realm="x"`)
		resp.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	client := newClient(t, upstreamFor(t, server.URL, func(u *model.Upstream) {
		u.CAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	}))

	cred := upstream.Credential{ID: "cred", Version: "1", Username: "user", Password: "wrong"}
	resp, err := get(t, client, "/v2/a/manifests/latest", cred, "repository:a:pull")
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 2, tries)
}

func TestClientUsesNewCredentialVersionAndNotItsOldTokens(t *testing.T) {
	fake := fakeregistry.New(t, fakeregistry.Options{})
	repo := repoFor(t)
	image, err := random.Image(64, 1)
	require.NoError(t, err)
	require.NoError(t, fake.Push(repo+":latest", image))

	client := newClient(t, fakeUpstream(t, fake, nil))
	scope := "repository:" + repo + ":pull"
	path := "/v2/" + repo + "/manifests/latest"

	exchanges := func() int {
		count := 0

		for _, entry := range fake.Log() {
			if entry.Path == "/token" {
				count++
			}
		}

		fake.ResetLog()

		return count
	}

	current := upstream.Credential{ID: "cred-uid", Version: "1", Username: "registrygate", Password: fake.PAT()}

	fake.ResetLog()

	resp, err := get(t, client, path, current, scope)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, exchanges())

	resp, err = get(t, client, path, current, scope)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 0, exchanges(), "the same version reuses its token")

	changed := upstream.Credential{ID: "cred-uid", Version: "2", Username: "registrygate", Password: "revoked"}
	_, err = get(t, client, path, changed, scope)
	require.ErrorIs(t, err, upstream.ErrCredentialRejected, "the new values are used, not the old token")
	assert.Equal(t, 1, exchanges())

	_, err = get(t, client, path, current, scope)
	require.NoError(t, err)
	assert.Equal(t, 1, exchanges(), "the old version's token was evicted")
}

func TestClientsRebuildOnGenerationOrCAChange(t *testing.T) {
	clients := upstream.NewClients(upstream.ClientOptions{})
	u := upstreamFor(t, "https://registry.example", func(u *model.Upstream) { u.CAResourceVersion = "10" })

	first, err := clients.Get(u)
	require.NoError(t, err)

	same, err := clients.Get(u)
	require.NoError(t, err)
	assert.Same(t, first, same, "an unchanged upstream keeps its client")

	newGeneration := *u
	newGeneration.Object = u.Object.DeepCopy()
	newGeneration.Object.Generation = 2

	rebuilt, err := clients.Get(&newGeneration)
	require.NoError(t, err)
	assert.NotSame(t, first, rebuilt, "a new generation rebuilds the client")

	newCA := newGeneration
	newCA.CAResourceVersion = "11"

	rebuiltAgain, err := clients.Get(&newCA)
	require.NoError(t, err)
	assert.NotSame(t, rebuilt, rebuiltAgain, "a new CA Secret version rebuilds the client")

	clients.Retain(func(types.UID) bool { return false })

	afterRetain, err := clients.Get(&newCA)
	require.NoError(t, err)
	assert.NotSame(t, rebuiltAgain, afterRetain, "a dropped upstream's client is gone")
}

func TestCredentialHidesItsValues(t *testing.T) {
	cred := upstream.Credential{ID: "cred", Version: "1", Username: "user", Password: "hunter2"}

	for _, formatted := range []string{fmt.Sprint(cred), fmt.Sprintf("%+v", cred), fmt.Sprintf("%#v", cred)} {
		assert.NotContains(t, formatted, "hunter2")
		assert.NotContains(t, formatted, "user")
	}
}

func TestClientReachesPlainHTTPUpstream(t *testing.T) {
	fake := fakeregistry.New(t, fakeregistry.Options{PlainHTTP: true})
	repo := repoFor(t)
	require.NoError(t, fake.Push(repo, mustRandomImage(t)))

	client := newClient(t, upstreamFor(t, "http://"+fake.RegistryHost(), nil))

	cred := upstream.Credential{ID: "cred", Version: "1", Username: "registrygate", Password: fake.PAT()}
	resp, err := get(t, client, "/v2/"+repo+"/manifests/latest", cred, "repository:"+repo+":pull")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestClientSendsTokenRequestForAliasRealmToItsURL(t *testing.T) {
	// The alias names a host that doesn't resolve, so a token request sent to
	// it would fail the pull.
	const alias = "registry.alias.invalid"

	tests := []struct {
		name      string
		plainHTTP bool
		realm     string
	}{
		{name: "https alias realm on an https upstream", realm: "https://" + alias + "/token?extra=1"},
		{name: "https alias realm on a plain HTTP upstream", plainHTTP: true, realm: "https://" + alias + "/token"},
		{name: "http alias realm on an https upstream", realm: "http://" + alias + "/token"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := fakeregistry.New(t, fakeregistry.Options{PlainHTTP: test.plainHTTP})
			repo := repoFor(t)
			require.NoError(t, fake.Push(repo, mustRandomImage(t)))
			fake.SetRealm(test.realm)
			fake.ResetLog()

			scheme := "https"
			if test.plainHTTP {
				scheme = "http"
			}

			client := newClient(t, upstreamFor(t, scheme+"://"+fake.RegistryHost(), func(u *model.Upstream) {
				u.CAs = fake.CAPool()
				u.Object.Spec.AliasHosts = []string{alias}
			}))

			cred := upstream.Credential{ID: "cred", Version: "1", Username: "registrygate", Password: fake.PAT()}
			resp, err := get(t, client, "/v2/"+repo+"/manifests/latest", cred, "repository:"+repo+":pull")
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, resp.StatusCode)

			var tokenRequests int

			for _, entry := range fake.Log() {
				if entry.Path == "/token" {
					tokenRequests++

					if strings.Contains(test.realm, "?extra=") {
						assert.Contains(t, entry.QueryKeys, "extra", "the realm's query is kept")
					}
				}
			}

			assert.Positive(t, tokenRequests, "the token request reaches the upstream's url")
		})
	}
}

func TestClientRefusesRealmOnAnAliasItDoesNotList(t *testing.T) {
	fake := fakeregistry.New(t, fakeregistry.Options{})
	fake.SetRealm("https://registry.alias.invalid/token")

	client := newClient(t, fakeUpstream(t, fake, func(u *model.Upstream) {
		u.Object.Spec.AliasHosts = []string{"other.alias.invalid"}
	}))

	cred := upstream.Credential{ID: "cred", Version: "1", Username: "registrygate", Password: fake.PAT()}
	_, err := get(t, client, "/v2/"+repoFor(t)+"/manifests/latest", cred, "repository:"+repoFor(t)+":pull")
	require.ErrorIs(t, err, upstream.ErrRealmNotAllowed)
}

func mustRandomImage(t *testing.T) v1.Image {
	t.Helper()

	image, err := random.Image(64, 1)
	require.NoError(t, err)

	return image
}
