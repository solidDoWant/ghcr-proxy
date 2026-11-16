package registry_test

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/ghcr-proxy/internal/config"
	"github.com/solidDoWant/ghcr-proxy/internal/mirror"
	"github.com/solidDoWant/ghcr-proxy/internal/registry"
	"github.com/solidDoWant/ghcr-proxy/test/fakeghcr"
)

func TestMirrorThatNeedsNoAuthenticationServesPulls(t *testing.T) {
	var authorized atomic.Int32

	open := ggcrregistry.New()
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			authorized.Add(1)
		}

		open.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)

	frontURL, err := url.Parse(front.URL)
	require.NoError(t, err)

	image, err := random.Image(256, 1)
	require.NoError(t, err)

	ref, err := name.ParseReference(frontURL.Host + "/" + mirrorPrefix + "/anon/app:v1")
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, image, remote.WithTransport(front.Client().Transport)))

	caPath := filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: front.Certificate().Raw}), 0o600))

	mirrorConfig := &config.Mirror{
		URL: frontURL, PathPrefix: mirrorPrefix, CAFile: caPath,
		FallbackToUpstream: false, ResponseHeaderTimeout: 10 * time.Second,
	}
	client, err := mirror.NewClient(mirror.ClientConfig{
		Mirror: mirrorConfig, Files: mirror.NewFiles("anonymous", mirrorConfig, nil), TokenTimeout: 10 * time.Second,
	})
	require.NoError(t, err)

	upstreamFake := fakeghcr.New(t, fakeghcr.Options{})
	harness := newProxyHarness(t, upstreamFake, upstreamFake.PAT(), decideByOwner, func(cfg *registry.ForwarderConfig) {
		cfg.AnonymousMirror = client
	})

	for range 2 {
		status, _, body := harness.do(t, http.MethodGet, "/v2/anon/app/manifests/v1")
		assert.Equal(t, http.StatusOK, status, strings.TrimSpace(body))
	}

	assert.Zero(t, authorized.Load(), "no credential may reach an open mirror")
}
