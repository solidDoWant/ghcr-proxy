package metrics_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/ghcr-proxy/internal/metrics"
)

func TestRecordedEventsAppearWithFixedLabels(t *testing.T) {
	proxyMetrics := metrics.New()

	proxyMetrics.Request("pull", "allowed", 200)
	proxyMetrics.Request("pull", "allowed", 206)
	proxyMetrics.Request("none", "unauthenticated", 401)
	proxyMetrics.UpstreamRequest(metrics.TargetUpstream, 503, time.Millisecond)
	proxyMetrics.UpstreamRequest(metrics.TargetPolicyMirror, 0, time.Millisecond)
	proxyMetrics.MirrorFallback(metrics.MirrorPolicy)
	proxyMetrics.TokenReview(metrics.ResultAuthenticated, true)
	proxyMetrics.TokenReview(metrics.ResultError, false)
	proxyMetrics.UpstreamTokenFetch(metrics.TargetUpstream, metrics.ResultOK)
	proxyMetrics.CredentialReload(metrics.KindPAT, nil)
	proxyMetrics.CredentialReload(metrics.KindTLS, errors.New("bad pair"))
	proxyMetrics.SetPolicyCounts(func() (int, int) { return 3, 1 })

	expectations := map[string]string{
		"ghcr_proxy_requests_total": `
# HELP ghcr_proxy_requests_total Requests served, by action, decision and status class.
# TYPE ghcr_proxy_requests_total counter
ghcr_proxy_requests_total{action="none",code="4xx",decision="unauthenticated"} 1
ghcr_proxy_requests_total{action="pull",code="2xx",decision="allowed"} 2
`,
		"ghcr_proxy_upstream_requests_total": `
# HELP ghcr_proxy_upstream_requests_total Requests sent to the upstream or a mirror, by status class.
# TYPE ghcr_proxy_upstream_requests_total counter
ghcr_proxy_upstream_requests_total{code="5xx",target="upstream"} 1
ghcr_proxy_upstream_requests_total{code="error",target="policy_mirror"} 1
`,
		"ghcr_proxy_mirror_fallbacks_total": `
# HELP ghcr_proxy_mirror_fallbacks_total Pulls that fell back from a mirror to the upstream.
# TYPE ghcr_proxy_mirror_fallbacks_total counter
ghcr_proxy_mirror_fallbacks_total{mirror="policy"} 1
`,
		"ghcr_proxy_token_reviews_total": `
# HELP ghcr_proxy_token_reviews_total ServiceAccount token reviews, by result and cache use.
# TYPE ghcr_proxy_token_reviews_total counter
ghcr_proxy_token_reviews_total{cache="hit",result="authenticated"} 1
ghcr_proxy_token_reviews_total{cache="miss",result="error"} 1
`,
		"ghcr_proxy_upstream_token_fetches_total": `
# HELP ghcr_proxy_upstream_token_fetches_total Registry token fetches, by target and result.
# TYPE ghcr_proxy_upstream_token_fetches_total counter
ghcr_proxy_upstream_token_fetches_total{result="ok",target="upstream"} 1
`,
		"ghcr_proxy_credential_reloads_total": `
# HELP ghcr_proxy_credential_reloads_total Credential file reloads, by kind and result.
# TYPE ghcr_proxy_credential_reloads_total counter
ghcr_proxy_credential_reloads_total{kind="pat",result="ok"} 1
ghcr_proxy_credential_reloads_total{kind="tls",result="error"} 1
`,
		"ghcr_proxy_policies": `
# HELP ghcr_proxy_policies GHCRAccessPolicy objects in the local index, by whether they are inside the grants.
# TYPE ghcr_proxy_policies gauge
ghcr_proxy_policies{ready="false"} 1
ghcr_proxy_policies{ready="true"} 3
`,
	}

	for name, expected := range expectations {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, testutil.GatherAndCompare(proxyMetrics.Gatherer(), strings.NewReader(expected), name))
		})
	}

	count, err := testutil.GatherAndCount(proxyMetrics.Gatherer(), "ghcr_proxy_upstream_request_duration_seconds")
	require.NoError(t, err)
	assert.Equal(t, 2, count, "one histogram per target seen")
}

func TestNilMetricsDoNothing(t *testing.T) {
	var proxyMetrics *metrics.Metrics

	assert.NotPanics(t, func() {
		proxyMetrics.Request("pull", "allowed", 200)
		proxyMetrics.UpstreamRequest(metrics.TargetAPI, 200, time.Second)
		proxyMetrics.MirrorFallback(metrics.MirrorAnonymous)
		proxyMetrics.TokenReview(metrics.ResultRejected, false)
		proxyMetrics.UpstreamTokenFetch(metrics.TargetAPI, metrics.ResultError)
		proxyMetrics.CredentialReload(metrics.KindMirror, nil)
		proxyMetrics.SetPolicyCounts(nil)
	})
}

func TestStatusClass(t *testing.T) {
	for status, want := range map[int]string{100: "1xx", 204: "2xx", 307: "3xx", 404: "4xx", 599: "5xx", 0: "unknown", 600: "unknown"} {
		assert.Equal(t, want, metrics.StatusClass(status), status)
	}
}
