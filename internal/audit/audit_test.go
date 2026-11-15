package audit_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/ghcr-proxy/internal/audit"
)

func TestLogWritesOneJSONLineWithTheDocumentedFields(t *testing.T) {
	var out bytes.Buffer

	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	audit.New(&out).Log(audit.Entry{
		Time: started, ClientIP: "10.0.0.1", Namespace: "ci", SA: "builder", Pod: "p-1", Method: "PUT",
		Path: "/v2/org/app/manifests/latest", Repository: "org/app", From: "org/base", Action: "push",
		Decision: "allowed", Policy: "pol", Status: 201, Upstream: 201, Bytes: 12, DurationMS: 7, Digest: "sha256:abc",
	})

	require.Equal(t, 1, bytes.Count(out.Bytes(), []byte("\n")))

	var line map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &line))

	assert.Equal(t, map[string]any{
		"time": "2026-01-02T03:04:05Z", "clientIP": "10.0.0.1", "namespace": "ci", "serviceAccount": "builder",
		"pod": "p-1", "method": "PUT", "path": "/v2/org/app/manifests/latest", "repository": "org/app",
		"from": "org/base", "action": "push", "decision": "allowed", "policy": "pol", "status": float64(201),
		"upstreamStatus": float64(201), "bytes": float64(12), "durationMs": float64(7), "digest": "sha256:abc",
	}, line)
}

func TestLogOmitsEmptyOptionalFields(t *testing.T) {
	var out bytes.Buffer

	audit.New(&out).Log(audit.Entry{Time: time.Now(), Method: "GET", Path: "/v2/", Action: "none", Decision: "unauthenticated", Status: 401})

	var line map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &line))

	for _, key := range []string{"namespace", "serviceAccount", "pod", "repository", "from", "policy", "upstreamStatus", "digest"} {
		assert.NotContains(t, line, key)
	}
}

func TestNilLoggerDiscards(t *testing.T) {
	var logger *audit.Logger

	assert.NotPanics(t, func() { logger.Log(audit.Entry{}) })
}
