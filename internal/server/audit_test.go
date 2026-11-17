package server_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lockedBuffer is a writer the audit log can fill while the test reads it.
type lockedBuffer struct {
	mutex  sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	return b.buffer.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	return b.buffer.String()
}

// lines waits until count audit lines were written and returns them decoded.
func (b *lockedBuffer) lines(t *testing.T, count int) []map[string]any {
	t.Helper()

	require.Eventually(t, func() bool {
		return strings.Count(b.String(), "\n") >= count
	}, 5*1e9, 5*1e6)

	var out []map[string]any

	for _, raw := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		var line map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &line), raw)

		out = append(out, line)
	}

	require.Len(t, out, count, "exactly one line per request")

	return out
}

func putManifest(t *testing.T, proxy *harness, target, authorization string, body []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, proxy.url()+target, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/vnd.oci.image.index.v1+json")
	req.Header.Set("Authorization", authorization)

	resp, err := proxy.client().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestAuditWritesOneLineForEachKindOfDecision(t *testing.T) {
	tests := []struct {
		name          string
		method        string
		target        string
		authorization []string
		want          map[string]any
	}{
		{
			name: "policy allowed push", method: http.MethodPost, target: "/v2/soliddowant/foo/app/blobs/uploads/",
			authorization: []string{basic(builderToken)},
			want: map[string]any{
				"namespace": "ci-foo", "serviceAccount": "builder", "pod": "build-1", "repository": "soliddowant/foo/app",
				"action": "push", "decision": "allowed", "policy": "build", "status": float64(202), "upstreamStatus": float64(202),
			},
		},
		{
			name: "pull the policy does not cover", method: http.MethodGet, target: "/v2/other/img/manifests/latest",
			authorization: []string{basic(builderToken)},
			want: map[string]any{
				"namespace": "ci-foo", "serviceAccount": "builder", "repository": "other/img", "action": "pull",
				"decision": "anonymous", "status": float64(401),
			},
		},
		{
			name: "policy allowed pull of a missing image", method: http.MethodGet,
			target:        "/v2/soliddowant/foo/source/manifests/latest",
			authorization: []string{basic(builderToken)},
			want: map[string]any{
				"repository": "soliddowant/foo/source", "action": "pull", "decision": "allowed", "policy": "build",
				"status": float64(404), "upstreamStatus": float64(404),
			},
		},
		{
			name: "anonymous pull", method: http.MethodGet, target: "/v2/other/img/manifests/latest",
			want: map[string]any{
				"repository": "other/img", "action": "pull", "decision": "unauthenticated", "status": float64(401),
			},
		},
		{
			name: "denied push", method: http.MethodPost, target: "/v2/soliddowant/foo/other/blobs/uploads/",
			authorization: []string{basic(builderToken)},
			want: map[string]any{
				"namespace": "ci-foo", "serviceAccount": "builder", "pod": "build-1", "repository": "soliddowant/foo/other",
				"action": "push", "decision": "denied", "status": float64(403),
			},
		},
		{
			name: "anonymous push", method: http.MethodPost, target: "/v2/soliddowant/foo/app/blobs/uploads/",
			want: map[string]any{
				"repository": "soliddowant/foo/app", "action": "push", "decision": "unauthenticated", "status": float64(401),
			},
		},
		{
			name: "rejected token", method: http.MethodGet, target: "/v2/other/img/manifests/latest",
			authorization: []string{basic(rejectedToken)},
			want: map[string]any{
				"repository": "other/img", "action": "pull", "decision": "unauthenticated", "status": float64(401),
			},
		},
		{
			name: "token review unavailable", method: http.MethodGet, target: "/v2/other/img/manifests/latest",
			authorization: []string{basic(unavailableToken)},
			want: map[string]any{
				"repository": "other/img", "action": "pull", "decision": "unavailable", "status": float64(503),
			},
		},
		{
			name: "unknown route", method: http.MethodGet, target: "/v2/_catalog",
			want: map[string]any{"action": "unknown", "decision": "notfound", "status": float64(404)},
		},
		{
			name: "invalid digest", method: http.MethodPost,
			target:        "/v2/soliddowant/foo/app/blobs/uploads/?digest=nonsense",
			authorization: []string{basic(builderToken)},
			want:          map[string]any{"action": "unknown", "decision": "invalid", "status": float64(400)},
		},
		{
			name: "version check", method: http.MethodGet, target: "/v2/", authorization: []string{basic(builderToken)},
			want: map[string]any{
				"namespace": "ci-foo", "serviceAccount": "builder", "pod": "build-1", "action": "none",
				"decision": "allowed", "status": float64(200),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out lockedBuffer

			proxy := newHarness(t, harnessOptions{audit: &out})
			proxy.do(t, test.method, test.target, test.authorization...)

			line := out.lines(t, 1)[0]

			for key, want := range test.want {
				assert.Equal(t, want, line[key], key)
			}

			// Fields the case does not name are absent or the always-present ones.
			assert.Equal(t, test.method, line["method"])
			assert.Equal(t, strings.SplitN(test.target, "?", 2)[0], line["path"], "the path never carries the query")
			assert.NotEmpty(t, line["clientIP"])
			assert.NotEmpty(t, line["time"])
			assert.Contains(t, line, "bytes")
			assert.Contains(t, line, "durationMs")

			if _, named := test.want["upstreamStatus"]; !named {
				assert.NotContains(t, line, "upstreamStatus", "nothing reached the upstream")
			}
		})
	}
}

func TestAuditManifestPushCarriesThePushedDigest(t *testing.T) {
	var out lockedBuffer

	proxy := newHarness(t, harnessOptions{audit: &out})

	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	sum := sha256.Sum256(manifest)

	putManifest(t, proxy, "/v2/soliddowant/foo/app/manifests/latest", basic(builderToken), manifest)

	line := out.lines(t, 1)[0]
	assert.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), line["digest"])
	assert.Equal(t, "push", line["action"])
	assert.Equal(t, float64(201), line["status"])
}

func TestAuditLinesNeverContainSecrets(t *testing.T) {
	var out lockedBuffer

	proxy := newHarness(t, harnessOptions{audit: &out})

	requests := []struct {
		target        string
		authorization string
	}{
		{"/v2/soliddowant/foo/app/blobs/uploads/?digest=" + builderToken, basic(builderToken)},
		{"/v2/soliddowant/foo/source/manifests/latest", basic(builderToken)},
		{"/v2/soliddowant/foo/source/manifests/latest", basic(rejectedToken)},
		{"/v2/soliddowant/foo/source/manifests/latest", "Bearer " + builderToken},
		{"/v2/soliddowant/foo/source/manifests/latest", basic(unavailableToken)},
	}

	for _, request := range requests {
		proxy.do(t, http.MethodGet, request.target, request.authorization)
	}

	lines := out.lines(t, len(requests))
	require.Len(t, lines, len(requests))

	written := out.String()

	for _, secret := range []string{
		proxy.fake.PAT(), builderToken, rejectedToken, unavailableToken, "Bearer", "Basic", basic(builderToken),
	} {
		assert.NotContains(t, written, secret)
	}
}
