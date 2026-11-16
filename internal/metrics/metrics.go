// Package metrics holds the proxy's Prometheus metrics. Every label has a small fixed set of values; no label ever
// holds a repository, namespace, ServiceAccount or any other caller-chosen text. Every method accepts a nil
// *Metrics and then does nothing, so components work without metrics in tests.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Upstream targets.
const (
	TargetUpstream        = "upstream"
	TargetPolicyMirror    = "policy_mirror"
	TargetAnonymousMirror = "anonymous_mirror"
	TargetAPI             = "api"
)

// Results of a token review, a token fetch or a credential reload.
const (
	ResultAuthenticated = "authenticated"
	ResultRejected      = "rejected"
	ResultError         = "error"
	ResultOK            = "ok"
)

// Kinds of reloaded credential.
const (
	KindPAT    = "pat"
	KindTLS    = "tls"
	KindMirror = "mirror"
)

// Mirrors that can fall back to the upstream.
const (
	MirrorPolicy    = "policy"
	MirrorAnonymous = "anonymous"
)

// CodeError is the code label of an upstream request that got no answer.
const CodeError = "error"

// Metrics is the set of the proxy's collectors.
type Metrics struct {
	registry *prometheus.Registry

	requests          *prometheus.CounterVec
	upstreamDuration  *prometheus.HistogramVec
	upstreamRequests  *prometheus.CounterVec
	mirrorFallbacks   *prometheus.CounterVec
	tokenReviews      *prometheus.CounterVec
	upstreamTokens    *prometheus.CounterVec
	credentialReloads *prometheus.CounterVec
	policies          *policyCollector
}

// New returns Metrics on a registry of its own. The Go collector is trimmed to the runtime memory and GC metrics.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registrygate_requests_total", Help: "Requests served, by action, decision and status class.",
		}, []string{"action", "decision", "code"}),
		upstreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "registrygate_upstream_request_duration_seconds",
			Help:    "Time to the upstream's or a mirror's response headers.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 10, 60},
		}, []string{"target"}),
		upstreamRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registrygate_upstream_requests_total", Help: "Requests sent to the upstream or a mirror, by status class.",
		}, []string{"target", "code"}),
		mirrorFallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registrygate_mirror_fallbacks_total", Help: "Pulls that fell back from a mirror to the upstream.",
		}, []string{"mirror"}),
		tokenReviews: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registrygate_token_reviews_total", Help: "ServiceAccount token reviews, by result and cache use.",
		}, []string{"result", "cache"}),
		upstreamTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registrygate_upstream_token_fetches_total", Help: "Registry token fetches, by target and result.",
		}, []string{"target", "result"}),
		credentialReloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registrygate_credential_reloads_total", Help: "Credential file reloads, by kind and result.",
		}, []string{"kind", "result"}),
		policies: &policyCollector{desc: prometheus.NewDesc(
			"registrygate_policies", "GHCRAccessPolicy objects in the local index, by whether they are inside the grants.",
			[]string{"ready"}, nil)},
	}

	m.registry.MustRegister(
		collectors.NewGoCollector(collectors.WithGoCollectorRuntimeMetrics(
			collectors.GoRuntimeMetricsRule{Matcher: runtimeMetricsMatcher})),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.requests, m.upstreamDuration, m.upstreamRequests, m.mirrorFallbacks, m.tokenReviews,
		m.upstreamTokens, m.credentialReloads, m.policies,
	)

	return m
}

// Gatherer returns the registry to serve at /metrics.
func (m *Metrics) Gatherer() prometheus.Gatherer {
	if m == nil {
		return New().registry
	}

	return m.registry
}

// Request counts one served request.
func (m *Metrics) Request(action, decision string, status int) {
	if m != nil {
		m.requests.WithLabelValues(action, decision, StatusClass(status)).Inc()
	}
}

// UpstreamRequest records one request to target: its time to response headers and its status, or CodeError when
// status is 0.
func (m *Metrics) UpstreamRequest(target string, status int, elapsed time.Duration) {
	if m == nil {
		return
	}

	code := CodeError
	if status != 0 {
		code = StatusClass(status)
	}

	m.upstreamDuration.WithLabelValues(target).Observe(elapsed.Seconds())
	m.upstreamRequests.WithLabelValues(target, code).Inc()
}

// MirrorFallback counts a pull that went to the upstream because a mirror failed.
func (m *Metrics) MirrorFallback(mirror string) {
	if m != nil {
		m.mirrorFallbacks.WithLabelValues(mirror).Inc()
	}
}

// TokenReview counts one token review answer.
func (m *Metrics) TokenReview(result string, cacheHit bool) {
	if m == nil {
		return
	}

	cache := "miss"
	if cacheHit {
		cache = "hit"
	}

	m.tokenReviews.WithLabelValues(result, cache).Inc()
}

// UpstreamTokenFetch counts one registry token fetch.
func (m *Metrics) UpstreamTokenFetch(target, result string) {
	if m != nil {
		m.upstreamTokens.WithLabelValues(target, result).Inc()
	}
}

// CredentialReload counts one reload of a credential file.
func (m *Metrics) CredentialReload(kind string, err error) {
	if m == nil {
		return
	}

	result := ResultOK
	if err != nil {
		result = ResultError
	}

	m.credentialReloads.WithLabelValues(kind, result).Inc()
}

// SetPolicyCounts sets where the policies gauge reads its counts from, at scrape time.
func (m *Metrics) SetPolicyCounts(counts func() (ready, notReady int)) {
	if m != nil {
		m.policies.counts.Store(&counts)
	}
}

// StatusClass is the status code's class: 1xx to 5xx.
func StatusClass(status int) string {
	if status < 100 || status > 599 {
		return "unknown"
	}

	return strconv.Itoa(status/100) + "xx"
}

// Handler serves gatherer in the Prometheus text format.
func Handler(gatherer prometheus.Gatherer) http.Handler {
	return promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{})
}
