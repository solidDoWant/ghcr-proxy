package server_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeleteByDigestWithTheDeleteActionRemovesTheManifest(t *testing.T) {
	proxy := newHarness(t, harnessOptions{})

	image, err := random.Image(256, 1)
	require.NoError(t, err)
	require.NoError(t, proxy.fake.Push("soliddowant/foo/disposable:v1", image))

	digest, err := image.Digest()
	require.NoError(t, err)

	ref, err := name.NewDigest(strings.TrimPrefix(proxy.server.URL, "https://") + "/soliddowant/foo/disposable@" + digest.String())
	require.NoError(t, err)

	// remote.Delete is what crane delete runs.
	options := []remote.Option{
		remote.WithAuth(&authn.Basic{Username: "registrygate", Password: builderToken}),
		remote.WithTransport(proxy.server.Client().Transport),
	}
	require.NoError(t, remote.Delete(ref, options...))

	upstreamRef, err := name.NewDigest(proxy.fake.RegistryHost() + "/soliddowant/foo/disposable@" + digest.String())
	require.NoError(t, err)

	_, err = remote.Head(upstreamRef, proxy.fake.RemoteOptions(true)...)

	var transportErr *transport.Error
	require.ErrorAs(t, err, &transportErr, "the manifest is gone from the upstream")
	assert.Equal(t, http.StatusNotFound, transportErr.StatusCode)
}

func TestDeleteWithoutTheDeleteActionIs403AndSendsNothingUpstream(t *testing.T) {
	proxy := newHarness(t, harnessOptions{})

	tests := []struct {
		name   string
		target string
	}{
		{name: "manifest by digest", target: "/v2/soliddowant/foo/app/manifests/sha256:0000000000000000000000000000000000000000000000000000000000000000"},
		{name: "manifest by tag", target: "/v2/soliddowant/foo/app/manifests/v1"},
		{name: "blob", target: "/v2/soliddowant/foo/app/blobs/sha256:0000000000000000000000000000000000000000000000000000000000000000"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy.fake.ResetLog()

			got := proxy.do(t, http.MethodDelete, test.target, basic(builderToken))

			assert.Equal(t, http.StatusForbidden, got.status)
			assert.Equal(t, "DENIED", got.code)
			assert.Contains(t, got.body, "delete")
			assert.Empty(t, proxy.fake.Log(), "neither the registry nor the REST API sees the request")
		})
	}
}
