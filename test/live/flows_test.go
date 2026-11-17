//go:build live

package live

import (
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mountRecorder notes the status of each cross-repository mount request a
// client sends.
type mountRecorder struct {
	inner http.RoundTripper

	mu       sync.Mutex
	statuses []int
}

func (m *mountRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := m.inner.RoundTrip(req)
	if err == nil && req.Method == http.MethodPost && req.URL.Query().Has("mount") {
		m.mu.Lock()
		m.statuses = append(m.statuses, resp.StatusCode)
		m.mu.Unlock()
	}

	return resp, err
}

// proxyRef names image ref ("<repo>:<tag>" or "<repo>@<digest>") on the proxy.
func (e *liveEnv) proxyRef(t *testing.T, ref string) name.Reference {
	t.Helper()

	parsed, err := name.ParseReference(registryHost + "/" + ref)
	require.NoError(t, err)

	return parsed
}

// callerOptions authenticate to the proxy as the CI ServiceAccount, the way
// the credential helper does.
func (e *liveEnv) callerOptions(transport http.RoundTripper) []remote.Option {
	if transport == nil {
		transport = e.clientTransport
	}

	return []remote.Option{
		remote.WithAuth(&authn.Basic{Username: serviceAccount, Password: e.callerToken}),
		remote.WithTransport(transport),
	}
}

// pushThroughProxy writes a small random image to <repo>:<tag> through the
// proxy and returns it.
func (e *liveEnv) pushThroughProxy(t *testing.T, repo, tag string) v1.Image {
	t.Helper()

	image, err := random.Image(1024, 1)
	require.NoError(t, err)

	e.notePushed(t, repo)
	require.NoError(t, remote.Write(e.proxyRef(t, repo+":"+tag), image, e.callerOptions(nil)...))

	return image
}

func digestOf(t *testing.T, image v1.Image) v1.Hash {
	t.Helper()

	digest, err := image.Digest()
	require.NoError(t, err)

	return digest
}

func TestClientFlowsThroughTheProxy(t *testing.T) {
	env := requireLive(t)
	source := env.repo("flows/source")

	var pushed v1.Image

	t.Run("push", func(t *testing.T) {
		pushed = env.pushThroughProxy(t, source, "v1")
	})

	t.Run("pull", func(t *testing.T) {
		require.NotNil(t, pushed, "push failed")

		pulled, err := remote.Image(env.proxyRef(t, source+":v1"), env.callerOptions(nil)...)
		require.NoError(t, err)
		assert.Equal(t, digestOf(t, pushed), digestOf(t, pulled))

		layers, err := pulled.Layers()
		require.NoError(t, err)

		for _, layer := range layers {
			reader, err := layer.Compressed()
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, reader)
			require.NoError(t, err)
			require.NoError(t, reader.Close())
		}
	})

	t.Run("mount", func(t *testing.T) {
		require.NotNil(t, pushed, "push failed")

		// An image read from the source through the proxy carries its
		// repository, so the client asks the registry to mount its blobs.
		recorder := &mountRecorder{inner: env.clientTransport}
		fromSource, err := remote.Image(env.proxyRef(t, source+":v1"), env.callerOptions(recorder)...)
		require.NoError(t, err)

		mounted := env.repo("flows/mounted")
		env.notePushed(t, mounted)
		require.NoError(t, remote.Write(env.proxyRef(t, mounted+":v1"), fromSource, env.callerOptions(recorder)...))

		recorder.mu.Lock()
		t.Logf("mount requests answered: %v", recorder.statuses)
		recorder.mu.Unlock()

		copied, err := remote.Image(env.proxyRef(t, mounted+":v1"), env.callerOptions(nil)...)
		require.NoError(t, err)
		assert.Equal(t, digestOf(t, pushed), digestOf(t, copied))
	})

	t.Run("anonymous pull of a public image outside the grant", func(t *testing.T) {
		public, err := name.NewDigest(publicImage)
		require.NoError(t, err)

		// The caller's policy and grant don't cover this repository, so the
		// proxy forwards the pull with no credential.
		image, err := remote.Image(env.proxyRef(t, public.Context().RepositoryStr()+"@"+public.DigestStr()), env.callerOptions(nil)...)
		require.NoError(t, err)

		_, err = image.Manifest()
		require.NoError(t, err)
	})
}
