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
	helper := credhelper.Helper{TokenFile: writeToken(t, "tok-1\n")}

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
	helper := credhelper.Helper{TokenFile: path}

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
			out, err := get(t, credhelper.Helper{TokenFile: path}, "ghcr.io")
			require.EqualError(t, err, "registrygate credential helper: token file "+path+" is missing or empty")
			assert.Empty(t, out)
		})
	}
}

func TestStoreEraseAndListDoNothing(t *testing.T) {
	helper := credhelper.Helper{TokenFile: writeToken(t, "tok")}

	var out bytes.Buffer
	require.NoError(t, credentials.HandleCommand(helper, credentials.ActionStore,
		strings.NewReader(`{"ServerURL":"ghcr.io","Username":"u","Secret":"s"}`), &out))
	assert.Empty(t, out.String())

	require.NoError(t, credentials.HandleCommand(helper, credentials.ActionErase, strings.NewReader("ghcr.io"), &out))
	assert.Empty(t, out.String())

	require.NoError(t, credentials.HandleCommand(helper, credentials.ActionList, strings.NewReader(""), &out))
	assert.JSONEq(t, `{}`, out.String())
}
