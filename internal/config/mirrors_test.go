package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseMirrorsAppliesDefaults(t *testing.T) {
	pat := writePAT(t)

	cfg, err := Parse([]byte("upstream:\n  patFile: " + pat + `
mirrors:
  policy:
    url: https://harbor.example.com
    pathPrefix: ghcr
    credentialsFile: /var/run/registrygate/mirror-policy/credentials
    caFile: /var/run/registrygate/mirror-policy-ca/ca.crt
  anonymous:
    url: http://harbor.example.com:8080/
    pathPrefix: proxy/ghcr
    fallbackToUpstream: false
    responseHeaderTimeout: 2s
`))
	require.NoError(t, err)

	policy := cfg.Mirrors.Policy
	require.NotNil(t, policy)
	assert.Equal(t, "https://harbor.example.com", policy.URL.String())
	assert.Equal(t, "ghcr", policy.PathPrefix)
	assert.Equal(t, "/var/run/registrygate/mirror-policy/credentials", policy.CredentialsFile)
	assert.Equal(t, "/var/run/registrygate/mirror-policy-ca/ca.crt", policy.CAFile)
	assert.True(t, policy.FallbackToUpstream)
	assert.Equal(t, 10*time.Second, policy.ResponseHeaderTimeout)

	anonymous := cfg.Mirrors.Anonymous
	require.NotNil(t, anonymous)
	assert.Equal(t, "http://harbor.example.com:8080", anonymous.URL.String())
	assert.Equal(t, "proxy/ghcr", anonymous.PathPrefix)
	assert.Empty(t, anonymous.CredentialsFile)
	assert.False(t, anonymous.FallbackToUpstream)
	assert.Equal(t, 2*time.Second, anonymous.ResponseHeaderTimeout)
}

func TestParseWithoutMirrorsConfiguresNone(t *testing.T) {
	cfg, err := Parse([]byte("upstream:\n  patFile: " + writePAT(t) + "\n"))
	require.NoError(t, err)
	assert.Nil(t, cfg.Mirrors.Policy)
	assert.Nil(t, cfg.Mirrors.Anonymous)
}

func TestParseRejectsInvalidMirrorValues(t *testing.T) {
	tests := map[string]struct {
		mirror  string
		message string
	}{
		"missing url":           {mirror: "pathPrefix: ghcr", message: "mirrors.anonymous.url: is required"},
		"not a url":             {mirror: "url: harbor\n    pathPrefix: ghcr", message: "mirrors.anonymous.url"},
		"ftp scheme":            {mirror: "url: ftp://harbor\n    pathPrefix: ghcr", message: "mirrors.anonymous.url"},
		"url with path":         {mirror: "url: https://harbor/ghcr\n    pathPrefix: ghcr", message: "mirrors.anonymous.url"},
		"url with user":         {mirror: "url: https://u:p@harbor\n    pathPrefix: ghcr", message: "mirrors.anonymous.url"},
		"url with query":        {mirror: "url: https://harbor?x=1\n    pathPrefix: ghcr", message: "mirrors.anonymous.url"},
		"missing path prefix":   {mirror: "url: https://harbor", message: "mirrors.anonymous.pathPrefix: is required"},
		"uppercase path prefix": {mirror: "url: https://harbor\n    pathPrefix: GHCR", message: "mirrors.anonymous.pathPrefix"},
		"slash path prefix":     {mirror: "url: https://harbor\n    pathPrefix: /ghcr/", message: "mirrors.anonymous.pathPrefix"},
		"bad timeout":           {mirror: "url: https://harbor\n    pathPrefix: ghcr\n    responseHeaderTimeout: soon", message: "mirrors.anonymous.responseHeaderTimeout"},
		"zero timeout":          {mirror: "url: https://harbor\n    pathPrefix: ghcr\n    responseHeaderTimeout: 0s", message: "mirrors.anonymous.responseHeaderTimeout"},
		"unknown field":         {mirror: "url: https://harbor\n    pathPrefix: ghcr\n    password: x", message: "password"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte("upstream:\n  patFile: " + writePAT(t) + "\nmirrors:\n  anonymous:\n    " + test.mirror + "\n"))
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.message)
		})
	}
}

func TestParseRejectsMirrorCredentialsFileThatIsThePATFile(t *testing.T) {
	pat := writePAT(t)
	dir := t.TempDir()

	symlink := filepath.Join(dir, "credentials")
	require.NoError(t, os.Symlink(pat, symlink))

	hardlink := filepath.Join(dir, "hardlink")
	require.NoError(t, os.Link(pat, hardlink))

	tests := map[string]string{
		"same path":       pat,
		"unclean path":    filepath.Dir(pat) + "/./" + filepath.Base(pat),
		"symlink to it":   symlink,
		"hard link to it": hardlink,
	}
	for name, credentials := range tests {
		t.Run(name, func(t *testing.T) {
			for _, mirror := range []string{"policy", "anonymous"} {
				_, err := Parse([]byte("upstream:\n  patFile: " + pat + "\nmirrors:\n  " + mirror +
					":\n    url: https://harbor\n    pathPrefix: ghcr\n    credentialsFile: " + credentials + "\n"))
				require.Error(t, err)
				assert.Contains(t, err.Error(), "mirrors."+mirror+".credentialsFile")
			}
		})
	}
}

func TestParseAcceptsMirrorCredentialsFileBesideThePATFile(t *testing.T) {
	pat := writePAT(t)
	credentials := filepath.Join(filepath.Dir(pat), "credentials")
	require.NoError(t, os.WriteFile(credentials, []byte("x"), 0o600))

	_, err := Parse([]byte("upstream:\n  patFile: " + pat + "\nmirrors:\n  policy:\n    url: https://harbor\n    pathPrefix: ghcr\n    credentialsFile: " + credentials + "\n"))
	require.NoError(t, err)
}
