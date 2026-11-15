//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/solidDoWant/ghcr-proxy/test/fakeghcr"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func freeAddr(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	return addr
}

func writeSelfSigned(t *testing.T, dir string) (string, string) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ghcr.io"}, DNSNames: []string{"ghcr.io"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))

	return certFile, keyFile
}

type proxyUnderTest struct {
	env    *envtest.Environment
	health string
	stderr *syncBuffer
	done   chan int
	k8s    client.Client
}

func startProxy(t *testing.T, ctx context.Context) *proxyUnderTest {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set")
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	_, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.Stop() })

	user, err := env.ControlPlane.AddUser(envtest.User{Name: "admin", Groups: []string{"system:masters"}}, nil)
	require.NoError(t, err)
	kubeconfig, err := user.KubeConfig()
	require.NoError(t, err)

	k8s, err := client.New(user.Config(), client.Options{})
	require.NoError(t, err)

	fake := fakeghcr.New(t, fakeghcr.Options{})
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kubeconfig"), kubeconfig, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ca.crt"), fake.CAPEM(), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pat"), []byte(fake.PAT()), 0o600))
	certFile, keyFile := writeSelfSigned(t, dir)

	health := freeAddr(t)
	configYAML := fmt.Sprintf(`
upstream:
  url: https://%s
  patFile: %s
  caFile: %s
tls:
  certFile: %s
  keyFile: %s
listen:
  https: %s
  health: %s
server:
  shutdownGracePeriod: 5s
grants:
  - namespaces: "ci-(?P<repo>[a-z0-9-]+)"
    repositories: ["soliddowant/${repo}"]
    actions: [pull, push]
`, fake.RegistryHost(), filepath.Join(dir, "pat"), filepath.Join(dir, "ca.crt"), certFile, keyFile, freeAddr(t), health)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configYAML), 0o600))

	t.Setenv("KUBECONFIG", filepath.Join(dir, "kubeconfig"))
	t.Setenv("POD_NAMESPACE", "default")

	proxy := &proxyUnderTest{env: env, health: health, stderr: &syncBuffer{}, done: make(chan int, 1), k8s: k8s}

	go func() {
		proxy.done <- run(ctx, []string{"--config", filepath.Join(dir, "config.yaml")}, &bytes.Buffer{}, proxy.stderr)
	}()

	require.Eventually(t, func() bool { return proxy.ready() }, 60*time.Second, 200*time.Millisecond, "proxy never became ready: %s", proxy.stderr)

	return proxy
}

func (p *proxyUnderTest) ready() bool {
	resp, err := http.Get("http://" + p.health + "/readyz")
	if err != nil {
		return false
	}

	_ = resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

func (p *proxyUnderTest) leaseHolder(t *testing.T) string {
	var lease coordinationv1.Lease
	if err := p.k8s.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "ghcr-proxy-status"}, &lease); err != nil {
		return "error: " + err.Error()
	}

	if lease.Spec.HolderIdentity == nil {
		return ""
	}

	return *lease.Spec.HolderIdentity
}

// The leader gives the status lease up when it shuts down, so another replica takes over at once. main exits the
// process as soon as run returns, so the lease must be released by then.
func TestLeaderReleasesLeaseWhenRunReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	proxy := startProxy(t, ctx)

	require.Eventually(t, func() bool { h := proxy.leaseHolder(t); return h != "" && len(h) > 5 && h[:5] != "error" },
		30*time.Second, 200*time.Millisecond, "never became leader")

	cancel()

	select {
	case <-proxy.done:
	case <-time.After(60 * time.Second):
		t.Fatal("run did not return")
	}

	require.Empty(t, proxy.leaseHolder(t), "lease still held when run returned")
}
