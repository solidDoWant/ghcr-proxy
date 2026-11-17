package server_test

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/secrets"
	"github.com/solidDoWant/registrygate/test/fakeregistry"
)

// defaultMirror makes mirrorFake the upstream's default mirror, logged in to with the mirror's own PAT.
func defaultMirror(objects *objects, mirrorFake *fakeregistry.Fake) {
	mirrorURL := "https://" + mirrorFake.RegistryHost()

	objects.upstream.Spec.AllowedMirrorURLs = []string{mirrorURL}
	objects.upstream.Spec.DefaultMirrorRef = ref("default-mirror")
	objects.mirrors = append(objects.mirrors, v1alpha1.RegistryMirror{
		ObjectMeta: objectMeta(adminNamespace, "default-mirror"),
		Spec: v1alpha1.RegistryMirrorSpec{
			UpstreamRef: *ref("upstream-a"),
			URL:         mirrorURL,
			PathPrefix:  "proxy-cache",
			SecretRef:   &v1alpha1.CredentialsSecretRef{Name: "default-mirror-robot"},
			CASecretRef: &v1alpha1.LocalRef{Name: "default-mirror-ca"},
		},
	})
	objects.secrets[secrets.Key{Namespace: adminNamespace, Name: "default-mirror-robot"}] = secrets.Secret{
		State: secrets.Present, ResourceVersion: "1",
		Data: secrets.Data{"username": []byte("robot"), "password": []byte(mirrorFake.PAT())},
	}
	objects.secrets[secrets.Key{Namespace: adminNamespace, Name: "default-mirror-ca"}] = secrets.Secret{
		State: secrets.Present, ResourceVersion: "1", Data: secrets.Data{"ca.crt": mirrorFake.CAPEM()},
	}
}

// manifestRequests are the manifest requests a fake logged.
func manifestRequests(fake *fakeregistry.Fake) []fakeregistry.Entry {
	var manifests []fakeregistry.Entry

	for _, entry := range fake.Log() {
		if strings.Contains(entry.Path, "/manifests/") {
			manifests = append(manifests, entry)
		}
	}

	return manifests
}

func TestPullsNoRuleDecidesGoToTheDefaultMirror(t *testing.T) {
	tests := []struct {
		name          string
		authorization []string
	}{
		{name: "anonymous pull"},
		{name: "pull no rule matches", authorization: []string{basic(builderToken)}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mirrorFake := fakeregistry.New(t, fakeregistry.Options{Auth: fakeregistry.Bearer, UploadPath: fakeregistry.Uploads})
			proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
				objects.upstream.Spec.AnonymousPulls = v1alpha1.AnonymousPullsForward
				defaultMirror(objects, mirrorFake)
			}})

			// Only the mirror has the image, so a 200 can only come from the mirror.
			require.NoError(t, mirrorFake.Push("proxy-cache/soliddowant/foo/lib", randomImage(t)))
			mirrorFake.ResetLog()

			got := proxy.do(t, http.MethodGet, "/v2/soliddowant/foo/lib/manifests/latest", test.authorization...)
			assert.Equal(t, http.StatusOK, got.status, got.body)

			mirrored := manifestRequests(mirrorFake)
			require.NotEmpty(t, mirrored, "the pull went to the default mirror")

			for _, entry := range mirrored {
				assert.Equal(t, "/v2/proxy-cache/soliddowant/foo/lib/manifests/latest", entry.Path)
				assert.Equal(t, fakeregistry.CredPATToken, entry.Credential, "the mirror got its own credential")
			}

			assert.Empty(t, manifestRequests(proxy.fake), "the upstream did not serve the pull")
		})
	}
}

func TestPullsNoRuleDecidesGoToTheUpstreamAnonymouslyWithoutADefaultMirror(t *testing.T) {
	tests := []struct {
		name          string
		authorization []string
	}{
		{name: "anonymous pull"},
		{name: "pull no rule matches", authorization: []string{basic(builderToken)}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
				objects.upstream.Spec.AnonymousPulls = v1alpha1.AnonymousPullsForward
			}})

			require.NoError(t, proxy.fake.Push("soliddowant/foo/lib", randomImage(t)))
			proxy.fake.MakePublic("soliddowant/foo/lib")
			proxy.fake.ResetLog()

			got := proxy.do(t, http.MethodGet, "/v2/soliddowant/foo/lib/manifests/latest", test.authorization...)
			assert.Equal(t, http.StatusOK, got.status, got.body)

			manifests := manifestRequests(proxy.fake)
			require.NotEmpty(t, manifests, "the pull went to the upstream")

			for _, entry := range proxy.fake.Log() {
				assert.False(t, usedCredential(entry), "the upstream got no credential: %s %s", entry.Method, entry.Path)
			}
		})
	}
}

// answeringMirror is a mirror that needs no login and answers every repository request with status.
func answeringMirror(t *testing.T, status int) (mirrorURL string, caPEM []byte) {
	t.Helper()

	server := httptest.NewTLSServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v2/" {
			resp.WriteHeader(http.StatusOK)
			return
		}

		resp.WriteHeader(status)
	}))
	t.Cleanup(server.Close)

	return server.URL, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
}

func TestMirror5xxFollowsTheFallbackRule(t *testing.T) {
	tests := []struct {
		name       string
		mirror     int
		fallback   bool
		wantStatus int
	}{
		{name: "500 falls back to the upstream", mirror: http.StatusInternalServerError, fallback: true, wantStatus: http.StatusOK},
		{name: "503 falls back to the upstream", mirror: http.StatusServiceUnavailable, fallback: true, wantStatus: http.StatusOK},
		{name: "500 without fallback is 502", mirror: http.StatusInternalServerError, fallback: false, wantStatus: http.StatusBadGateway},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mirrorURL, caPEM := answeringMirror(t, test.mirror)
			proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
				mirrorAt(objects, mirrorURL, caPEM, "unused", "proxy-cache", test.fallback)
			}})

			require.NoError(t, proxy.fake.Push("soliddowant/foo/lib", randomImage(t)))
			proxy.fake.MakePublic("soliddowant/foo/lib")
			proxy.fake.ResetLog()

			got := proxy.do(t, http.MethodGet, "/v2/soliddowant/foo/lib/manifests/latest", basic(builderToken))
			assert.Equal(t, test.wantStatus, got.status, got.body)

			if test.fallback {
				assert.NotEmpty(t, manifestRequests(proxy.fake), "the pull fell back to the upstream")
			} else {
				assert.Empty(t, manifestRequests(proxy.fake), "the pull did not fall back")
			}
		})
	}
}

func TestMirrorRefusingTheProxyIs502AndNeverFallsBack(t *testing.T) {
	tests := []struct {
		name   string
		mirror func(t *testing.T) (mirrorURL string, caPEM []byte, password string)
	}{
		{
			name: "401 from the mirror",
			mirror: func(t *testing.T) (string, []byte, string) {
				mirrorURL, caPEM := answeringMirror(t, http.StatusUnauthorized)
				return mirrorURL, caPEM, "unused"
			},
		},
		{
			name: "403 from the mirror",
			mirror: func(t *testing.T) (string, []byte, string) {
				mirrorURL, caPEM := answeringMirror(t, http.StatusForbidden)
				return mirrorURL, caPEM, "unused"
			},
		},
		{
			name: "mirror token endpoint rejects the credential",
			mirror: func(t *testing.T) (string, []byte, string) {
				mirrorFake := fakeregistry.New(t, fakeregistry.Options{Auth: fakeregistry.Bearer, UploadPath: fakeregistry.Uploads})
				return "https://" + mirrorFake.RegistryHost(), mirrorFake.CAPEM(), "wrong-password"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mirrorURL, caPEM, password := test.mirror(t)
			proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
				mirrorAt(objects, mirrorURL, caPEM, password, "proxy-cache", true)
			}})

			require.NoError(t, proxy.fake.Push("soliddowant/foo/lib", randomImage(t)))
			proxy.fake.MakePublic("soliddowant/foo/lib")
			proxy.fake.ResetLog()

			got := proxy.do(t, http.MethodGet, "/v2/soliddowant/foo/lib/manifests/latest", basic(builderToken))
			assert.Equal(t, http.StatusBadGateway, got.status, got.body)
			assert.Empty(t, manifestRequests(proxy.fake), "a refused mirror never falls back to the upstream")
		})
	}
}

func TestMirrorCredentialNeverReachesTheUpstream(t *testing.T) {
	// The upstream accepts the same secret as the mirror, so it would recognise the mirror's credential, or a token
	// made from it, if the proxy sent either upstream.
	const sharedSecret = "mirror-and-upstream-secret"

	tests := []struct {
		name      string
		configure func(objects *objects, mirrorFake *fakeregistry.Fake)
	}{
		{
			name: "rule mirror",
			configure: func(objects *objects, mirrorFake *fakeregistry.Fake) {
				mirrorAt(objects, "https://"+mirrorFake.RegistryHost(), mirrorFake.CAPEM(), sharedSecret, "proxy-cache", true)
			},
		},
		{
			name: "default mirror",
			configure: func(objects *objects, mirrorFake *fakeregistry.Fake) {
				defaultMirror(objects, mirrorFake)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mirrorFake := fakeregistry.New(t, fakeregistry.Options{
				Auth: fakeregistry.Bearer, UploadPath: fakeregistry.Uploads, PAT: sharedSecret,
			})
			proxy := newHarness(t, harnessOptions{
				fake:      fakeregistry.Options{PAT: sharedSecret},
				configure: func(objects *objects) { test.configure(objects, mirrorFake) },
			})

			// The mirror doesn't have the image, so the pull falls back to the upstream after the mirror saw the
			// mirror's credential.
			require.NoError(t, proxy.fake.Push("soliddowant/foo/lib", randomImage(t)))
			proxy.fake.MakePublic("soliddowant/foo/lib")
			proxy.fake.ResetLog()

			got := proxy.do(t, http.MethodGet, "/v2/soliddowant/foo/lib/manifests/latest", basic(builderToken))
			assert.Equal(t, http.StatusOK, got.status, got.body)

			mirrored := manifestRequests(mirrorFake)
			require.NotEmpty(t, mirrored, "the pull tried the mirror first")
			assert.Equal(t, fakeregistry.CredPATToken, mirrored[0].Credential, "the mirror got its own credential")

			require.NotEmpty(t, manifestRequests(proxy.fake), "the pull fell back to the upstream")

			for _, entry := range proxy.fake.Log() {
				assert.False(t, usedCredential(entry), "the upstream got a mirror credential: %s %s", entry.Method, entry.Path)
			}
		})
	}
}
