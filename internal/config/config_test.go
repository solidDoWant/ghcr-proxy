package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseReportsEveryError(t *testing.T) {
	_, err := Parse([]byte(`
authentication:
  cacheTTL: 31s
server:
  idleTimeout: soon
`))
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "authentication.cacheTTL")
	assert.Contains(t, msg, "server.idleTimeout")
}

func TestParseRejectsBadValues(t *testing.T) {
	tests := map[string]string{
		"cache ttl too large":       "authentication:\n  cacheTTL: 31s\n",
		"cache ttl too small":       "authentication:\n  cacheTTL: 500ms\n",
		"unknown field":             "bogus: 1\n",
		"bad duration":              "server:\n  readHeaderTimeout: soon\n",
		"non-positive header limit": "server:\n  maxHeaderBytes: 0\n",
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(data))
			require.Error(t, err)
		})
	}
}

func TestParseRefusesSectionsReplacedByCustomResources(t *testing.T) {
	for _, section := range []string{"upstream", "grants", "mirrors", "tls"} {
		t.Run(section, func(t *testing.T) {
			_, err := Parse([]byte(section + ": {}\n"))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "UpstreamRegistry")
		})
	}
}

func TestParseAcceptsCacheTTLBounds(t *testing.T) {
	for _, ttl := range []string{"1s", "30s"} {
		cfg, err := Parse([]byte("authentication:\n  cacheTTL: " + ttl + "\n"))
		require.NoError(t, err)
		assert.Equal(t, ttl, cfg.Authentication.CacheTTL.String()[:len(ttl)])
	}
}

func TestParseAcceptsAnEmptyFile(t *testing.T) {
	cfg, err := Parse(nil)
	require.NoError(t, err)
	assert.Equal(t, ":8443", cfg.Listen.HTTPS)
}

func TestParseDefaultsTheAudienceToRegistrygate(t *testing.T) {
	cfg, err := Parse(nil)
	require.NoError(t, err)
	assert.Equal(t, "registrygate", cfg.Authentication.Audience)
}
