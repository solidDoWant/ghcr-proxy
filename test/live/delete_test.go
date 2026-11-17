//go:build live

package live

import (
	"net/http"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeleteByDigestThroughTheProxyReturnsGHCRsUnsupported(t *testing.T) {
	env := requireLive(t)
	repo := env.repo("delete/by-digest")

	target := env.pushThroughProxy(t, repo, "target")
	ref := env.proxyRef(t, repo+"@"+digestOf(t, target).String())

	err := remote.Delete(ref, env.callerOptions(nil)...)
	require.Error(t, err)

	var registryErr *transport.Error
	require.ErrorAs(t, err, &registryErr)
	assert.Equal(t, http.StatusMethodNotAllowed, registryErr.StatusCode)
	require.NotEmpty(t, registryErr.Errors)
	assert.Equal(t, transport.UnsupportedErrorCode, registryErr.Errors[0].Code)

	_, err = remote.Head(ref, env.callerOptions(nil)...)
	require.NoError(t, err, "the manifest must still be served after the refused delete")
}
