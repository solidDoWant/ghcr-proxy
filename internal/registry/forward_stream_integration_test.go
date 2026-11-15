//go:build integration

package registry_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/ghcr-proxy/test/fakeghcr"
)

const (
	streamBlockSize  = 1 << 20
	streamBlockCount = 2048 // 2 GiB in total
	// maxForwarderHeap is far below the upload's size: whole-body buffering
	// anywhere in the path would exceed it many times over.
	maxForwarderHeap = 256 << 20
)

// repeatingReader yields block count times without holding more than block.
type repeatingReader struct {
	block     []byte
	remaining int
	offset    int
}

func (r *repeatingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}

	n := copy(p, r.block[r.offset:])
	r.offset += n

	if r.offset == len(r.block) {
		r.offset = 0
		r.remaining--
	}

	return n, nil
}

func TestForwarderStreamsA2GiBUploadInBoundedMemory(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})
	harness := newProxyHarness(t, fake, fake.PAT(), withPAT)

	block := make([]byte, streamBlockSize)
	_, err := rand.Read(block)
	require.NoError(t, err)

	hasher := sha256.New()
	for range streamBlockCount {
		_, _ = hasher.Write(block)
	}

	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))

	start, err := http.NewRequestWithContext(t.Context(), http.MethodPost, harness.server.URL+"/v2/owner/big/blobs/uploads/", nil)
	require.NoError(t, err)

	startResp, err := harness.client().Do(start)
	require.NoError(t, err)
	require.NoError(t, startResp.Body.Close())
	require.Equal(t, http.StatusAccepted, startResp.StatusCode)

	location, err := startResp.Location()
	require.NoError(t, err)

	query := location.Query()
	query.Set("digest", digest)
	location.RawQuery = query.Encode()

	runtime.GC()

	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	var peak atomic.Uint64

	stop := make(chan struct{})
	sampled := make(chan struct{})

	go func() {
		defer close(sampled)

		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			var stats runtime.MemStats
			runtime.ReadMemStats(&stats)

			if stats.HeapInuse > peak.Load() {
				peak.Store(stats.HeapInuse)
			}

			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()

	// No Content-Length: the body is sent chunked, as it streams.
	upload, err := http.NewRequestWithContext(t.Context(), http.MethodPut, location.String(),
		io.NopCloser(&repeatingReader{block: block, remaining: streamBlockCount}))
	require.NoError(t, err)

	uploadResp, err := harness.client().Do(upload)
	close(stop)
	<-sampled

	require.NoError(t, err)
	require.NoError(t, uploadResp.Body.Close())
	require.Equal(t, http.StatusCreated, uploadResp.StatusCode)

	growth := int64(peak.Load()) - int64(baseline.HeapInuse)
	assert.Less(t, growth, int64(maxForwarderHeap), "heap grew by %d bytes during a 2 GiB upload", growth)
}
