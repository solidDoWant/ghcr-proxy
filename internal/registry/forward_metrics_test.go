package registry_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/ghcr-proxy/internal/metrics"
	"github.com/solidDoWant/ghcr-proxy/internal/registry"
	"github.com/solidDoWant/ghcr-proxy/test/fakeghcr"
)

func withMetrics(proxyMetrics *metrics.Metrics) func(*registry.ForwarderConfig) {
	return func(cfg *registry.ForwarderConfig) { cfg.Metrics = proxyMetrics }
}

func TestForwarderCountsUpstreamRequestsAndMirrorFallbacks(t *testing.T) {
	proxyMetrics := metrics.New()
	upstreamFake := fakeghcr.New(t, fakeghcr.Options{})
	policy := newTestMirror(t, testMirrorOptions{fallback: true})
	pushMirrorImage(t, "owner/app", upstreamFake, policy)

	harness := newProxyHarness(t, upstreamFake, upstreamFake.PAT(), decideByOwner, withMirrors(policy, nil), withMetrics(proxyMetrics))

	status, _, _ := harness.do(t, http.MethodGet, "/v2/owner/app/manifests/v1")
	require.Equal(t, http.StatusOK, status)

	policy.mode.Store(mirrorFails)

	status, _, _ = harness.do(t, http.MethodGet, "/v2/owner/app/manifests/v1")
	require.Equal(t, http.StatusOK, status)

	require.NoError(t, testutil.GatherAndCompare(proxyMetrics.Gatherer(), strings.NewReader(`
# HELP ghcr_proxy_mirror_fallbacks_total Pulls that fell back from a mirror to the upstream.
# TYPE ghcr_proxy_mirror_fallbacks_total counter
ghcr_proxy_mirror_fallbacks_total{mirror="policy"} 1
# HELP ghcr_proxy_upstream_requests_total Requests sent to the upstream or a mirror, by status class.
# TYPE ghcr_proxy_upstream_requests_total counter
ghcr_proxy_upstream_requests_total{code="2xx",target="policy_mirror"} 1
ghcr_proxy_upstream_requests_total{code="2xx",target="upstream"} 1
ghcr_proxy_upstream_requests_total{code="5xx",target="policy_mirror"} 1
`), "ghcr_proxy_mirror_fallbacks_total", "ghcr_proxy_upstream_requests_total"))

	count, err := testutil.GatherAndCount(proxyMetrics.Gatherer(), "ghcr_proxy_upstream_request_duration_seconds")
	require.NoError(t, err)
	assert.Equal(t, 2, count, "a duration histogram for the policy mirror and one for the upstream")
}

// The series count stays bounded when every kind of request runs against both mirrors.
func TestMetricsSeriesCountStaysSmallWithBothMirrors(t *testing.T) {
	proxyMetrics := metrics.New()
	upstreamFake := fakeghcr.New(t, fakeghcr.Options{})
	policy := newTestMirror(t, testMirrorOptions{fallback: true})
	anonymous := newTestMirror(t, testMirrorOptions{fallback: true, anonymous: true})

	pushMirrorImage(t, "owner/app", upstreamFake, policy)
	pushMirrorImage(t, "anon/app", upstreamFake, anonymous)
	pushMirrorImage(t, "denied/app", upstreamFake, anonymous)

	harness := newProxyHarness(t, upstreamFake, upstreamFake.PAT(), decideByOwner,
		withMirrors(policy, anonymous), withMetrics(proxyMetrics))

	for _, path := range []string{
		"/v2/owner/app/manifests/v1", "/v2/anon/app/manifests/v1", "/v2/denied/app/manifests/v1",
		"/v2/owner/missing/manifests/v1", "/v2/anon/missing/manifests/v1",
	} {
		harness.do(t, http.MethodGet, path)
	}

	policy.mode.Store(mirrorFails)
	anonymous.mode.Store(mirrorFails)

	for _, path := range []string{"/v2/owner/app/manifests/v1", "/v2/anon/app/manifests/v1"} {
		harness.do(t, http.MethodGet, path)
	}

	families, err := proxyMetrics.Gatherer().Gather()
	require.NoError(t, err)

	series := 0

	for _, family := range families {
		series += len(family.GetMetric())
		if family.GetType().String() == "HISTOGRAM" {
			series += (len(family.GetMetric()[0].GetHistogram().GetBucket()) + 2) * len(family.GetMetric())
		}
	}

	assert.Positive(t, series)
	assert.LessOrEqual(t, series, 250)
}
