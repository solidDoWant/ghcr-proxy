package mirror_test

import (
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/mirror"
	"github.com/solidDoWant/registrygate/internal/model"
	"github.com/solidDoWant/registrygate/test/fakeregistry"
)

type fakeMirror struct {
	server *httptest.Server
	pool   *x509.CertPool
	open   bool
	// realm replaces the challenge's realm, which is otherwise the fake's own /token.
	realm string

	mu      sync.Mutex
	paths   []string
	headers []http.Header
}

func newFakeMirror(t *testing.T) *fakeMirror {
	t.Helper()

	return startFakeMirror(t, httptest.NewTLSServer)
}

func newPlainHTTPFakeMirror(t *testing.T) *fakeMirror {
	t.Helper()

	return startFakeMirror(t, httptest.NewServer)
}

func startFakeMirror(t *testing.T, start func(http.Handler) *httptest.Server) *fakeMirror {
	t.Helper()

	fake := &fakeMirror{}
	fake.server = start(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.paths = append(fake.paths, r.URL.Path)
		fake.headers = append(fake.headers, r.Header.Clone())
		fake.mu.Unlock()

		switch {
		case fake.open:
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v2/":
			realm := fake.server.URL + "/token"
			if fake.realm != "" {
				realm = fake.realm
			}

			w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`",service="mirror"`)
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/token":
			user, password, _ := r.BasicAuth()
			if user != "mirror-user" || password != "mirror-pass" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}

			_, _ = w.Write([]byte(`{"token":"mirror-token","expires_in":300}`))
		case r.Header.Get("Authorization") == "Bearer mirror-token":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(fake.server.Close)

	fake.pool = x509.NewCertPool()
	if certificate := fake.server.Certificate(); certificate != nil {
		fake.pool.AddCert(certificate)
	}

	return fake
}

func (f *fakeMirror) model(t *testing.T) *model.Mirror {
	t.Helper()

	parsed, err := url.Parse(f.server.URL + "/proxy")
	require.NoError(t, err)

	return &model.Mirror{
		Object: &v1alpha1.RegistryMirror{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns", UID: "uid-1", Generation: 1},
			Spec: v1alpha1.RegistryMirrorSpec{
				PathPrefix: "ghcr",
				Timeouts:   v1alpha1.Timeouts{Token: &metav1.Duration{Duration: 5 * time.Second}},
			},
		},
		Upstream:              &model.Upstream{Object: &v1alpha1.UpstreamRegistry{Spec: v1alpha1.UpstreamRegistrySpec{AllowedMirrorURLs: []string{"https://a"}}}},
		URL:                   parsed,
		Credentials:           &model.Credentials{Username: "mirror-user", Password: "mirror-pass"},
		SecretResourceVersion: "1",
		CAs:                   f.pool,
		FallbackToUpstream:    true,
	}
}

func TestClientSendsToPrefixedPathWithMirrorCredential(t *testing.T) {
	fake := newFakeMirror(t)
	client, err := mirror.NewClientFromMirror(fake.model(t), mirror.ClientOptions{})
	require.NoError(t, err)

	target := client.RequestURL("/v2/team/app/manifests/latest", "x=1")
	assert.Equal(t, "/proxy/v2/ghcr/team/app/manifests/latest", target.Path)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target.String(), nil)
	require.NoError(t, err)

	resp, err := client.Send(req, "repository:team/app:pull")
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, fake.paths, "/proxy/v2/ghcr/team/app/manifests/latest")
	assert.True(t, client.FallbackToUpstream())
}

func TestClientRejectedCredentialIsAnError(t *testing.T) {
	fake := newFakeMirror(t)
	object := fake.model(t)
	object.Credentials = &model.Credentials{Username: "mirror-user", Password: "wrong"}

	client, err := mirror.NewClientFromMirror(object, mirror.ClientOptions{})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, client.RequestURL("/v2/a/manifests/b", "").String(), nil)
	require.NoError(t, err)

	_, err = client.Send(req, "repository:a:pull")
	require.ErrorIs(t, err, mirror.ErrCredentialRejected)
	assert.NotContains(t, err.Error(), "wrong")
}

func TestClientUnreachableMirrorIsUnavailable(t *testing.T) {
	fake := newFakeMirror(t)
	object := fake.model(t)
	fake.server.Close()

	client, err := mirror.NewClientFromMirror(object, mirror.ClientOptions{})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, client.RequestURL("/v2/a/manifests/b", "").String(), nil)
	require.NoError(t, err)

	_, err = client.Send(req, "repository:a:pull")
	require.ErrorIs(t, err, mirror.ErrUnavailable)
}

func TestCacheRebuildsOnlyOnRelevantChange(t *testing.T) {
	fake := newFakeMirror(t)

	tests := map[string]struct {
		change  func(*model.Mirror)
		rebuilt bool
	}{
		"nothing changed":       {change: func(*model.Mirror) {}},
		"generation":            {change: func(m *model.Mirror) { m.Object.Generation = 2 }, rebuilt: true},
		"allowedMirrorURLs":     {change: func(m *model.Mirror) { m.Upstream.Object.Spec.AllowedMirrorURLs = []string{"https://a", "https://b"} }, rebuilt: true},
		"credentials Secret":    {change: func(m *model.Mirror) { m.SecretResourceVersion = "2" }, rebuilt: true},
		"CA Secret":             {change: func(m *model.Mirror) { m.CAResourceVersion = "9" }, rebuilt: true},
		"status only (message)": {change: func(m *model.Mirror) { m.Status.Message = "x" }},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			cache := mirror.NewCache(mirror.ClientOptions{})
			first, err := cache.Client(fake.model(t))
			require.NoError(t, err)

			changed := fake.model(t)
			test.change(changed)

			second, err := cache.Client(changed)
			require.NoError(t, err)
			assert.Equal(t, test.rebuilt, first != second)
		})
	}
}

func TestCacheKeysByUID(t *testing.T) {
	fake := newFakeMirror(t)
	cache := mirror.NewCache(mirror.ClientOptions{})

	first, err := cache.Client(fake.model(t))
	require.NoError(t, err)

	other := fake.model(t)
	other.Object.UID = "uid-2"

	second, err := cache.Client(other)
	require.NoError(t, err)
	assert.NotSame(t, first, second)

	cache.Retain(map[types.UID]struct{}{"uid-2": {}})

	third, err := cache.Client(fake.model(t))
	require.NoError(t, err)
	assert.NotSame(t, first, third)
}

func TestClientSendsNoUpstreamCredentialToAnOpenMirror(t *testing.T) {
	fake := newFakeMirror(t)
	fake.open = true
	client, err := mirror.NewClientFromMirror(fake.model(t), mirror.ClientOptions{})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, client.RequestURL("/v2/a/manifests/b", "").String(), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer upstream-token")

	resp, err := client.Send(req, "repository:a:pull")
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	fake.mu.Lock()
	defer fake.mu.Unlock()

	for _, header := range fake.headers {
		assert.NotContains(t, header.Get("Authorization"), "upstream-token")
	}
}

func TestClientTokenRealm(t *testing.T) {
	// The alias names a host that doesn't resolve, so a token request sent to
	// it would fail the pull.
	const alias = "harbor.alias.invalid"

	// tokenServer is a separate token endpoint, as a mirror whose token
	// service runs on another host has. It is named localhost because
	// go-containerregistry refuses a private IP as another host's realm, and
	// served over plain HTTP because the test certificate doesn't cover
	// localhost.
	tokenServer := newPlainHTTPFakeMirror(t)
	tokenHost := "localhost:" + tokenServer.server.URL[strings.LastIndex(tokenServer.server.URL, ":")+1:]

	// tlsTokenServer serves TLS for localhost, so only the realm check stops a
	// token request to it from a plain HTTP mirror.
	tlsTokenServer := fakeregistry.New(t, fakeregistry.Options{DNSNames: []string{"localhost"}})
	tlsTokenHost := "localhost:" + tlsTokenServer.RegistryHost()[strings.LastIndex(tlsTokenServer.RegistryHost(), ":")+1:]

	tests := []struct {
		name         string
		plainHTTP    bool
		realm        func(fake *fakeMirror) string
		configure    func(object *model.Mirror, fake *fakeMirror)
		errAssertion require.ErrorAssertionFunc
		// tokenAt is where the token request must go; the mirror itself when nil.
		tokenAt *fakeMirror
	}{
		{
			name:  "realm on an alias host goes to the mirror's url",
			realm: func(*fakeMirror) string { return "https://" + alias + "/token?service=x" },
			configure: func(object *model.Mirror, _ *fakeMirror) {
				object.Object.Spec.AliasHosts = []string{alias}
			},
		},
		{
			name:      "https realm on an alias host of a plain HTTP mirror goes to the mirror's url",
			plainHTTP: true,
			realm:     func(*fakeMirror) string { return "https://" + alias + "/token" },
			configure: func(object *model.Mirror, _ *fakeMirror) {
				object.Object.Spec.AliasHosts = []string{alias}
			},
		},
		{
			name:         "realm on an unlisted host is refused",
			realm:        func(*fakeMirror) string { return "https://" + alias + "/token" },
			errAssertion: errorIs(mirror.ErrUnavailable),
		},
		{
			name:      "realm on a token realm host is used",
			plainHTTP: true,
			realm:     func(*fakeMirror) string { return "http://" + tokenHost + "/token" },
			configure: func(object *model.Mirror, _ *fakeMirror) { object.Object.Spec.TokenRealmHosts = []string{tokenHost} },
			tokenAt:   tokenServer,
		},
		{
			name:      "realm on a token realm host with another scheme is refused",
			plainHTTP: true,
			realm:     func(*fakeMirror) string { return "https://" + tlsTokenHost + "/token" },
			configure: func(object *model.Mirror, _ *fakeMirror) {
				object.Object.Spec.TokenRealmHosts = []string{tlsTokenHost}
				object.CAs = tlsTokenServer.CAPool()
			},
			errAssertion: func(t require.TestingT, err error, _ ...any) {
				require.ErrorIs(t, err, mirror.ErrUnavailable)
				assert.Empty(t, tlsTokenServer.Log(), "nothing goes to a realm on another scheme")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeMirror(t)
			if test.plainHTTP {
				fake = newPlainHTTPFakeMirror(t)
			}

			fake.realm = test.realm(fake)

			object := fake.model(t)
			if test.configure != nil {
				test.configure(object, fake)
			}

			client, err := mirror.NewClientFromMirror(object, mirror.ClientOptions{})
			require.NoError(t, err)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, client.RequestURL("/v2/a/manifests/b", "").String(), nil)
			require.NoError(t, err)

			resp, err := client.Send(req, "repository:a:pull")
			if test.errAssertion != nil {
				test.errAssertion(t, err)
				fake.mu.Lock()
				defer fake.mu.Unlock()

				assert.NotContains(t, fake.paths, "/token", "no credential goes to a refused realm")

				return
			}

			require.NoError(t, err)

			defer func() { _ = resp.Body.Close() }()

			assert.Equal(t, http.StatusOK, resp.StatusCode)

			tokenAt := fake
			if test.tokenAt != nil {
				tokenAt = test.tokenAt
			}

			tokenAt.mu.Lock()
			defer tokenAt.mu.Unlock()

			assert.Contains(t, tokenAt.paths, "/token")
		})
	}
}

func errorIs(target error) require.ErrorAssertionFunc {
	return func(t require.TestingT, err error, _ ...any) {
		require.ErrorIs(t, err, target)
	}
}

func TestClientNonPositiveTimeoutsUseTheDefaults(t *testing.T) {
	for name, value := range map[string]time.Duration{"zero": 0, "negative": -time.Second} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeMirror(t)
			object := fake.model(t)
			object.Object.Spec.Timeouts = v1alpha1.Timeouts{
				Token:          &metav1.Duration{Duration: value},
				ResponseHeader: &metav1.Duration{Duration: value},
			}

			client, err := mirror.NewClientFromMirror(object, mirror.ClientOptions{})
			require.NoError(t, err)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, client.RequestURL("/v2/team/app/manifests/latest", "").String(), nil)
			require.NoError(t, err)

			resp, err := client.Send(req, "repository:team/app:pull")
			require.NoError(t, err)

			defer func() { _ = resp.Body.Close() }()

			assert.Equal(t, http.StatusOK, resp.StatusCode)
		})
	}
}
