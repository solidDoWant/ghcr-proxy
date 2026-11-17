package mirror_test

import (
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
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
)

type fakeMirror struct {
	server *httptest.Server
	pool   *x509.CertPool
	open   bool

	mu      sync.Mutex
	paths   []string
	headers []http.Header
}

func newFakeMirror(t *testing.T) *fakeMirror {
	t.Helper()

	fake := &fakeMirror{}
	fake.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.paths = append(fake.paths, r.URL.Path)
		fake.headers = append(fake.headers, r.Header.Clone())
		fake.mu.Unlock()

		switch {
		case fake.open:
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v2/":
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+fake.server.URL+`/token",service="mirror"`)
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
	fake.pool.AddCert(fake.server.Certificate())

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
