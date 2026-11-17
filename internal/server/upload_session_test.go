package server_test

import (
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/secrets"
	"github.com/solidDoWant/registrygate/internal/uploadtoken"
	"github.com/solidDoWant/registrygate/test/fakeregistry"
)

var sessionLocation = regexp.MustCompile(`^/v2/soliddowant/foo/app/blobs/uploads/[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{43}$`)

// startUpload starts an upload for the builder and returns the session location the client got.
func (h *harness) startUpload(t *testing.T, repository string) string {
	t.Helper()

	got := h.do(t, http.MethodPost, "/v2/"+repository+"/blobs/uploads/", basic(builderToken))
	require.Equal(t, http.StatusAccepted, got.status, got.body)

	return got.header.Get("Location")
}

func TestUploadLocationsAreSignedSessionTokens(t *testing.T) {
	for _, path := range []fakeregistry.UploadPath{fakeregistry.Upload, fakeregistry.Uploads} {
		t.Run(string(path), func(t *testing.T) {
			proxy := newHarness(t, harnessOptions{fake: fakeregistry.Options{UploadPath: path}})

			location := proxy.startUpload(t, "soliddowant/foo/app")
			assert.Regexp(t, sessionLocation, location)

			got := proxy.do(t, http.MethodPatch, location, basic(builderToken))
			assert.Equal(t, http.StatusAccepted, got.status, "the token opens the upstream's session: %s", got.body)
			assert.Regexp(t, sessionLocation, got.header.Get("Location"), "a session answer gets a token again")
		})
	}
}

func TestForgedUploadSessionTokensAre404AndSendNothing(t *testing.T) {
	proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
		objects.policy.Spec.Rules = append(objects.policy.Spec.Rules, v1alpha1.AccessRule{
			Name: "second", CredentialRef: ref("shared"),
			Repositories: []string{"soliddowant/foo/second"}, Actions: []v1alpha1.Action{v1alpha1.ActionPush},
		})
	}})

	location := proxy.startUpload(t, "soliddowant/foo/app")
	token := strings.TrimPrefix(location, "/v2/soliddowant/foo/app/blobs/uploads/")
	payload, mac, _ := strings.Cut(token, ".")

	otherKey, err := uploadtoken.New([]byte("another key of thirty-two bytes!"))
	require.NoError(t, err)

	forged, err := otherKey.Issue("upstream-a-uid", "soliddowant/foo/app",
		mustURL(t, "https://"+proxy.fake.RegistryHost()+"/v2/soliddowant/foo/app/blobs/"),
		"/v2/soliddowant/foo/app/blobs/x/../../manifests/latest")
	if err == nil {
		t.Fatalf("the codec issued a token for a path outside the blobs: %s", forged)
	}

	escaping := base64.RawURLEncoding.EncodeToString([]byte("../manifests/latest"))

	tests := map[string]string{
		"signature changed":            "/v2/soliddowant/foo/app/blobs/uploads/" + payload + "." + flipFirst(mac),
		"payload changed":              "/v2/soliddowant/foo/app/blobs/uploads/" + flipFirst(payload) + "." + mac,
		"another repository's session": "/v2/soliddowant/foo/second/blobs/uploads/" + token,
		"payload escaping the blobs":   "/v2/soliddowant/foo/app/blobs/uploads/" + escaping + "." + mac,
	}

	for name, target := range tests {
		t.Run(name, func(t *testing.T) {
			proxy.fake.ResetLog()

			got := proxy.do(t, http.MethodPatch, target, basic(builderToken))

			assert.Equal(t, http.StatusNotFound, got.status, got.body)
			assert.Equal(t, "BLOB_UPLOAD_UNKNOWN", got.code)
			assert.Empty(t, proxy.fake.Log(), "nothing is sent upstream")
		})
	}
}

func flipFirst(value string) string {
	if value[0] == 'A' {
		return "B" + value[1:]
	}

	return "A" + value[1:]
}

func TestUploadSessionRequestsAcceptOnlyADigestOnPut(t *testing.T) {
	proxy := newHarness(t, harnessOptions{})
	location := proxy.startUpload(t, "soliddowant/foo/app")

	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

	tests := []struct {
		name   string
		method string
		query  string
	}{
		{name: "another key on PUT", method: http.MethodPut, query: "digest=" + digest + "&state=x"},
		{name: "a digest on PATCH", method: http.MethodPatch, query: "digest=" + digest},
		{name: "any key on GET", method: http.MethodGet, query: "n=1"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy.fake.ResetLog()

			got := proxy.do(t, test.method, location+"?"+test.query, basic(builderToken))

			assert.Equal(t, http.StatusBadRequest, got.status, got.body)
			assert.Empty(t, proxy.fake.Log(), "nothing is sent upstream")
		})
	}
}

// fixedLocationUpstream is an upstream with no authentication that answers every upload start with 202 and the
// given Location.
func fixedLocationUpstream(t *testing.T, location string) *httptest.Server {
	t.Helper()

	upstreamServer := httptest.NewTLSServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v2/" {
			resp.WriteHeader(http.StatusOK)
			return
		}

		resp.Header().Set("Location", strings.ReplaceAll(location, "HOST", req.Host))
		resp.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(upstreamServer.Close)

	return upstreamServer
}

func TestUnusableUploadLocationsAre502(t *testing.T) {
	tests := map[string]string{
		"another host":                   "https://elsewhere.test/v2/soliddowant/foo/app/blobs/uploads/session",
		"another repository":             "https://HOST/v2/soliddowant/foo/other/blobs/uploads/session",
		"outside the repository's blobs": "/v2/soliddowant/foo/app/manifests/latest",
		"plain http":                     "http://HOST/v2/soliddowant/foo/app/blobs/uploads/session",
	}

	for name, location := range tests {
		t.Run(name, func(t *testing.T) {
			upstreamServer := fixedLocationUpstream(t, location)
			caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstreamServer.Certificate().Raw})

			proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
				objects.upstream.Spec.URL = upstreamServer.URL
				objects.secrets[secrets.Key{Namespace: adminNamespace, Name: "upstream-a-ca"}] = secrets.Secret{
					State: secrets.Present, ResourceVersion: "1", Data: secrets.Data{"ca.crt": caPEM},
				}
			}})

			got := proxy.do(t, http.MethodPost, "/v2/soliddowant/foo/app/blobs/uploads/", basic(builderToken))

			assert.Equal(t, http.StatusBadGateway, got.status, got.body)
			assert.Equal(t, "upstream returned an unusable upload location", got.body)
			assert.Empty(t, got.header.Get("Location"))
		})
	}
}
