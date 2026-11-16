package fakeghcr_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/ghcr-proxy/test/fakeghcr"
)

func ref(t *testing.T, fake *fakeghcr.Fake, repo string) name.Reference {
	t.Helper()

	parsed, err := name.ParseReference(fake.RegistryHost() + "/" + repo + ":latest")
	require.NoError(t, err)

	return parsed
}

func TestPATClientPushesAndPullsThroughBearerChallenge(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})
	image, err := random.Image(1024, 2)
	require.NoError(t, err)
	require.NoError(t, fake.Push("owner/app", image))

	pulled, err := remote.Image(ref(t, fake, "owner/app"), fake.RemoteOptions(true)...)
	require.NoError(t, err)
	want, err := image.Digest()
	require.NoError(t, err)
	got, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, want, got)

	layers, err := pulled.Layers()
	require.NoError(t, err)

	for _, layer := range layers {
		reader, err := layer.Uncompressed()
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, reader)
		require.NoError(t, err)
	}
}

func TestAnonymousPullNeedsPublicRepository(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})
	image, err := random.Image(256, 1)
	require.NoError(t, err)
	require.NoError(t, fake.Push("owner/app", image))

	_, err = remote.Image(ref(t, fake, "owner/app"), fake.RemoteOptions(false)...)

	var transportErr *transport.Error
	require.ErrorAs(t, err, &transportErr)
	assert.Equal(t, http.StatusUnauthorized, transportErr.StatusCode)

	fake.MakePublic("owner/app")
	_, err = remote.Image(ref(t, fake, "owner/app"), fake.RemoteOptions(false)...)
	require.NoError(t, err)

	// Public does not mean writable.
	err = remote.Write(ref(t, fake, "owner/app"), image, fake.RemoteOptions(false)...)
	require.Error(t, err)
}

func getToken(t *testing.T, fake *fakeghcr.Fake, scope string) string {
	t.Helper()

	query := url.Values{"service": {"ghcr.io"}, "scope": {scope}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+fake.RegistryHost()+"/token?"+query.Encode(), nil)
	require.NoError(t, err)
	req.SetBasicAuth("fake", fake.PAT())
	client := &http.Client{Transport: fake.Transport()}
	resp, err := client.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	// {"token":"...","expires_in":N}
	_, rest, _ := strings.Cut(string(body), `"token":"`)
	token, _, _ := strings.Cut(rest, `"`)
	require.NotEmpty(t, token)

	return token
}

func TestTokenIsLimitedToItsScope(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})
	image, err := random.Image(256, 1)
	require.NoError(t, err)
	require.NoError(t, fake.Push("owner/app", image))
	require.NoError(t, fake.Push("owner/other", image))
	token := getToken(t, fake, "repository:owner/app:pull")

	tests := map[string]struct {
		method string
		path   string
		status int
	}{
		"pull in scope":      {http.MethodGet, "/v2/owner/app/manifests/latest", http.StatusOK},
		"pull other repo":    {http.MethodGet, "/v2/owner/other/manifests/latest", http.StatusUnauthorized},
		"push in scope repo": {http.MethodPost, "/v2/owner/app/blobs/uploads/", http.StatusUnauthorized},
		"delete":             {http.MethodDelete, "/v2/owner/app/manifests/latest", http.StatusUnauthorized},
	}
	for testName, tt := range tests {
		t.Run(testName, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), tt.method, "https://"+fake.RegistryHost()+tt.path, nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := (&http.Client{Transport: fake.Transport()}).Do(req)
			require.NoError(t, err)

			defer func() { _ = resp.Body.Close() }()

			assert.Equal(t, tt.status, resp.StatusCode)

			if tt.status == http.StatusUnauthorized {
				assert.Contains(t, resp.Header.Get("WWW-Authenticate"), "Bearer realm=")
			}
		})
	}
}

func TestUploadLocationIsAbsoluteAndBlobReadRedirectsToStorage(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})
	token := getToken(t, fake, "repository:owner/app:pull,push")
	client := &http.Client{
		Transport:     fake.Transport(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+fake.RegistryHost()+"/v2/owner/app/blobs/uploads/", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	require.NoError(t, err)

	_ = resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	assert.True(t, strings.HasPrefix(resp.Header.Get("Location"), "https://"+fake.RegistryHost()+"/v2/owner/app/blobs/upload/"), resp.Header.Get("Location"))

	image, err := random.Image(512, 1)
	require.NoError(t, err)
	require.NoError(t, fake.Push("owner/app", image))
	layers, err := image.Layers()
	require.NoError(t, err)
	digest, err := layers[0].Digest()
	require.NoError(t, err)

	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+fake.RegistryHost()+"/v2/owner/app/blobs/"+digest.String(), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	require.NoError(t, err)

	_ = resp.Body.Close()
	require.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
	location := resp.Header.Get("Location")
	require.True(t, strings.HasPrefix(location, "https://"+fake.StorageHost()+"/"+digest.String()+"?sig="), location)

	// The storage server needs no credential and serves the blob.
	stored, err := client.Get(location)
	require.NoError(t, err)

	defer func() { _ = stored.Body.Close() }()

	require.Equal(t, http.StatusOK, stored.StatusCode)

	compressed, err := layers[0].Compressed()
	require.NoError(t, err)
	want, err := io.ReadAll(compressed)
	require.NoError(t, err)
	got, err := io.ReadAll(stored.Body)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	// An unsigned storage URL is refused.
	bare, err := client.Get("https://" + fake.StorageHost() + "/" + digest.String())
	require.NoError(t, err)

	_ = bare.Body.Close()
	assert.Equal(t, http.StatusNotFound, bare.StatusCode)
}

func TestRequestLogRecordsCredentialKind(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})
	fake.MakePublic("owner/pub")

	image, err := random.Image(128, 1)
	require.NoError(t, err)
	require.NoError(t, fake.Push("owner/pub", image))
	fake.ResetLog()

	_, err = remote.Image(ref(t, fake, "owner/pub"), fake.RemoteOptions(false)...)
	require.NoError(t, err)
	_, err = remote.Image(ref(t, fake, "owner/pub"), fake.RemoteOptions(true)...)
	require.NoError(t, err)

	kinds := map[fakeghcr.Credential]bool{}

	var sawManifestGet bool

	for _, entry := range fake.Log() {
		kinds[entry.Credential] = true
		if entry.Method == http.MethodGet && entry.Path == "/v2/owner/pub/manifests/latest" && entry.Status == http.StatusOK {
			sawManifestGet = true
		}

		if entry.Path == "/token" {
			assert.Contains(t, entry.QueryKeys, "scope")
		}
	}

	assert.True(t, sawManifestGet)
	assert.True(t, kinds[fakeghcr.CredNone], "unauthenticated challenge request")
	assert.True(t, kinds[fakeghcr.CredAnonymousToken])
	assert.True(t, kinds[fakeghcr.CredPATToken])
	assert.True(t, kinds[fakeghcr.CredPATBasic])
}

func TestExpiredTokenIsRefused(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{TokenTTL: 1})
	token := getToken(t, fake, "repository:owner/app:pull")
	require.Eventually(t, func() bool {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+fake.RegistryHost()+"/v2/", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := (&http.Client{Transport: fake.Transport()}).Do(req)
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		return resp.StatusCode == http.StatusUnauthorized
	}, 5e9, 5e6)
}

func TestPushUploadsThroughGHCRSingularSessionPath(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})

	image, err := random.Image(512, 1)
	require.NoError(t, err)
	require.NoError(t, fake.Push("owner/app", image))

	var sessionRequests int

	for _, entry := range fake.Log() {
		if entry.Server != "registry" || entry.Method == http.MethodPost {
			continue
		}

		assert.NotContains(t, entry.Path, "/blobs/uploads/", "%s %s", entry.Method, entry.Path)

		if strings.HasPrefix(entry.Path, "/v2/owner/app/blobs/upload/") {
			sessionRequests++

			assert.Less(t, entry.Status, 300, "%s %s", entry.Method, entry.Path)
		}
	}

	assert.Positive(t, sessionRequests, "no request used a singular upload session path")

	pushed, err := remote.Image(ref(t, fake, "owner/app"), fake.RemoteOptions(true)...)
	require.NoError(t, err)

	wantDigest, err := image.Digest()
	require.NoError(t, err)
	gotDigest, err := pushed.Digest()
	require.NoError(t, err)
	assert.Equal(t, wantDigest, gotDigest)
}
