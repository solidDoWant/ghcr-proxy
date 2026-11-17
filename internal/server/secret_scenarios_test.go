package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/internal/secrets"
	"github.com/solidDoWant/registrygate/test/fakeregistry"
)

func TestPushWithAnUnusableCredentialSecretIs403AndSendsNothingUpstream(t *testing.T) {
	credentialKey := secrets.Key{Namespace: adminNamespace, Name: "shared-credential"}

	tests := []struct {
		name   string
		secret secrets.Secret
	}{
		{name: "missing Secret", secret: secrets.Secret{State: secrets.NotFound}},
		{name: "Secret without the password key", secret: secrets.Secret{
			State: secrets.Present, ResourceVersion: "2", Data: secrets.Data{"username": []byte("registrygate")},
		}},
		{name: "Secret with an empty password", secret: secrets.Secret{
			State: secrets.Present, ResourceVersion: "2",
			Data: secrets.Data{"username": []byte("registrygate"), "password": {}},
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy := newHarness(t, harnessOptions{})

			// A push first works, so a token made from the old credential is cached.
			require.NotEqual(t, http.StatusForbidden,
				proxy.do(t, http.MethodPost, "/v2/soliddowant/foo/app/blobs/uploads/", basic(builderToken)).status)

			proxy.objects.secrets[credentialKey] = test.secret
			proxy.rebuild(t)
			proxy.fake.ResetLog()

			got := proxy.do(t, http.MethodPost, "/v2/soliddowant/foo/app/blobs/uploads/", basic(builderToken))

			assert.Equal(t, http.StatusForbidden, got.status, got.body)
			assert.Empty(t, proxy.fake.Log(), "nothing is sent with a stale or empty credential")
		})
	}
}

func TestMirrorSecretChangeAppliesToTheNextMirrorPull(t *testing.T) {
	mirrorFake := fakeregistry.New(t, fakeregistry.Options{Auth: fakeregistry.Bearer, UploadPath: fakeregistry.Uploads})
	proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
		mirrorOf(t, objects, mirrorFake, "proxy-cache", true)
	}})
	require.NoError(t, mirrorFake.Push("proxy-cache/soliddowant/foo/lib", randomImage(t)))

	const target = "/v2/soliddowant/foo/lib/manifests/latest"

	setPassword := func(password, version string) {
		proxy.objects.secrets[secrets.Key{Namespace: ciNamespace, Name: "mirror-robot"}] = secrets.Secret{
			State: secrets.Present, ResourceVersion: version,
			Data: secrets.Data{"username": []byte("robot"), "password": []byte(password)},
		}
		proxy.rebuild(t)
	}

	require.Equal(t, http.StatusOK, proxy.do(t, http.MethodGet, target, basic(builderToken)).status)

	setPassword("revoked", "2")

	got := proxy.do(t, http.MethodGet, target, basic(builderToken))
	assert.Equal(t, http.StatusBadGateway, got.status, "the token cached for the old Secret is not used")
	assert.NotContains(t, got.body, "revoked")

	setPassword(mirrorFake.PAT(), "3")

	assert.Equal(t, http.StatusOK, proxy.do(t, http.MethodGet, target, basic(builderToken)).status,
		"the replaced mirror Secret is used without a restart")
}

func TestMirrorWithAnUnreadableSecretIsSkippedAndPullsFallBack(t *testing.T) {
	mirrorFake := fakeregistry.New(t, fakeregistry.Options{Auth: fakeregistry.Bearer, UploadPath: fakeregistry.Uploads})
	proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
		mirrorOf(t, objects, mirrorFake, "proxy-cache", true)
		objects.secrets[secrets.Key{Namespace: ciNamespace, Name: "mirror-robot"}] = secrets.Secret{State: secrets.Forbidden}
	}})

	require.NoError(t, proxy.fake.Push("soliddowant/foo/lib", randomImage(t)))
	proxy.fake.MakePublic("soliddowant/foo/lib")
	proxy.fake.ResetLog()

	got := proxy.do(t, http.MethodGet, "/v2/soliddowant/foo/lib/manifests/latest", basic(builderToken))

	assert.Equal(t, http.StatusOK, got.status, got.body)
	assert.Empty(t, mirrorFake.Log(), "a mirror whose Secret can't be read is never contacted")

	for _, entry := range proxy.fake.Log() {
		assert.False(t, usedCredential(entry), "the fallback is anonymous: %s %s", entry.Method, entry.Path)
	}
}

func TestMirrorOutsideTheAllowedURLsIsNeverContacted(t *testing.T) {
	mirrorFake := fakeregistry.New(t, fakeregistry.Options{Auth: fakeregistry.Bearer, UploadPath: fakeregistry.Uploads})
	proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
		mirrorOf(t, objects, mirrorFake, "proxy-cache", true)
		objects.upstream.Spec.AllowedMirrorURLs = []string{"https://harbor.example/base"}
	}})

	image := randomImage(t)
	require.NoError(t, mirrorFake.Push("proxy-cache/soliddowant/foo/lib", image))
	require.NoError(t, proxy.fake.Push("soliddowant/foo/lib", image))
	proxy.fake.MakePublic("soliddowant/foo/lib")
	mirrorFake.ResetLog()

	got := proxy.do(t, http.MethodGet, "/v2/soliddowant/foo/lib/manifests/latest", basic(builderToken))

	assert.Equal(t, http.StatusOK, got.status, "pulls fall back as for a failed mirror: %s", got.body)
	assert.Empty(t, mirrorFake.Log(), "a mirror outside the allowed URLs is never contacted")
}
