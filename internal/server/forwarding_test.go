package server_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/secrets"
	"github.com/solidDoWant/registrygate/test/fakeregistry"
)

// builderOptions are go-containerregistry options for a CI pod's client: the builder's token as the Basic
// password, through the proxy.
func (h *harness) builderOptions() []remote.Option {
	return []remote.Option{
		remote.WithAuth(&authn.Basic{Username: "registrygate", Password: builderToken}),
		remote.WithTransport(h.transport()),
	}
}

func (h *harness) ref(t *testing.T, repository string) name.Reference {
	t.Helper()

	ref, err := name.ParseReference(registryHost+"/"+repository, name.WithDefaultTag("latest"))
	require.NoError(t, err)

	return ref
}

func randomImage(t *testing.T) v1.Image {
	t.Helper()

	image, err := random.Image(512, 2)
	require.NoError(t, err)

	return image
}

// usedCredential reports whether a request the fake logged carried the credential's own secret.
func usedCredential(entry fakeregistry.Entry) bool {
	return entry.Credential == fakeregistry.CredPATBasic || entry.Credential == fakeregistry.CredPATToken
}

func TestPushAndPullWorkWithEveryUpstreamChallenge(t *testing.T) {
	tests := []struct {
		name string
		fake fakeregistry.Options
	}{
		{name: "no authentication", fake: fakeregistry.Options{Auth: fakeregistry.None, UploadPath: fakeregistry.Uploads}},
		{name: "Basic", fake: fakeregistry.Options{Auth: fakeregistry.Basic, UploadPath: fakeregistry.Uploads}},
		{name: "Bearer with singular upload sessions", fake: fakeregistry.Options{Auth: fakeregistry.Bearer, UploadPath: fakeregistry.Upload}},
		{name: "Bearer with plural upload sessions", fake: fakeregistry.Options{Auth: fakeregistry.Bearer, UploadPath: fakeregistry.Uploads}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy := newHarness(t, harnessOptions{fake: test.fake})
			image := randomImage(t)

			require.NoError(t, remote.Write(proxy.ref(t, "soliddowant/foo/app"), image, proxy.builderOptions()...))

			pulled, err := remote.Image(proxy.ref(t, "soliddowant/foo/app"), proxy.builderOptions()...)
			require.NoError(t, err)

			want, err := image.Digest()
			require.NoError(t, err)

			got, err := pulled.Digest()
			require.NoError(t, err)
			assert.Equal(t, want, got)

			if test.fake.Auth == fakeregistry.None {
				return
			}

			var pushes int

			for _, entry := range proxy.fake.Log() {
				if entry.Server == "registry" && (entry.Method == http.MethodPut || entry.Method == http.MethodPatch) {
					pushes++

					assert.True(t, usedCredential(entry), "%s %s went upstream with %s", entry.Method, entry.EscapedPath, entry.Credential)
				}
			}

			assert.NotZero(t, pushes, "the push reached the upstream")
		})
	}
}

func TestTokenRealmOutsideTheUpstreamIs502AndGetsNoCredential(t *testing.T) {
	proxy := newHarness(t, harnessOptions{})
	proxy.fake.SetRealm("https://tokens.elsewhere.test/token")

	got := proxy.do(t, http.MethodGet, "/v2/soliddowant/foo/source/manifests/latest", basic(builderToken))

	assert.Equal(t, http.StatusBadGateway, got.status)
	assert.Equal(t, "upstream token endpoint not allowed", got.body)

	for _, entry := range proxy.fake.Log() {
		assert.False(t, usedCredential(entry), "%s %s", entry.Method, entry.EscapedPath)
	}
}

func TestTokenRealmOnAnAllowedHostIsUsed(t *testing.T) {
	proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
		objects.upstream.Spec.TokenRealmHosts = []string{"tokens.elsewhere.test"}
	}})
	image := randomImage(t)
	require.NoError(t, proxy.fake.Push("soliddowant/foo/source", image))

	// The realm host is allowed, so the proxy tries it; it doesn't resolve, which is an unavailable upstream rather
	// than a refused realm.
	proxy.fake.SetRealm("https://tokens.elsewhere.test/token")

	got := proxy.do(t, http.MethodGet, "/v2/soliddowant/foo/source/manifests/latest", basic(builderToken))

	assert.Equal(t, http.StatusBadGateway, got.status)
	assert.Equal(t, "upstream unavailable", got.body)
}

func TestDeleteForwardsWithTheCredentialAndReturnsTheUpstreamAnswer(t *testing.T) {
	tests := []struct {
		name       string
		delete     fakeregistry.Delete
		target     func(manifest, blob string) string
		wantStatus int
		wantCode   string
	}{
		{
			name: "manifest by tag", delete: fakeregistry.Supported,
			target:     func(string, string) string { return "/v2/soliddowant/foo/disposable/manifests/latest" },
			wantStatus: http.StatusAccepted,
		},
		{
			name: "manifest by digest", delete: fakeregistry.Supported,
			target:     func(manifest, _ string) string { return "/v2/soliddowant/foo/disposable/manifests/" + manifest },
			wantStatus: http.StatusAccepted,
		},
		{
			name: "blob", delete: fakeregistry.Supported,
			target:     func(_, blob string) string { return "/v2/soliddowant/foo/disposable/blobs/" + blob },
			wantStatus: http.StatusAccepted,
		},
		{
			name: "unsupported by the upstream", delete: fakeregistry.Unsupported,
			target:     func(manifest, _ string) string { return "/v2/soliddowant/foo/disposable/manifests/" + manifest },
			wantStatus: http.StatusMethodNotAllowed, wantCode: "UNSUPPORTED",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy := newHarness(t, harnessOptions{fake: fakeregistry.Options{Delete: test.delete, UploadPath: fakeregistry.Uploads}})
			image := randomImage(t)
			require.NoError(t, proxy.fake.Push("soliddowant/foo/disposable", image))

			manifest, err := image.Digest()
			require.NoError(t, err)

			layers, err := image.Layers()
			require.NoError(t, err)

			blob, err := layers[0].Digest()
			require.NoError(t, err)

			proxy.fake.ResetLog()

			got := proxy.do(t, http.MethodDelete, test.target(manifest.String(), blob.String()), basic(builderToken))

			assert.Equal(t, test.wantStatus, got.status, got.body)

			if test.wantCode != "" {
				assert.Equal(t, test.wantCode, got.code)
			}

			deletes := slices.DeleteFunc(proxy.fake.Log(), func(entry fakeregistry.Entry) bool { return entry.Method != http.MethodDelete })
			require.Len(t, deletes, 1, "the delete was forwarded once")
			assert.True(t, usedCredential(deletes[0]), "the delete carried the credential")
		})
	}
}

func TestCredentialSecretChangeAppliesToTheNextRequest(t *testing.T) {
	proxy := newHarness(t, harnessOptions{})
	require.NoError(t, proxy.fake.Push("soliddowant/foo/source", randomImage(t)))

	credentialKey := secrets.Key{Namespace: adminNamespace, Name: "shared-credential"}
	setPassword := func(password, version string) {
		proxy.objects.secrets[credentialKey] = secrets.Secret{
			State: secrets.Present, ResourceVersion: version,
			Data: secrets.Data{"username": []byte("registrygate"), "password": []byte(password)},
		}
		proxy.rebuild(t)
	}

	const target = "/v2/soliddowant/foo/source/manifests/latest"

	require.Equal(t, http.StatusOK, proxy.do(t, http.MethodGet, target, basic(builderToken)).status,
		"the first pull caches a token made from the credential")

	setPassword("revoked", "2")

	got := proxy.do(t, http.MethodGet, target, basic(builderToken))
	assert.Equal(t, http.StatusBadGateway, got.status, "the token cached for the old Secret is not used")
	assert.Equal(t, "upstream rejected the proxy's credential", got.body)
	assert.NotContains(t, got.body, "revoked")

	setPassword(proxy.fake.PAT(), "3")

	assert.Equal(t, http.StatusOK, proxy.do(t, http.MethodGet, target, basic(builderToken)).status,
		"the replaced Secret is used without a restart")
}

// mirrorOf sets up a pull-through mirror for the upstream: its own fake registry with its own secret, serving the
// upstream's content under pathPrefix.
func mirrorOf(t *testing.T, objects *objects, mirrorFake *fakeregistry.Fake, pathPrefix string, fallback bool) {
	t.Helper()

	objects.upstream.Spec.AllowedMirrorURLs = []string{"https://" + mirrorFake.RegistryHost()}
	objects.mirrors = append(objects.mirrors, v1alpha1.RegistryMirror{
		ObjectMeta: objectMeta(ciNamespace, "mirror"),
		Spec: v1alpha1.RegistryMirrorSpec{
			UpstreamRef:        *ref("upstream-a"),
			URL:                "https://" + mirrorFake.RegistryHost(),
			PathPrefix:         pathPrefix,
			SecretRef:          &v1alpha1.CredentialsSecretRef{Name: "mirror-robot"},
			CASecretRef:        &v1alpha1.LocalRef{Name: "mirror-ca"},
			FallbackToUpstream: &fallback,
		},
	})
	objects.secrets[secrets.Key{Namespace: ciNamespace, Name: "mirror-robot"}] = secrets.Secret{
		State: secrets.Present, ResourceVersion: "1",
		Data: secrets.Data{"username": []byte("robot"), "password": []byte(mirrorFake.PAT())},
	}
	objects.secrets[secrets.Key{Namespace: ciNamespace, Name: "mirror-ca"}] = secrets.Secret{
		State: secrets.Present, ResourceVersion: "1", Data: secrets.Data{"ca.crt": mirrorFake.CAPEM()},
	}
	objects.policy.Spec.Rules = append(objects.policy.Spec.Rules, v1alpha1.AccessRule{
		Name: "mirrored", MirrorRef: &v1alpha1.ObjectRef{Name: "mirror"},
		Repositories: []string{"soliddowant/foo/**"}, Actions: []v1alpha1.Action{v1alpha1.ActionPull},
	})
}

func TestMirrorRuleServesPullsWithTheMirrorsOwnCredential(t *testing.T) {
	mirrorFake := fakeregistry.New(t, fakeregistry.Options{Auth: fakeregistry.Bearer, UploadPath: fakeregistry.Uploads})
	proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
		mirrorOf(t, objects, mirrorFake, "proxy-cache", true)
	}})

	image := randomImage(t)
	require.NoError(t, mirrorFake.Push("proxy-cache/soliddowant/foo/lib", image))

	// The manifest comes through the proxy; blob downloads redirect to the mirror's storage, which the client's
	// SSRF guard refuses on a loopback address, so they are checked through the proxy directly.
	pulled, err := remote.Image(proxy.ref(t, "soliddowant/foo/lib"), proxy.builderOptions()...)
	require.NoError(t, err)

	manifest, err := pulled.Manifest()
	require.NoError(t, err)

	blob := proxy.do(t, http.MethodGet, "/v2/soliddowant/foo/lib/blobs/"+manifest.Config.Digest.String(), basic(builderToken))
	assert.Equal(t, http.StatusTemporaryRedirect, blob.status, blob.body)
	assert.Contains(t, blob.header.Get("Location"), mirrorFake.StorageHost(), "the blob download went to the mirror")

	var mirrorPulls int

	for _, entry := range mirrorFake.Log() {
		if entry.Server != "registry" || entry.Path == "/v2/" || entry.Path == "/token" {
			continue
		}

		mirrorPulls++

		assert.True(t, strings.HasPrefix(entry.Path, "/v2/proxy-cache/soliddowant/foo/lib/"), entry.Path)
		assert.Equal(t, fakeregistry.CredPATToken, entry.Credential,
			"%s %s carried only the mirror's own credential", entry.Method, entry.Path)
	}

	assert.NotZero(t, mirrorPulls, "the pull went to the mirror")

	for _, entry := range proxy.fake.Log() {
		assert.False(t, usedCredential(entry), "the upstream got no credential for a mirror rule's pull: %s %s", entry.Method, entry.Path)
		assert.NotContains(t, entry.Path, "/manifests/", "manifests came from the mirror")
	}
}

func TestMirrorFailuresFollowTheFallbackRule(t *testing.T) {
	tests := []struct {
		name       string
		fallback   bool
		wantStatus int
	}{
		{name: "falls back to the upstream", fallback: true, wantStatus: http.StatusOK},
		{name: "no fallback is 502", fallback: false, wantStatus: http.StatusBadGateway},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mirrorFake := fakeregistry.New(t, fakeregistry.Options{Auth: fakeregistry.Bearer, UploadPath: fakeregistry.Uploads})
			proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
				mirrorOf(t, objects, mirrorFake, "proxy-cache", test.fallback)
			}})

			// The image is public upstream: a mirror rule's pull goes upstream anonymously.
			require.NoError(t, proxy.fake.Push("soliddowant/foo/lib", randomImage(t)))
			proxy.fake.MakePublic("soliddowant/foo/lib")
			require.NoError(t, mirrorFake.Close())
			proxy.fake.ResetLog()

			got := proxy.do(t, http.MethodGet, "/v2/soliddowant/foo/lib/manifests/latest", basic(builderToken))
			assert.Equal(t, test.wantStatus, got.status, got.body)

			for _, entry := range proxy.fake.Log() {
				assert.False(t, usedCredential(entry), "a fallback never uses the upstream credential: %s %s", entry.Method, entry.Path)
			}
		})
	}
}

func TestCredentialedPullSkipsTheMirror(t *testing.T) {
	mirrorFake := fakeregistry.New(t, fakeregistry.Options{Auth: fakeregistry.Bearer, UploadPath: fakeregistry.Uploads})
	proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
		mirrorOf(t, objects, mirrorFake, "proxy-cache", true)
	}})

	image := randomImage(t)
	require.NoError(t, proxy.fake.Push("soliddowant/foo/source", image))
	require.NoError(t, mirrorFake.Push("proxy-cache/soliddowant/foo/source", image))
	mirrorFake.ResetLog()

	pulled, err := remote.Image(proxy.ref(t, "soliddowant/foo/source"), proxy.builderOptions()...)
	require.NoError(t, err)

	_, err = pulled.Manifest()
	require.NoError(t, err)

	assert.Empty(t, mirrorFake.Log(), "no mirror sees a pull a credential rule decides")

	var manifests int

	for _, entry := range proxy.fake.Log() {
		if strings.Contains(entry.Path, "/manifests/") {
			manifests++

			assert.True(t, usedCredential(entry), "the pull went upstream with the credential")
		}
	}

	assert.NotZero(t, manifests)
}

func TestHostHeaderMustNameTheServerNamesRegistry(t *testing.T) {
	proxy := newHarness(t, harnessOptions{})
	require.NoError(t, proxy.fake.Push("soliddowant/foo/source", randomImage(t)))

	tests := map[string]int{
		registryHost:              http.StatusOK,
		registryHost + ":443":     http.StatusOK,
		"other.test":              http.StatusBadRequest,
		registryHost + ":8443":    http.StatusBadRequest,
		proxy.fake.RegistryHost(): http.StatusBadRequest,
	}

	for host, want := range tests {
		t.Run(host, func(t *testing.T) {
			proxy.fake.ResetLog()

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, proxy.url()+"/v2/soliddowant/foo/source/manifests/latest", nil)
			require.NoError(t, err)

			req.Host = host
			req.Header.Set("Authorization", basic(builderToken))

			resp, err := proxy.client().Do(req)
			require.NoError(t, err)

			_ = resp.Body.Close()

			assert.Equal(t, want, resp.StatusCode)

			if want == http.StatusBadRequest {
				assert.Empty(t, proxy.fake.Log(), "nothing is sent upstream")
			}
		})
	}
}
