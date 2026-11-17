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

// NoUpstream is the upstream label of a request that named no upstream: it came before its host resolved.
const NoUpstream = "none"

// Targets of an upstream request.
const (
	TargetUpstream = "upstream"
	TargetMirror   = "mirror"
)

// Results of a token review, a token fetch or a Secret update.
const (
	ResultAuthenticated = "authenticated"
	ResultRejected      = "rejected"
	ResultError         = "error"
	ResultOK            = "ok"
	ResultInvalid       = "invalid"
	ResultUnavailable   = "unavailable"
)

// Kinds of custom resource counted by the objects gauge.
const (
	KindUpstreamRegistry   = "UpstreamRegistry"
	KindUpstreamCredential = "UpstreamCredential"
	KindRegistryMirror     = "RegistryMirror"
	KindRegistryAccess     = "RegistryAccessPolicy"
)

// Readiness values of the objects gauge.
const (
	ReadyTrue    = "true"
	ReadyFalse   = "false"
	ReadyPending = "pending"
)

// CodeError is the code label of an upstream request that got no answer.
const CodeError = "error"

// Metrics is the set of the proxy's collectors.
type Metrics struct {
	registry *prometheus.Registry

	requests         *prometheus.CounterVec
	upstreamDuration *prometheus.HistogramVec
	upstreamRequests *prometheus.CounterVec
	mirrorFallbacks  *prometheus.CounterVec
	tokenReviews     *prometheus.CounterVec
	upstreamTokens   *prometheus.CounterVec
	secretUpdates    *prometheus.CounterVec
	objects          *objectCollector
}

// New returns Metrics on a registry of its own. The Go collector is trimmed to the runtime memory and GC metrics.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registrygate_requests_total", Help: "Requests served, by upstream, action, decision and status class.",
		}, []string{"upstream", "action", "decision", "code"}),
		upstreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "registrygate_upstream_request_duration_seconds",
			Help:    "Time to the upstream's or a mirror's response headers.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 10, 60},
		}, []string{"upstream", "target"}),
		upstreamRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registrygate_upstream_requests_total", Help: "Requests sent to the upstream or a mirror, by status class.",
		}, []string{"upstream", "target", "code"}),
		mirrorFallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registrygate_mirror_fallbacks_total", Help: "Pulls that fell back from a mirror to the upstream.",
		}, []string{"upstream"}),
		tokenReviews: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registrygate_token_reviews_total", Help: "ServiceAccount token reviews, by result and cache use.",
		}, []string{"result", "cache"}),
		upstreamTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registrygate_upstream_token_fetches_total", Help: "Registry token fetches, by upstream, target and result.",
		}, []string{"upstream", "target", "result"}),
		secretUpdates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registrygate_secret_updates_total", Help: "Secret updates the proxy saw, by result.",
		}, []string{"result"}),
		objects: &objectCollector{desc: prometheus.NewDesc(
			"registrygate_objects", "Custom resources in the model snapshot, by kind and readiness.",
			[]string{"kind", "ready"}, nil)},
	}

	m.registry.MustRegister(
		collectors.NewGoCollector(collectors.WithGoCollectorRuntimeMetrics(
			collectors.GoRuntimeMetricsRule{Matcher: runtimeMetricsMatcher})),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.requests, m.upstreamDuration, m.upstreamRequests, m.mirrorFallbacks, m.tokenReviews,
		m.upstreamTokens, m.secretUpdates, m.objects,
	)

	// Every result exists from the start, so the series are there before the first update.
	for _, result := range []string{ResultOK, ResultInvalid, ResultUnavailable} {
		m.secretUpdates.WithLabelValues(result)
	}

	return m
}

// Gatherer returns the registry to serve at /metrics.
func (m *Metrics) Gatherer() prometheus.Gatherer {
	if m == nil {
		return New().registry
	}

	return m.registry
}

// Request counts one served request. upstream is "<namespace>/<name>", or NoUpstream (also used for "").
func (m *Metrics) Request(upstream, action, decision string, status int) {
	if m != nil {
		m.requests.WithLabelValues(upstreamLabel(upstream), action, decision, StatusClass(status)).Inc()
	}
}

// UpstreamRequest records one request to target: its time to response headers and its status, or CodeError when
// status is 0.
func (m *Metrics) UpstreamRequest(upstream, target string, status int, elapsed time.Duration) {
	if m == nil {
		return
	}

	code := CodeError
	if status != 0 {
		code = StatusClass(status)
	}

	m.upstreamDuration.WithLabelValues(upstream, target).Observe(elapsed.Seconds())
	m.upstreamRequests.WithLabelValues(upstream, target, code).Inc()
}

// MirrorFallback counts a pull that went to the upstream because a mirror failed.
func (m *Metrics) MirrorFallback(upstream string) {
	if m != nil {
		m.mirrorFallbacks.WithLabelValues(upstream).Inc()
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
func (m *Metrics) UpstreamTokenFetch(upstream, target, result string) {
	if m != nil {
		m.upstreamTokens.WithLabelValues(upstream, target, result).Inc()
	}
}

// SecretUpdate counts one Secret update with its result: ResultOK, ResultInvalid or ResultUnavailable.
func (m *Metrics) SecretUpdate(result string) {
	if m != nil {
		m.secretUpdates.WithLabelValues(result).Inc()
	}
}

// ObjectCount is the number of objects of one kind and readiness.
type ObjectCount struct {
	Kind  string
	Ready string
	Count int
}

// SetObjectCounts sets where the objects gauge reads its counts from, at scrape time.
func (m *Metrics) SetObjectCounts(counts func() []ObjectCount) {
	if m != nil {
		m.objects.counts.Store(&counts)
	}
}

func upstreamLabel(upstream string) string {
	if upstream == "" {
		return NoUpstream
	}

	return upstream
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
