package metrics

import (
	"regexp"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// runtimeMetricsMatcher keeps the runtime memory and GC metrics only.
var runtimeMetricsMatcher = regexp.MustCompile(`^/(memory/classes/total|gc/(heap|cycles|pauses|gogc|gomemlimit)).*`)

// objectCollector reports the object counts at scrape time.
type objectCollector struct {
	desc   *prometheus.Desc
	counts atomic.Pointer[func() []ObjectCount]
}

func (c *objectCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *objectCollector) Collect(ch chan<- prometheus.Metric) {
	counts := c.counts.Load()
	if counts == nil {
		return
	}

	for _, count := range (*counts)() {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(count.Count), count.Kind, count.Ready)
	}
}
