//go:build e2e

package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// auditEntry is the part of a proxy audit line that these tests read.
type auditEntry struct {
	Namespace  string `json:"namespace"`
	Method     string `json:"method"`
	Repository string `json:"repository"`
	Decision   string `json:"decision"`
	Mirror     string `json:"mirror"`
	Status     int    `json:"status"`
}

// proxyAuditEntries reads the audit lines of every proxy pod. The proxy logs a
// line when it finishes a request, so a pull that has returned is in the log.
func proxyAuditEntries(t *testing.T) []auditEntry {
	t.Helper()

	pods, err := cluster.kubectl(t.Context(), "", "-n", proxyNamespace, "get", "pods", "-o", "jsonpath={.items[*].metadata.name}")
	require.NoError(t, err)

	var entries []auditEntry

	for _, pod := range strings.Fields(pods) {
		logs, err := cluster.kubectl(t.Context(), "", "-n", proxyNamespace, "logs", pod)
		require.NoError(t, err, pod)

		for _, line := range strings.Split(logs, "\n") {
			var entry auditEntry
			if json.Unmarshal([]byte(line), &entry) == nil && entry.Decision != "" {
				entries = append(entries, entry)
			}
		}
	}

	return entries
}

// servedByMirror reports whether the proxy answered a pull of repository from
// namespace with a success it got from one of the namespace's mirrors.
func servedByMirror(entries []auditEntry, namespace, repository string) bool {
	for _, entry := range entries {
		if entry.Namespace == namespace && entry.Repository == repository && entry.Decision == "mirror" &&
			strings.HasPrefix(entry.Mirror, namespace+"/") && entry.Status >= 200 && entry.Status < 300 {
			return true
		}
	}

	return false
}
