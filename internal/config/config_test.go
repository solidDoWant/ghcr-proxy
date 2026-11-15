package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writePAT(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))

	return path
}

func parseGrants(t *testing.T, grants string) *Config {
	t.Helper()
	cfg, err := Parse([]byte("upstream:\n  patFile: " + writePAT(t) + "\ngrants:\n" + grants))
	require.NoError(t, err)

	return cfg
}

const ciGrant = `  - namespaces: "ci-(?P<repo>[a-z0-9-]+)"
    repositories: ["soliddowant/${repo}", "soliddowant/${repo}/**"]
    actions: [pull, push, delete]
`

func TestParseReportsEveryError(t *testing.T) {
	_, err := Parse([]byte(`
upstream:
  patFile: /nonexistent/pat
grants:
  - namespaces: "ci-(?P<repo>[a-z]+)"
    repositories: ["x/${nope}"]
    actions: [pull]
  - namespaces: "ci-("
    repositories: ["x"]
    actions: [pull]
`))
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "nope")
	assert.Contains(t, msg, "regular expression")
	assert.Contains(t, msg, "/nonexistent/pat")
}

func TestParseRejectsBadValues(t *testing.T) {
	pat := writePAT(t)

	tests := map[string]string{
		"cache ttl too large": "authentication:\n  cacheTTL: 31s\n",
		"cache ttl too small": "authentication:\n  cacheTTL: 500ms\n",
		"unknown field":       "bogus: 1\n",
		"bad pattern":         "grants:\n  - namespaces: a\n    repositories: ['A/b']\n    actions: [pull]\n",
		"unknown action":      "grants:\n  - namespaces: a\n    repositories: [a]\n    actions: [admin]\n",
		"bad duration":        "upstream:\n  timeout: soon\n",
	}
	for name, extra := range tests {
		t.Run(name, func(t *testing.T) {
			data := extra
			if name != "bad duration" {
				data = "upstream:\n  patFile: " + pat + "\n" + extra
				if name == "unknown field" {
					data = extra
				}
			}

			_, err := Parse([]byte(data))
			require.Error(t, err)
		})
	}
}

func TestParseAcceptsCacheTTLBounds(t *testing.T) {
	pat := writePAT(t)
	for _, ttl := range []string{"1s", "30s"} {
		cfg, err := Parse([]byte("upstream:\n  patFile: " + pat + "\nauthentication:\n  cacheTTL: " + ttl + "\n"))
		require.NoError(t, err)
		assert.Equal(t, ttl, cfg.Authentication.CacheTTL.String()[:len(ttl)])
	}
}

func TestGrantsFor(t *testing.T) {
	cfg := parseGrants(t, ciGrant)
	got := cfg.Grants.For("ci-foo")
	require.Len(t, got, 1)
	assert.Equal(t, []string{"soliddowant/foo", "soliddowant/foo/**"}, got[0].Patterns)
	assert.Equal(t, []Action{ActionPull, ActionPush, ActionDelete}, got[0].Actions)

	assert.Empty(t, cfg.Grants.For("ci-"))
	assert.Empty(t, cfg.Grants.For("other"))
}

func TestGrantsPushImpliesPull(t *testing.T) {
	cfg := parseGrants(t, "  - namespaces: ci\n    repositories: [a]\n    actions: [push]\n")
	got := cfg.Grants.For("ci")
	require.Len(t, got, 1)
	assert.Equal(t, []Action{ActionPull, ActionPush}, got[0].Actions)
}

func TestGrantsUnsafeCapturesDoNotApply(t *testing.T) {
	cfg := parseGrants(t, "  - namespaces: 'ns-(?P<repo>.*)'\n    repositories: ['org/${repo}']\n    actions: [pull]\n")
	assert.Empty(t, cfg.Grants.For("ns-ok~name"))
	assert.Len(t, cfg.Grants.For("ns-ok.name_1-x"), 1)

	for _, ns := range []string{"ns-a/b", "ns-a*", "ns-", "ns-a?b", "ns-a b"} {
		assert.Empty(t, cfg.Grants.For(ns), ns)
	}
}

func TestGrantsNonParticipatingCaptureDoesNotApply(t *testing.T) {
	cfg := parseGrants(t, "  - namespaces: 'ci(?:-(?P<repo>[a-z]+))?'\n    repositories: ['org/${repo}']\n    actions: [pull]\n")
	assert.Empty(t, cfg.Grants.For("ci"))
	assert.Len(t, cfg.Grants.For("ci-x"), 1)
}

func TestGrantsRegexIsAnchored(t *testing.T) {
	cfg := parseGrants(t, "  - namespaces: 'ci'\n    repositories: [a]\n    actions: [pull]\n")
	assert.Empty(t, cfg.Grants.For("xci"))
	assert.Empty(t, cfg.Grants.For("cix"))
}

func TestGrantsLowerCaseCapture(t *testing.T) {
	cfg := parseGrants(t, "  - namespaces: 'ci-(?P<repo>.+)'\n    repositories: ['o/${repo}']\n    actions: [pull]\n")
	got := cfg.Grants.For("ci-Foo")
	require.Len(t, got, 1)
	assert.Equal(t, []string{"o/foo"}, got[0].Patterns)
}

func TestGrantsCombine(t *testing.T) {
	cfg := parseGrants(t, ciGrant+"  - namespaces: 'ci-.*'\n    repositories: [shared/**]\n    actions: [pull]\n")
	got := cfg.Grants.For("ci-foo")
	require.Len(t, got, 2)
	assert.Equal(t, []string{"shared/**"}, got[1].Patterns)
	assert.Equal(t, []Action{ActionPull}, got[1].Actions)
}
