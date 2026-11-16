package metrics_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/internal/metrics"
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
		"registrygate_requests_total": `
# HELP registrygate_requests_total Requests served, by action, decision and status class.
# TYPE registrygate_requests_total counter
registrygate_requests_total{action="none",code="4xx",decision="unauthenticated"} 1
registrygate_requests_total{action="pull",code="2xx",decision="allowed"} 2
`,
		"registrygate_upstream_requests_total": `
# HELP registrygate_upstream_requests_total Requests sent to the upstream or a mirror, by status class.
# TYPE registrygate_upstream_requests_total counter
registrygate_upstream_requests_total{code="5xx",target="upstream"} 1
registrygate_upstream_requests_total{code="error",target="policy_mirror"} 1
`,
		"registrygate_mirror_fallbacks_total": `
# HELP registrygate_mirror_fallbacks_total Pulls that fell back from a mirror to the upstream.
# TYPE registrygate_mirror_fallbacks_total counter
registrygate_mirror_fallbacks_total{mirror="policy"} 1
`,
		"registrygate_token_reviews_total": `
# HELP registrygate_token_reviews_total ServiceAccount token reviews, by result and cache use.
# TYPE registrygate_token_reviews_total counter
registrygate_token_reviews_total{cache="hit",result="authenticated"} 1
registrygate_token_reviews_total{cache="miss",result="error"} 1
`,
		"registrygate_upstream_token_fetches_total": `
# HELP registrygate_upstream_token_fetches_total Registry token fetches, by target and result.
# TYPE registrygate_upstream_token_fetches_total counter
registrygate_upstream_token_fetches_total{result="ok",target="upstream"} 1
`,
		"registrygate_credential_reloads_total": `
# HELP registrygate_credential_reloads_total Credential file reloads, by kind and result.
# TYPE registrygate_credential_reloads_total counter
registrygate_credential_reloads_total{kind="pat",result="ok"} 1
registrygate_credential_reloads_total{kind="tls",result="error"} 1
`,
		"registrygate_policies": `
# HELP registrygate_policies GHCRAccessPolicy objects in the local index, by whether they are inside the grants.
# TYPE registrygate_policies gauge
registrygate_policies{ready="false"} 1
registrygate_policies{ready="true"} 3
`,
	}

	for name, expected := range expectations {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, testutil.GatherAndCompare(proxyMetrics.Gatherer(), strings.NewReader(expected), name))
		})
	}

	count, err := testutil.GatherAndCount(proxyMetrics.Gatherer(), "registrygate_upstream_request_duration_seconds")
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
