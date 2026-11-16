package registry_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/registry"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func newRequest(t *testing.T, method, target string) *http.Request {
	t.Helper()
	return httptest.NewRequestWithContext(t.Context(), method, "https://ghcr.io"+target, nil)
}

func requireRegistryError(status int, code string) require.ErrorAssertionFunc {
	return func(t require.TestingT, err error, _ ...any) {
		var registryErr *registry.Error
		require.True(t, errors.As(err, &registryErr), "error %v is not a *registry.Error", err)
		assert.Equal(t, status, registryErr.Status)
		assert.Equal(t, code, registryErr.Code)
	}
}

var (
	requireNotFound      = requireRegistryError(http.StatusNotFound, "NOT_FOUND")
	requireDigestInvalid = requireRegistryError(http.StatusBadRequest, "DIGEST_INVALID")
)

type classifyCase struct {
	name        string
	method      string
	target      string
	wantRoute   registry.Route
	wantAction  v1alpha1.Action
	wantName    string
	wantRef     string
	wantDigest  digest.Digest
	wantSession string
	wantMount   *registry.Mount
	wantURL     string
	assertErr   require.ErrorAssertionFunc
}

func runClassifyCases(t *testing.T, cases []classifyCase) {
	t.Helper()

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			assertErr := testCase.assertErr
			if assertErr == nil {
				assertErr = require.NoError
			}

			request, err := registry.Classify(newRequest(t, testCase.method, testCase.target))
			assertErr(t, err)

			if err != nil {
				return
			}

			assert.Equal(t, testCase.wantRoute, request.Route)
			assert.Equal(t, testCase.wantAction, request.Action)
			assert.Equal(t, testCase.wantName, request.Name)
			assert.Equal(t, testCase.wantRef, request.Reference)
			assert.Equal(t, testCase.wantDigest, request.Digest)
			assert.Equal(t, testCase.wantSession, request.Session)
			assert.Equal(t, testCase.wantMount, request.Mount)
			assert.Equal(t, testCase.wantURL, request.UpstreamURL().String())
		})
	}
}

func TestClassifyRouteTable(t *testing.T) {
	runClassifyCases(t, []classifyCase{
		{name: "GET version check", method: http.MethodGet, target: "/v2/", wantRoute: registry.RouteBase, wantURL: "/v2/"},
		{name: "HEAD version check", method: http.MethodHead, target: "/v2/", wantRoute: registry.RouteBase, wantURL: "/v2/"},
		{
			name: "GET manifest by tag", method: http.MethodGet, target: "/v2/owner/image/manifests/v1.0",
			wantRoute: registry.RouteManifest, wantAction: v1alpha1.ActionPull, wantName: "owner/image", wantRef: "v1.0",
			wantURL: "/v2/owner/image/manifests/v1.0",
		},
		{
			name: "HEAD manifest by digest", method: http.MethodHead, target: "/v2/owner/image/manifests/" + digestA,
			wantRoute: registry.RouteManifest, wantAction: v1alpha1.ActionPull, wantName: "owner/image", wantRef: digestA,
			wantURL: "/v2/owner/image/manifests/" + digestA,
		},
		{
			name: "GET blob", method: http.MethodGet, target: "/v2/owner/image/blobs/" + digestA,
			wantRoute: registry.RouteBlob, wantAction: v1alpha1.ActionPull, wantName: "owner/image", wantDigest: digestA,
			wantURL: "/v2/owner/image/blobs/" + digestA,
		},
		{
			name: "HEAD blob", method: http.MethodHead, target: "/v2/owner/image/blobs/" + digestA,
			wantRoute: registry.RouteBlob, wantAction: v1alpha1.ActionPull, wantName: "owner/image", wantDigest: digestA,
			wantURL: "/v2/owner/image/blobs/" + digestA,
		},
		{
			name: "GET tag list", method: http.MethodGet, target: "/v2/owner/image/tags/list",
			wantRoute: registry.RouteTagsList, wantAction: v1alpha1.ActionPull, wantName: "owner/image",
			wantURL: "/v2/owner/image/tags/list",
		},
		{
			name: "HEAD tag list", method: http.MethodHead, target: "/v2/owner/image/tags/list",
			wantRoute: registry.RouteTagsList, wantAction: v1alpha1.ActionPull, wantName: "owner/image",
			wantURL: "/v2/owner/image/tags/list",
		},
		{
			name: "GET referrers", method: http.MethodGet, target: "/v2/owner/image/referrers/" + digestA,
			wantRoute: registry.RouteReferrers, wantAction: v1alpha1.ActionPull, wantName: "owner/image", wantDigest: digestA,
			wantURL: "/v2/owner/image/referrers/" + digestA,
		},
		{
			name: "HEAD referrers", method: http.MethodHead, target: "/v2/owner/image/referrers/" + digestA,
			wantRoute: registry.RouteReferrers, wantAction: v1alpha1.ActionPull, wantName: "owner/image", wantDigest: digestA,
			wantURL: "/v2/owner/image/referrers/" + digestA,
		},
		{
			name: "POST upload start", method: http.MethodPost, target: "/v2/owner/image/blobs/uploads/",
			wantRoute: registry.RouteUploadStart, wantAction: v1alpha1.ActionPush, wantName: "owner/image",
			wantURL: "/v2/owner/image/blobs/uploads/",
		},
		{
			name: "POST upload start with mount", method: http.MethodPost,
			target:    "/v2/owner/image/blobs/uploads/?mount=" + digestA + "&from=owner/other",
			wantRoute: registry.RouteUploadStart, wantAction: v1alpha1.ActionPush, wantName: "owner/image",
			wantMount: &registry.Mount{Digest: digestA, From: "owner/other"},
			wantURL:   "/v2/owner/image/blobs/uploads/?from=owner%2Fother&mount=" + strings.ReplaceAll(digestA, ":", "%3A"),
		},
		{
			name: "GET upload session", method: http.MethodGet, target: "/v2/owner/image/blobs/uploads/session-1",
			wantRoute: registry.RouteUploadSession, wantAction: v1alpha1.ActionPush, wantName: "owner/image", wantSession: "session-1",
			wantURL: "/v2/owner/image/blobs/uploads/session-1",
		},
		{
			name: "PATCH upload session", method: http.MethodPatch, target: "/v2/owner/image/blobs/uploads/session-1",
			wantRoute: registry.RouteUploadSession, wantAction: v1alpha1.ActionPush, wantName: "owner/image", wantSession: "session-1",
			wantURL: "/v2/owner/image/blobs/uploads/session-1",
		},
		{
			name: "PUT upload session", method: http.MethodPut, target: "/v2/owner/image/blobs/uploads/session-1?digest=" + digestA,
			wantRoute: registry.RouteUploadSession, wantAction: v1alpha1.ActionPush, wantName: "owner/image", wantSession: "session-1",
			wantURL: "/v2/owner/image/blobs/uploads/session-1?digest=" + digestA,
		},
		{
			name: "DELETE upload session cancels the upload and is push", method: http.MethodDelete,
			target:    "/v2/owner/image/blobs/uploads/session-1",
			wantRoute: registry.RouteUploadSession, wantAction: v1alpha1.ActionPush, wantName: "owner/image", wantSession: "session-1",
			wantURL: "/v2/owner/image/blobs/uploads/session-1",
		},
		{
			name: "GET GHCR singular upload session", method: http.MethodGet,
			target:    "/v2/owner/image/blobs/upload/19.3c730f6c-c0f8-4e04-9c9c-8864a15a4a1b",
			wantRoute: registry.RouteUploadSession, wantAction: v1alpha1.ActionPush, wantName: "owner/image", wantSession: "19.3c730f6c-c0f8-4e04-9c9c-8864a15a4a1b",
			wantURL: "/v2/owner/image/blobs/upload/19.3c730f6c-c0f8-4e04-9c9c-8864a15a4a1b",
		},
		{
			name: "PATCH GHCR singular upload session", method: http.MethodPatch,
			target:    "/v2/owner/image/blobs/upload/19.3c730f6c-c0f8-4e04-9c9c-8864a15a4a1b",
			wantRoute: registry.RouteUploadSession, wantAction: v1alpha1.ActionPush, wantName: "owner/image", wantSession: "19.3c730f6c-c0f8-4e04-9c9c-8864a15a4a1b",
			wantURL: "/v2/owner/image/blobs/upload/19.3c730f6c-c0f8-4e04-9c9c-8864a15a4a1b",
		},
		{
			name: "PUT GHCR singular upload session", method: http.MethodPut,
			target:    "/v2/owner/image/blobs/upload/19.3c730f6c-c0f8-4e04-9c9c-8864a15a4a1b?digest=" + digestA,
			wantRoute: registry.RouteUploadSession, wantAction: v1alpha1.ActionPush, wantName: "owner/image", wantSession: "19.3c730f6c-c0f8-4e04-9c9c-8864a15a4a1b",
			wantURL: "/v2/owner/image/blobs/upload/19.3c730f6c-c0f8-4e04-9c9c-8864a15a4a1b?digest=" + digestA,
		},
		{
			name: "DELETE GHCR singular upload session", method: http.MethodDelete,
			target:    "/v2/owner/image/blobs/upload/19.3c730f6c-c0f8-4e04-9c9c-8864a15a4a1b",
			wantRoute: registry.RouteUploadSession, wantAction: v1alpha1.ActionPush, wantName: "owner/image", wantSession: "19.3c730f6c-c0f8-4e04-9c9c-8864a15a4a1b",
			wantURL: "/v2/owner/image/blobs/upload/19.3c730f6c-c0f8-4e04-9c9c-8864a15a4a1b",
		},
		{
			name: "PUT manifest", method: http.MethodPut, target: "/v2/owner/image/manifests/latest",
			wantRoute: registry.RouteManifest, wantAction: v1alpha1.ActionPush, wantName: "owner/image", wantRef: "latest",
			wantURL: "/v2/owner/image/manifests/latest",
		},
		{
			name: "DELETE manifest", method: http.MethodDelete, target: "/v2/owner/image/manifests/" + digestA,
			wantRoute: registry.RouteManifest, wantAction: v1alpha1.ActionDelete, wantName: "owner/image", wantRef: digestA,
			wantURL: "/v2/owner/image/manifests/" + digestA,
		},
		{
			name: "DELETE blob", method: http.MethodDelete, target: "/v2/owner/image/blobs/" + digestA,
			wantRoute: registry.RouteBlob, wantAction: v1alpha1.ActionDelete, wantName: "owner/image", wantDigest: digestA,
			wantURL: "/v2/owner/image/blobs/" + digestA,
		},
		{name: "single-component name", method: http.MethodGet, target: "/v2/image/manifests/latest",
			wantRoute: registry.RouteManifest, wantAction: v1alpha1.ActionPull, wantName: "image", wantRef: "latest",
			wantURL: "/v2/image/manifests/latest"},
		{name: "deep name", method: http.MethodGet, target: "/v2/owner/a/b/c/manifests/latest",
			wantRoute: registry.RouteManifest, wantAction: v1alpha1.ActionPull, wantName: "owner/a/b/c", wantRef: "latest",
			wantURL: "/v2/owner/a/b/c/manifests/latest"},
	})
}

func TestClassifyRejectsMethodsOutsideTheRouteTable(t *testing.T) {
	cases := []struct{ method, target string }{
		{http.MethodPost, "/v2/"},
		{http.MethodDelete, "/v2/"},
		{http.MethodPost, "/v2/owner/image/manifests/latest"},
		{http.MethodPatch, "/v2/owner/image/manifests/latest"},
		{http.MethodPut, "/v2/owner/image/blobs/" + digestA},
		{http.MethodPost, "/v2/owner/image/blobs/" + digestA},
		{http.MethodPut, "/v2/owner/image/tags/list"},
		{http.MethodDelete, "/v2/owner/image/tags/list"},
		{http.MethodDelete, "/v2/owner/image/referrers/" + digestA},
		{http.MethodGet, "/v2/owner/image/blobs/uploads/"},
		{http.MethodPut, "/v2/owner/image/blobs/uploads/"},
		{http.MethodPost, "/v2/owner/image/blobs/uploads/session-1"},
		{http.MethodHead, "/v2/owner/image/blobs/uploads/session-1"},
		{http.MethodPost, "/v2/owner/image/blobs/upload/session-1"},
		{http.MethodHead, "/v2/owner/image/blobs/upload/session-1"},
		{http.MethodOptions, "/v2/owner/image/manifests/latest"},
	}
	for _, testCase := range cases {
		t.Run(testCase.method+" "+testCase.target, func(t *testing.T) {
			_, err := registry.Classify(newRequest(t, testCase.method, testCase.target))
			requireNotFound(t, err)
		})
	}
}

func TestClassifyNamesContainingRouteKeywords(t *testing.T) {
	runClassifyCases(t, []classifyCase{
		{
			name: "manifest of a name ending in manifests", method: http.MethodGet,
			target:    "/v2/owner/manifests/manifests/latest",
			wantRoute: registry.RouteManifest, wantAction: v1alpha1.ActionPull, wantName: "owner/manifests", wantRef: "latest",
			wantURL: "/v2/owner/manifests/manifests/latest",
		},
		{
			name: "manifest whose tag is manifests", method: http.MethodGet,
			target:    "/v2/owner/image/manifests/manifests",
			wantRoute: registry.RouteManifest, wantAction: v1alpha1.ActionPull, wantName: "owner/image", wantRef: "manifests",
			wantURL: "/v2/owner/image/manifests/manifests",
		},
		{
			name: "blob of a name containing blobs and uploads", method: http.MethodGet,
			target:    "/v2/owner/blobs/uploads/blobs/" + digestA,
			wantRoute: registry.RouteBlob, wantAction: v1alpha1.ActionPull, wantName: "owner/blobs/uploads", wantDigest: digestA,
			wantURL: "/v2/owner/blobs/uploads/blobs/" + digestA,
		},
		{
			name: "tag list of a name containing tags and list", method: http.MethodGet,
			target:    "/v2/tags/list/tags/list",
			wantRoute: registry.RouteTagsList, wantAction: v1alpha1.ActionPull, wantName: "tags/list",
			wantURL: "/v2/tags/list/tags/list",
		},
		{
			name: "upload start of a name containing blobs and uploads", method: http.MethodPost,
			target:    "/v2/blobs/uploads/blobs/uploads/",
			wantRoute: registry.RouteUploadStart, wantAction: v1alpha1.ActionPush, wantName: "blobs/uploads",
			wantURL: "/v2/blobs/uploads/blobs/uploads/",
		},
		{
			name: "upload session named uploads", method: http.MethodPatch,
			target:    "/v2/owner/manifests/blobs/uploads/uploads",
			wantRoute: registry.RouteUploadSession, wantAction: v1alpha1.ActionPush, wantName: "owner/manifests", wantSession: "uploads",
			wantURL: "/v2/owner/manifests/blobs/uploads/uploads",
		},
		{
			name: "singular upload session of a name containing blobs and upload", method: http.MethodPatch,
			target:    "/v2/owner/blobs/upload/blobs/upload/session-1",
			wantRoute: registry.RouteUploadSession, wantAction: v1alpha1.ActionPush, wantName: "owner/blobs/upload", wantSession: "session-1",
			wantURL: "/v2/owner/blobs/upload/blobs/upload/session-1",
		},
		{
			name: "plural upload session of a name ending in upload", method: http.MethodPatch,
			target:    "/v2/owner/upload/blobs/uploads/session-1",
			wantRoute: registry.RouteUploadSession, wantAction: v1alpha1.ActionPush, wantName: "owner/upload", wantSession: "session-1",
			wantURL: "/v2/owner/upload/blobs/uploads/session-1",
		},
		{
			name: "singular upload session of a name ending in uploads", method: http.MethodPatch,
			target:    "/v2/owner/uploads/blobs/upload/session-1",
			wantRoute: registry.RouteUploadSession, wantAction: v1alpha1.ActionPush, wantName: "owner/uploads", wantSession: "session-1",
			wantURL: "/v2/owner/uploads/blobs/upload/session-1",
		},
		{
			name: "singular upload session named upload", method: http.MethodPut,
			target:    "/v2/owner/image/blobs/upload/upload?digest=" + digestA,
			wantRoute: registry.RouteUploadSession, wantAction: v1alpha1.ActionPush, wantName: "owner/image", wantSession: "upload",
			wantURL: "/v2/owner/image/blobs/upload/upload?digest=" + digestA,
		},
		{
			name: "upload start of a name ending in upload", method: http.MethodPost,
			target:    "/v2/owner/upload/blobs/uploads/",
			wantRoute: registry.RouteUploadStart, wantAction: v1alpha1.ActionPush, wantName: "owner/upload",
			wantURL: "/v2/owner/upload/blobs/uploads/",
		},
		{
			name: "blob of a name ending in blobs and upload", method: http.MethodGet,
			target:    "/v2/owner/blobs/upload/blobs/" + digestA,
			wantRoute: registry.RouteBlob, wantAction: v1alpha1.ActionPull, wantName: "owner/blobs/upload", wantDigest: digestA,
			wantURL: "/v2/owner/blobs/upload/blobs/" + digestA,
		},
		{
			name: "upload is not a blob digest", method: http.MethodGet,
			target: "/v2/owner/image/blobs/upload", assertErr: requireNotFound,
		},
		{
			name: "singular upload is not an upload start", method: http.MethodPost,
			target: "/v2/owner/image/blobs/upload/", assertErr: requireNotFound,
		},
		{
			name: "singular upload session with no name", method: http.MethodPatch,
			target: "/v2/blobs/upload/session-1", assertErr: requireNotFound,
		},
		{
			name: "manifest push of a name containing referrers", method: http.MethodPut,
			target:    "/v2/referrers/tags/manifests/v1",
			wantRoute: registry.RouteManifest, wantAction: v1alpha1.ActionPush, wantName: "referrers/tags", wantRef: "v1",
			wantURL: "/v2/referrers/tags/manifests/v1",
		},
		{
			name: "uploads is not a blob digest", method: http.MethodGet,
			target: "/v2/owner/image/blobs/uploads", assertErr: requireNotFound,
		},
		{
			name: "route keywords with no name", method: http.MethodGet,
			target: "/v2/manifests/latest", assertErr: requireNotFound,
		},
		{
			name: "upload start with no name", method: http.MethodPost,
			target: "/v2/blobs/uploads/", assertErr: requireNotFound,
		},
	})
}

func TestClassifyRejectsMalformedPaths(t *testing.T) {
	longName := strings.Repeat("a", 255)
	tooLongName := strings.Repeat("a", 256)
	longMultiName := strings.Repeat("a", 127) + "/" + strings.Repeat("a", 127)
	tooLongMultiName := strings.Repeat("a", 127) + "/" + strings.Repeat("a", 128)
	longComponents := strings.Repeat("abcdefgh/", 230) // 2070 bytes, many components

	cases := []struct {
		name   string
		method string
		target string
	}{
		{"upper-case name", http.MethodGet, "/v2/Owner/image/manifests/latest"},
		{"upper-case last component", http.MethodGet, "/v2/owner/Image/manifests/latest"},
		{"percent-encoded slash in name", http.MethodGet, "/v2/owner%2Fimage/manifests/latest"},
		{"percent-encoded letter in name", http.MethodGet, "/v2/owner/im%61ge/manifests/latest"},
		{"percent-encoded tag", http.MethodGet, "/v2/owner/image/manifests/v%31"},
		{"percent-encoded upload session", http.MethodPatch, "/v2/owner/image/blobs/uploads/abc%2Fdef"},
		{"dot-dot in name", http.MethodGet, "/v2/owner/../image/manifests/latest"},
		{"dot in name", http.MethodGet, "/v2/owner/./image/manifests/latest"},
		{"dot-dot upload session", http.MethodPatch, "/v2/owner/image/blobs/uploads/.."},
		{"dot upload session", http.MethodPatch, "/v2/owner/image/blobs/uploads/."},
		{"empty component in name", http.MethodGet, "/v2/owner//image/manifests/latest"},
		{"leading empty component", http.MethodGet, "/v2//owner/image/manifests/latest"},
		{"trailing slash on manifest", http.MethodGet, "/v2/owner/image/manifests/latest/"},
		{"trailing slash on tag list", http.MethodGet, "/v2/owner/image/tags/list/"},
		{"trailing slash on upload session", http.MethodPatch, "/v2/owner/image/blobs/uploads/session-1/"},
		{"double trailing slash on upload start", http.MethodPost, "/v2/owner/image/blobs/uploads//"},
		{"no trailing slash on upload start", http.MethodPost, "/v2/owner/image/blobs/uploads"},
		{"double slash version check", http.MethodGet, "/v2//"},
		{"version check without slash", http.MethodGet, "/v2"},
		{"name component with invalid separator", http.MethodGet, "/v2/owner/-image/manifests/latest"},
		{"name component with a colon", http.MethodGet, "/v2/ghcr.io:443/image/manifests/latest"},
		{"name over 255 characters", http.MethodGet, "/v2/" + tooLongName + "/manifests/latest"},
		{"multi-component name over 255 characters", http.MethodGet, "/v2/" + tooLongMultiName + "/manifests/latest"},
		{"path over 2048 bytes", http.MethodGet, "/v2/" + longComponents + "manifests/latest"},
		{"invalid tag", http.MethodGet, "/v2/owner/image/manifests/-tag"},
		{"tag over 128 characters", http.MethodGet, "/v2/owner/image/manifests/" + strings.Repeat("t", 129)},
		{"invalid blob digest", http.MethodGet, "/v2/owner/image/blobs/sha256:abc"},
		{"upper-case blob digest", http.MethodGet, "/v2/owner/image/blobs/" + strings.ToUpper(digestA)},
		{"invalid referrers digest", http.MethodGet, "/v2/owner/image/referrers/latest"},
		{"upload session with invalid characters", http.MethodPatch, "/v2/owner/image/blobs/uploads/a:b"},
		{"upload session over 256 characters", http.MethodPatch, "/v2/owner/image/blobs/uploads/" + strings.Repeat("s", 257)},
		{"upper-case route keyword", http.MethodGet, "/v2/owner/image/Manifests/latest"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := registry.Classify(newRequest(t, testCase.method, testCase.target))
			requireNotFound(t, err)
		})
	}

	t.Run("name of exactly 255 characters", func(t *testing.T) {
		request, err := registry.Classify(newRequest(t, http.MethodGet, "/v2/"+longName+"/manifests/latest"))
		require.NoError(t, err)
		assert.Equal(t, longName, request.Name)
	})

	t.Run("multi-component name of exactly 255 characters", func(t *testing.T) {
		request, err := registry.Classify(newRequest(t, http.MethodGet, "/v2/"+longMultiName+"/manifests/latest"))
		require.NoError(t, err)
		assert.Equal(t, longMultiName, request.Name)
	})

	t.Run("tag of exactly 128 characters", func(t *testing.T) {
		tag := strings.Repeat("t", 128)
		request, err := registry.Classify(newRequest(t, http.MethodGet, "/v2/owner/image/manifests/"+tag))
		require.NoError(t, err)
		assert.Equal(t, tag, request.Reference)
	})

	t.Run("upload session of exactly 256 characters", func(t *testing.T) {
		session := strings.Repeat("s", 256)
		request, err := registry.Classify(newRequest(t, http.MethodPatch, "/v2/owner/image/blobs/uploads/"+session))
		require.NoError(t, err)
		assert.Equal(t, session, request.Session)
	})

	t.Run("GHCR upload session of digits, a dot and a UUID", func(t *testing.T) {
		session := "19.3c730f6c-c0f8-4e04-9c9c-8864a15a4a1b"
		request, err := registry.Classify(newRequest(t, http.MethodPatch, "/v2/owner/image/blobs/upload/"+session))
		require.NoError(t, err)
		assert.Equal(t, session, request.Session)
	})

	t.Run("upload session with every allowed character", func(t *testing.T) {
		session := "AZaz09._=~+-"
		request, err := registry.Classify(newRequest(t, http.MethodPatch, "/v2/owner/image/blobs/uploads/"+session))
		require.NoError(t, err)
		assert.Equal(t, session, request.Session)
	})
}

func TestClassifyUnknownPaths(t *testing.T) {
	cases := []struct{ method, target string }{
		{http.MethodGet, "/v2/_catalog"},
		{http.MethodGet, "/v2/_catalog?n=10"},
		{http.MethodGet, "/"},
		{http.MethodGet, "/v1/owner/image/manifests/latest"},
		{http.MethodGet, "/v2/owner/image"},
		{http.MethodGet, "/v2/owner/image/tags"},
		{http.MethodGet, "/v2/owner/image/tags/other"},
		{http.MethodGet, "/v2/owner/image/manifests"},
		{http.MethodGet, "/v2/owner/image/unknown/latest"},
		{http.MethodGet, "/v2/owner/image/blobs"},
		{http.MethodGet, "/healthz"},
	}
	for _, testCase := range cases {
		t.Run(testCase.method+" "+testCase.target, func(t *testing.T) {
			_, err := registry.Classify(newRequest(t, testCase.method, testCase.target))
			requireNotFound(t, err)
		})
	}
}

func TestClassifyQueryParameters(t *testing.T) {
	escapedA := strings.ReplaceAll(digestA, ":", "%3A")
	escapedB := strings.ReplaceAll(digestB, ":", "%3A")
	uploadStart := "/v2/owner/image/blobs/uploads/"
	plainUpload := func(name, query, wantURL string) classifyCase {
		return classifyCase{
			name: name, method: http.MethodPost, target: uploadStart + query,
			wantRoute: registry.RouteUploadStart, wantAction: v1alpha1.ActionPush, wantName: "owner/image",
			wantURL: wantURL,
		}
	}

	runClassifyCases(t, []classifyCase{
		plainUpload("upload start drops unknown parameters", "?foo=bar&n=1", uploadStart),
		plainUpload("mount without from is a plain upload", "?mount="+digestA, uploadStart),
		plainUpload("from without mount is a plain upload", "?from=owner/other", uploadStart),
		plainUpload("repeated mount is a plain upload", "?mount="+digestA+"&mount="+digestB+"&from=owner/other", uploadStart),
		plainUpload("repeated from is a plain upload", "?mount="+digestA+"&from=owner/other&from=owner/third", uploadStart),
		plainUpload("invalid mount digest is a plain upload", "?mount=sha256:abc&from=owner/other", uploadStart),
		plainUpload("upper-case from is a plain upload", "?mount="+digestA+"&from=Owner/other", uploadStart),
		plainUpload("invalid from is a plain upload", "?mount="+digestA+"&from=owner//other", uploadStart),
		plainUpload("empty from is a plain upload", "?mount="+digestA+"&from=", uploadStart),
		plainUpload("from equal to the name is a plain upload", "?mount="+digestA+"&from=owner/image", uploadStart),
		plainUpload("monolithic upload keeps its digest", "?digest="+digestA+"&foo=bar", uploadStart+"?digest="+escapedA),
		{
			name: "mount keeps digest too", method: http.MethodPost,
			target:    uploadStart + "?digest=" + digestB + "&mount=" + digestA + "&from=owner/other&extra=1",
			wantRoute: registry.RouteUploadStart, wantAction: v1alpha1.ActionPush, wantName: "owner/image",
			wantMount: &registry.Mount{Digest: digestA, From: "owner/other"},
			wantURL:   uploadStart + "?digest=" + escapedB + "&from=owner%2Fother&mount=" + escapedA,
		},
		{
			name: "invalid monolithic upload digest", method: http.MethodPost,
			target: uploadStart + "?digest=sha256:abc", assertErr: requireDigestInvalid,
		},
		{
			name: "repeated monolithic upload digest", method: http.MethodPost,
			target: uploadStart + "?digest=" + digestA + "&digest=" + digestA, assertErr: requireDigestInvalid,
		},
		{
			name: "upload session PUT keeps the upstream's state parameters as sent", method: http.MethodPut,
			target:    "/v2/owner/image/blobs/uploads/session-1?_state=abc%3D%3D&digest=" + digestA,
			wantRoute: registry.RouteUploadSession, wantAction: v1alpha1.ActionPush, wantName: "owner/image", wantSession: "session-1",
			wantURL: "/v2/owner/image/blobs/uploads/session-1?_state=abc%3D%3D&digest=" + digestA,
		},
		{
			name: "upload session PUT without digest", method: http.MethodPut,
			target: "/v2/owner/image/blobs/uploads/session-1?_state=abc", assertErr: requireDigestInvalid,
		},
		{
			name: "upload session PUT with invalid digest", method: http.MethodPut,
			target: "/v2/owner/image/blobs/uploads/session-1?digest=sha256:abc", assertErr: requireDigestInvalid,
		},
		{
			name: "upload session PUT with repeated digest", method: http.MethodPut,
			target: "/v2/owner/image/blobs/uploads/session-1?digest=" + digestA + "&digest=" + digestA, assertErr: requireDigestInvalid,
		},
		{
			name: "upload session PATCH drops the query", method: http.MethodPatch,
			target:    "/v2/owner/image/blobs/uploads/session-1?_state=abc",
			wantRoute: registry.RouteUploadSession, wantAction: v1alpha1.ActionPush, wantName: "owner/image", wantSession: "session-1",
			wantURL: "/v2/owner/image/blobs/uploads/session-1",
		},
		{
			name: "tag list keeps only n and last", method: http.MethodGet,
			target:    "/v2/owner/image/tags/list?n=10&last=v1&foo=bar",
			wantRoute: registry.RouteTagsList, wantAction: v1alpha1.ActionPull, wantName: "owner/image",
			wantURL: "/v2/owner/image/tags/list?last=v1&n=10",
		},
		{
			name: "referrers keep only artifactType", method: http.MethodGet,
			target:    "/v2/owner/image/referrers/" + digestA + "?artifactType=application/vnd.test&n=1",
			wantRoute: registry.RouteReferrers, wantAction: v1alpha1.ActionPull, wantName: "owner/image", wantDigest: digestA,
			wantURL: "/v2/owner/image/referrers/" + digestA + "?artifactType=application%2Fvnd.test",
		},
		{
			name: "manifest drops the query", method: http.MethodGet,
			target:    "/v2/owner/image/manifests/latest?ns=docker.io",
			wantRoute: registry.RouteManifest, wantAction: v1alpha1.ActionPull, wantName: "owner/image", wantRef: "latest",
			wantURL: "/v2/owner/image/manifests/latest",
		},
		{
			name: "blob drops the query", method: http.MethodDelete,
			target:    "/v2/owner/image/blobs/" + digestA + "?x=1",
			wantRoute: registry.RouteBlob, wantAction: v1alpha1.ActionDelete, wantName: "owner/image", wantDigest: digestA,
			wantURL: "/v2/owner/image/blobs/" + digestA,
		},
		{
			name: "version check drops the query", method: http.MethodGet, target: "/v2/?x=1",
			wantRoute: registry.RouteBase, wantURL: "/v2/",
		},
	})
}

func TestUpstreamURLReflectsDroppedMount(t *testing.T) {
	request, err := registry.Classify(newRequest(t, http.MethodPost,
		"/v2/owner/image/blobs/uploads/?mount="+digestA+"&from=owner/other"))
	require.NoError(t, err)
	require.NotNil(t, request.Mount)

	request.Mount = nil
	assert.Equal(t, "/v2/owner/image/blobs/uploads/", request.UpstreamURL().String())
}
