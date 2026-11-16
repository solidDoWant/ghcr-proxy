package metrics

import (
	"regexp"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// runtimeMetricsMatcher keeps the runtime memory and GC metrics only.
var runtimeMetricsMatcher = regexp.MustCompile(`^/(memory/classes/total|gc/(heap|cycles|pauses|gogc|gomemlimit)).*`)

// policyCollector reports the policy counts at scrape time.
type policyCollector struct {
	desc   *prometheus.Desc
	counts atomic.Pointer[func() (ready, notReady int)]
}

func (c *policyCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *policyCollector) Collect(ch chan<- prometheus.Metric) {
	counts := c.counts.Load()
	if counts == nil {
		return
	}

	ready, notReady := (*counts)()
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(ready), "true")
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(notReady), "false")
}
