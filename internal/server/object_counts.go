package server

import (
	"github.com/solidDoWant/registrygate/internal/metrics"
	"github.com/solidDoWant/registrygate/internal/model"
)

// ObjectCounts returns the function the objects gauge reads at scrape time: the snapshot's objects by kind and
// readiness. Without a snapshot it reports nothing.
func ObjectCounts(snapshot func() *model.Snapshot) func() []metrics.ObjectCount {
	return func() []metrics.ObjectCount {
		current := snapshot()
		if current == nil {
			return nil
		}

		tally := map[[2]string]int{}
		add := func(kind string, status model.Status) {
			ready := metrics.ReadyFalse

			switch {
			case status.Pending:
				ready = metrics.ReadyPending
			case status.Ready:
				ready = metrics.ReadyTrue
			}

			tally[[2]string{kind, ready}]++
		}

		for _, item := range current.Upstreams {
			add(metrics.KindUpstreamRegistry, item.Status)
		}

		for _, item := range current.Credentials {
			add(metrics.KindUpstreamCredential, item.Status)
		}

		for _, item := range current.Mirrors {
			add(metrics.KindRegistryMirror, item.Status)
		}

		for _, item := range current.Policies {
			add(metrics.KindRegistryAccess, item.Status)
		}

		counts := make([]metrics.ObjectCount, 0, len(tally))
		for key, count := range tally {
			counts = append(counts, metrics.ObjectCount{Kind: key[0], Ready: key[1], Count: count})
		}

		return counts
	}
}
