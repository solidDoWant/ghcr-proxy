//go:build integration

package server_test

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

	"github.com/solidDoWant/registrygate/test/fakeregistry"
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
	proxy := newHarness(t, harnessOptions{fake: fakeregistry.Options{BlobDir: t.TempDir()}})

	block := make([]byte, streamBlockSize)
	_, err := rand.Read(block)
	require.NoError(t, err)

	hasher := sha256.New()
	for range streamBlockCount {
		_, _ = hasher.Write(block)
	}

	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))

	location := proxy.url() + proxy.startUpload(t, "soliddowant/foo/app") + "?digest=" + digest

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
	upload, err := http.NewRequestWithContext(t.Context(), http.MethodPut, location,
		io.NopCloser(&repeatingReader{block: block, remaining: streamBlockCount}))
	require.NoError(t, err)
	upload.Header.Set("Authorization", basic(builderToken))

	uploadResp, err := proxy.client().Do(upload)

	close(stop)
	<-sampled

	require.NoError(t, err)
	require.NoError(t, uploadResp.Body.Close())
	require.Equal(t, http.StatusCreated, uploadResp.StatusCode)

	growth := int64(peak.Load()) - int64(baseline.HeapInuse)
	assert.Less(t, growth, int64(maxForwarderHeap), "heap grew by %d bytes during a 2 GiB upload", growth)
}
