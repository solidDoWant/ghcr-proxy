package registry_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/internal/registry"
)

func TestClassifyRefusesNonCanonicalPaths(t *testing.T) {
	var (
		requireNameInvalid = requireRegistryError(http.StatusBadRequest, "NAME_INVALID")
		requireUnsupported = requireRegistryError(http.StatusBadRequest, "UNSUPPORTED")
	)

	cases := []struct {
		name      string
		method    string
		target    string
		assertErr require.ErrorAssertionFunc
	}{
		{"percent-encoded slash in name", http.MethodGet, "/v2/owner%2Fimage/manifests/latest", requireNameInvalid},
		{"percent-encoded letter in name", http.MethodGet, "/v2/owner/im%61ge/manifests/latest", requireNameInvalid},
		{"percent-encoded tag", http.MethodGet, "/v2/owner/image/manifests/v%31", requireNameInvalid},
		{"percent-encoded upload session", http.MethodPatch, "/v2/owner/image/blobs/uploads/abc%2Fdef", requireNameInvalid},
		{"percent-encoded version segment", http.MethodGet, "/%76%32/owner/image/manifests/latest", requireUnsupported},
		{"percent-encoded path outside v2", http.MethodGet, "/other/%61", requireUnsupported},
		{"dot-dot in name", http.MethodGet, "/v2/owner/../image/manifests/latest", requireNameInvalid},
		{"dot in name", http.MethodGet, "/v2/owner/./image/manifests/latest", requireNameInvalid},
		{"dot-dot upload session", http.MethodPatch, "/v2/owner/image/blobs/uploads/..", requireNameInvalid},
		{"dot upload session", http.MethodPatch, "/v2/owner/image/blobs/uploads/.", requireNameInvalid},
		{"dot-dot climbing out of v2", http.MethodGet, "/v2/../other", requireNameInvalid},
		{"dot-dot before v2", http.MethodGet, "/../v2/owner/image/manifests/latest", requireUnsupported},
		{"empty component in name", http.MethodGet, "/v2/owner//image/manifests/latest", requireNameInvalid},
		{"leading empty component", http.MethodGet, "/v2//owner/image/manifests/latest", requireNameInvalid},
		{"double slash before v2", http.MethodGet, "//v2/owner/image/manifests/latest", requireUnsupported},
		{"double trailing slash on upload start", http.MethodPost, "/v2/owner/image/blobs/uploads//", requireNameInvalid},
		{"double slash version check", http.MethodGet, "/v2//", requireNameInvalid},
		{"upper-case version segment", http.MethodGet, "/V2/owner/image/manifests/latest", requireUnsupported},
		{"mixed-case version check", http.MethodGet, "/V2/", requireUnsupported},
		{"path outside v2", http.MethodGet, "/v3/owner/image/manifests/latest", requireNotFound},
		{"root", http.MethodGet, "/", requireNotFound},
		{"health path", http.MethodGet, "/healthz", requireNotFound},
		{"version segment with a suffix", http.MethodGet, "/v2x/owner/image/manifests/latest", requireNotFound},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// An origin-form target, as a client sends it: the request URI is the raw path.
			req := httptest.NewRequestWithContext(t.Context(), testCase.method, testCase.target, nil)

			_, err := registry.Classify(req)
			testCase.assertErr(t, err)
		})
	}

	t.Run("the raw target is judged, not the parsed path", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v2/owner/../image/manifests/latest", nil)
		// A cleaned path in the parsed URL must not hide the client's dot-dot segment.
		req.URL.Path, req.URL.RawPath = "/v2/image/manifests/latest", ""

		_, err := registry.Classify(req)
		requireNameInvalid(t, err)
	})

	t.Run("a single trailing slash is canonical", func(t *testing.T) {
		request, err := registry.Classify(
			httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v2/owner/image/blobs/uploads/", nil))
		require.NoError(t, err)
		require.Equal(t, registry.RouteUploadStart, request.Route)

		request, err = registry.Classify(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v2/", nil))
		require.NoError(t, err)
		require.Equal(t, registry.RouteBase, request.Route)
	})
}
