package server_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/internal/metrics"
	"github.com/solidDoWant/registrygate/test/fakeregistry"
)

func TestForwarderCountsUpstreamRequestsAndMirrorFallbacks(t *testing.T) {
	proxyMetrics := metrics.New()
	mirrorFake := fakeregistry.New(t, fakeregistry.Options{Auth: fakeregistry.Bearer, UploadPath: fakeregistry.Uploads})
	proxy := newHarness(t, harnessOptions{metrics: proxyMetrics, configure: func(objects *objects) {
		mirrorOf(t, objects, mirrorFake, "proxy-cache", true)
	}})

	image := randomImage(t)
	require.NoError(t, proxy.fake.Push("soliddowant/foo/lib", image))
	proxy.fake.MakePublic("soliddowant/foo/lib")
	require.NoError(t, mirrorFake.Push("proxy-cache/soliddowant/foo/lib", image))

	require.Equal(t, http.StatusOK, proxy.do(t, http.MethodGet, "/v2/soliddowant/foo/lib/manifests/latest", basic(builderToken)).status)

	require.NoError(t, mirrorFake.Close())

	require.Equal(t, http.StatusOK, proxy.do(t, http.MethodGet, "/v2/soliddowant/foo/lib/manifests/latest", basic(builderToken)).status)

	require.NoError(t, testutil.GatherAndCompare(proxyMetrics.Gatherer(), strings.NewReader(`
# HELP registrygate_mirror_fallbacks_total Pulls that fell back from a mirror to the upstream.
# TYPE registrygate_mirror_fallbacks_total counter
registrygate_mirror_fallbacks_total{upstream="registrygate/upstream-a"} 1
`), "registrygate_mirror_fallbacks_total"))

	count, err := testutil.GatherAndCount(proxyMetrics.Gatherer(), "registrygate_upstream_requests_total")
	require.NoError(t, err)
	assert.Equal(t, 3, count, "the mirror's answer, its failure and the upstream's answer")
}
