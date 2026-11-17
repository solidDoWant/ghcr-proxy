package credhelper_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker-credential-helpers/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/internal/credhelper"
)

func writeToken(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func get(t *testing.T, helper credentials.Helper, server string) (string, error) {
	t.Helper()

	var out bytes.Buffer

	err := credentials.HandleCommand(helper, credentials.ActionGet, strings.NewReader(server+"\n"), &out)

	return out.String(), err
}

func TestGetAnswersOnlyForGHCR(t *testing.T) {
	helper := credhelper.Helper{Hosts: []string{"ghcr.io"}, TokenFile: writeToken(t, "tok-1\n")}

	tests := []struct {
		server   string
		notFound bool
	}{
		{server: "ghcr.io"},
		{server: "https://ghcr.io"},
		{server: "ghcr.io/owner/image"},
		{server: "https://ghcr.io/v2/"},
		{server: "docker.io", notFound: true},
		{server: "https://index.docker.io/v1/", notFound: true},
		{server: "ghcr.io.evil.example", notFound: true},
		{server: "evil.example/ghcr.io", notFound: true},
		{server: "http://ghcr.io", notFound: true},
	}

	for _, test := range tests {
		t.Run(test.server, func(t *testing.T) {
			out, err := get(t, helper, test.server)
			if test.notFound {
				require.Error(t, err)
				assert.True(t, credentials.IsErrCredentialsNotFound(err))
				assert.Empty(t, out)

				return
			}

			require.NoError(t, err)
			assert.JSONEq(t, `{"ServerURL":"`+test.server+`","Username":"registrygate","Secret":"tok-1"}`, out)
		})
	}
}

func TestGetReadsReplacedTokenFile(t *testing.T) {
	path := writeToken(t, "old-token")
	helper := credhelper.Helper{Hosts: []string{"ghcr.io"}, TokenFile: path}

	out, err := get(t, helper, "ghcr.io")
	require.NoError(t, err)
	assert.Contains(t, out, `"Secret":"old-token"`)

	replacement := path + ".new"
	require.NoError(t, os.WriteFile(replacement, []byte("new-token\n"), 0o600))
	require.NoError(t, os.Rename(replacement, path))

	out, err = get(t, helper, "ghcr.io")
	require.NoError(t, err)
	assert.Contains(t, out, `"Secret":"new-token"`)
	assert.NotContains(t, out, "old-token")
}

func TestGetFailsOnMissingOrEmptyTokenFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")

	tests := map[string]string{
		"missing":    missing,
		"empty":      writeToken(t, ""),
		"whitespace": writeToken(t, " \n\t"),
	}

	for name, path := range tests {
		t.Run(name, func(t *testing.T) {
			out, err := get(t, credhelper.Helper{Hosts: []string{"ghcr.io"}, TokenFile: path}, "ghcr.io")
			require.EqualError(t, err, "registrygate credential helper: token file "+path+" is missing or empty")
			assert.Empty(t, out)
		})
	}
}

func TestStoreEraseAndListDoNothing(t *testing.T) {
	helper := credhelper.Helper{Hosts: []string{"ghcr.io"}, TokenFile: writeToken(t, "tok")}

	var out bytes.Buffer
	require.NoError(t, credentials.HandleCommand(helper, credentials.ActionStore,
		strings.NewReader(`{"ServerURL":"ghcr.io","Username":"u","Secret":"s"}`), &out))
	assert.Empty(t, out.String())

	require.NoError(t, credentials.HandleCommand(helper, credentials.ActionErase, strings.NewReader("ghcr.io"), &out))
	assert.Empty(t, out.String())

	require.NoError(t, credentials.HandleCommand(helper, credentials.ActionList, strings.NewReader(""), &out))
	assert.JSONEq(t, `{}`, out.String())
}

func TestGetAnswersOnlyForListedHosts(t *testing.T) {
	helper := credhelper.Helper{TokenFile: writeToken(t, "tok"), Hosts: credhelper.ParseHosts("ghcr.io,quay.io")}

	tests := []struct {
		server   string
		notFound bool
	}{
		{server: "ghcr.io"},
		{server: "https://quay.io/v2/"},
		{server: "quay.io:443"},
		{server: "QUAY.io"},
		{server: "docker.io", notFound: true},
		{server: "quay.io:5000", notFound: true},
	}

	for _, test := range tests {
		t.Run(test.server, func(t *testing.T) {
			_, err := get(t, helper, test.server)
			if test.notFound {
				require.Error(t, err)
				assert.True(t, credentials.IsErrCredentialsNotFound(err))

				return
			}

			require.NoError(t, err)
		})
	}
}

func TestGetAnswersNothingWithoutHosts(t *testing.T) {
	for _, list := range []string{"", " , "} {
		helper := credhelper.Helper{TokenFile: writeToken(t, "tok"), Hosts: credhelper.ParseHosts(list)}
		_, err := get(t, helper, "ghcr.io")
		assert.True(t, credentials.IsErrCredentialsNotFound(err))
	}
}

func TestConfigureMergesIntoExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path,
		[]byte(`{"auths":{"x.io":{"auth":"abc"}},"credHelpers":{"other.io":"foo","ghcr.io":"old"}}`), 0o644))

	require.NoError(t, credhelper.ConfigureFile(path, []string{"ghcr.io", "quay.io"}))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"auths":{"x.io":{"auth":"abc"}},`+
		`"credHelpers":{"other.io":"foo","ghcr.io":"registrygate","quay.io":"registrygate"}}`, string(raw))
}

func TestConfigureWritesTheDockerHubIndexURLKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	require.NoError(t, credhelper.ConfigureFile(path, []string{"registry-1.docker.io", "docker.io", "index.docker.io"}))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"credHelpers":{"registry-1.docker.io":"registrygate","docker.io":"registrygate","index.docker.io":"registrygate",`+
		`"https://index.docker.io/v1/":"registrygate"}}`, string(raw))
}

func TestConfigureCreatesFileAndDirectoryPrivately(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", ".docker")
	path := filepath.Join(dir, "config.json")

	require.NoError(t, credhelper.ConfigureFile(path, []string{"ghcr.io"}))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	info, err = os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestConfigureLeavesInvalidFilesUntouched(t *testing.T) {
	tests := map[string]string{
		"array":             `[1]`,
		"null":              `null`,
		"text":              `nope`,
		"credHelpers array": `{"credHelpers":["a"]}`,
		"credHelpers null":  `{"credHelpers":null}`,
	}

	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

			require.Error(t, credhelper.ConfigureFile(path, []string{"ghcr.io"}))

			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, content, string(raw))
		})
	}
}

func TestConfigureRejectsNoHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	require.Error(t, credhelper.ConfigureFile(path, nil))
	assert.NoFileExists(t, path)
}
