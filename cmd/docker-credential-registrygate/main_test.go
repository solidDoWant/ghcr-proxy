package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/internal/credhelper"
)

func TestTokenFileDefaultsAndOverrides(t *testing.T) {
	t.Setenv(credhelper.TokenFileEnv, "")
	assert.Equal(t, credhelper.DefaultTokenFile, tokenFile())

	t.Setenv(credhelper.TokenFileEnv, "/tmp/other")
	assert.Equal(t, "/tmp/other", tokenFile())
}

func TestConfigureWritesCredHelpersFromEnvironment(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docker")
	t.Setenv("DOCKER_CONFIG", dir)
	t.Setenv(credhelper.HostsEnv, "GHCR.io, https://quay.io/v2/,quay.io:443")

	require.NoError(t, configure())

	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"credHelpers":{"ghcr.io":"registrygate","quay.io":"registrygate"}}`, string(raw))
}

func TestConfigureFailsWithoutHosts(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv(credhelper.HostsEnv, "")

	require.Error(t, configure())
}
