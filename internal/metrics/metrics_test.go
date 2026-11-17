package metrics_test

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/internal/metrics"
)

func TestRecordedEventsAppearWithTheUpstreamLabel(t *testing.T) {
	proxyMetrics := metrics.New()

	proxyMetrics.Request("adm/a", "pull", "mirror", 200)
	proxyMetrics.Request("adm/a", "pull", "ambiguous", 403)
	proxyMetrics.Request("", "none", "notfound", 404)
	proxyMetrics.UpstreamRequest("adm/a", metrics.TargetUpstream, 503, time.Millisecond)
	proxyMetrics.UpstreamRequest("adm/b", metrics.TargetMirror, 0, time.Millisecond)
	proxyMetrics.MirrorFallback("adm/a")
	proxyMetrics.TokenReview(metrics.ResultAuthenticated, true)
	proxyMetrics.UpstreamTokenFetch("adm/a", metrics.TargetMirror, metrics.ResultOK)
	proxyMetrics.SecretUpdate(metrics.ResultInvalid)
	proxyMetrics.SetObjectCounts(func() []metrics.ObjectCount {
		return []metrics.ObjectCount{
			{Kind: metrics.KindRegistryMirror, Ready: metrics.ReadyTrue, Count: 3},
			{Kind: metrics.KindRegistryMirror, Ready: metrics.ReadyPending, Count: 1},
		}
	})

	expectations := map[string]string{
		"registrygate_requests_total": `
# HELP registrygate_requests_total Requests served, by upstream, action, decision and status class.
# TYPE registrygate_requests_total counter
registrygate_requests_total{action="none",code="4xx",decision="notfound",upstream="none"} 1
registrygate_requests_total{action="pull",code="2xx",decision="mirror",upstream="adm/a"} 1
registrygate_requests_total{action="pull",code="4xx",decision="ambiguous",upstream="adm/a"} 1
`,
		"registrygate_upstream_requests_total": `
# HELP registrygate_upstream_requests_total Requests sent to the upstream or a mirror, by status class.
# TYPE registrygate_upstream_requests_total counter
registrygate_upstream_requests_total{code="5xx",target="upstream",upstream="adm/a"} 1
registrygate_upstream_requests_total{code="error",target="mirror",upstream="adm/b"} 1
`,
		"registrygate_mirror_fallbacks_total": `
# HELP registrygate_mirror_fallbacks_total Pulls that fell back from a mirror to the upstream.
# TYPE registrygate_mirror_fallbacks_total counter
registrygate_mirror_fallbacks_total{upstream="adm/a"} 1
`,
		"registrygate_token_reviews_total": `
# HELP registrygate_token_reviews_total ServiceAccount token reviews, by result and cache use.
# TYPE registrygate_token_reviews_total counter
registrygate_token_reviews_total{cache="hit",result="authenticated"} 1
`,
		"registrygate_upstream_token_fetches_total": `
# HELP registrygate_upstream_token_fetches_total Registry token fetches, by upstream, target and result.
# TYPE registrygate_upstream_token_fetches_total counter
registrygate_upstream_token_fetches_total{result="ok",target="mirror",upstream="adm/a"} 1
`,
		"registrygate_secret_updates_total": `
# HELP registrygate_secret_updates_total Secret updates the proxy saw, by result.
# TYPE registrygate_secret_updates_total counter
registrygate_secret_updates_total{result="invalid"} 1
registrygate_secret_updates_total{result="ok"} 0
registrygate_secret_updates_total{result="unavailable"} 0
`,
		"registrygate_objects": `
# HELP registrygate_objects Custom resources in the model snapshot, by kind and readiness.
# TYPE registrygate_objects gauge
registrygate_objects{kind="RegistryMirror",ready="pending"} 1
registrygate_objects{kind="RegistryMirror",ready="true"} 3
`,
	}

	for name, expected := range expectations {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, testutil.GatherAndCompare(proxyMetrics.Gatherer(), strings.NewReader(expected), name))
		})
	}

	count, err := testutil.GatherAndCount(proxyMetrics.Gatherer(), "registrygate_upstream_request_duration_seconds")
	require.NoError(t, err)
	assert.Equal(t, 2, count, "one histogram per upstream and target seen")
}

func TestOnlyTheUpstreamLabelNamesAnObject(t *testing.T) {
	proxyMetrics := metrics.New()
	proxyMetrics.Request("adm/a", "pull", "allowed", 200)
	proxyMetrics.UpstreamRequest("adm/a", metrics.TargetUpstream, 200, time.Millisecond)
	proxyMetrics.MirrorFallback("adm/a")
	proxyMetrics.UpstreamTokenFetch("adm/a", metrics.TargetUpstream, metrics.ResultOK)

	families, err := proxyMetrics.Gatherer().Gather()
	require.NoError(t, err)

	allowed := map[string]bool{
		"upstream": true, "action": true, "decision": true, "code": true, "target": true, "result": true,
		"cache": true, "kind": true, "ready": true, "le": true,
	}

	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), "registrygate_") {
			continue
		}

		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				assert.True(t, allowed[label.GetName()], "%s has label %s", family.GetName(), label.GetName())
			}
		}
	}
}

func TestNilMetricsDoNothing(t *testing.T) {
	var proxyMetrics *metrics.Metrics

	assert.NotPanics(t, func() {
		proxyMetrics.Request("", "pull", "allowed", 200)
		proxyMetrics.UpstreamRequest("", metrics.TargetMirror, 200, time.Second)
		proxyMetrics.MirrorFallback("")
		proxyMetrics.TokenReview(metrics.ResultRejected, false)
		proxyMetrics.UpstreamTokenFetch("", metrics.TargetMirror, metrics.ResultError)
		proxyMetrics.SecretUpdate(metrics.ResultOK)
		proxyMetrics.SetObjectCounts(nil)
	})
}

func TestStatusClass(t *testing.T) {
	for status, want := range map[int]string{100: "1xx", 204: "2xx", 307: "3xx", 404: "4xx", 599: "5xx", 0: "unknown", 600: "unknown"} {
		assert.Equal(t, want, metrics.StatusClass(status), status)
	}
}
