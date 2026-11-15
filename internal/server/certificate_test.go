package server_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/ghcr-proxy/internal/server"
)

// writeKeyPair writes a fresh self-signed pair for name into dir as tls.crt and tls.key and returns the certificate.
// The files are replaced by renaming, so a reader never sees a half-written file.
func writeKeyPair(t *testing.T, dir, name string) *x509.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	replace(t, filepath.Join(dir, "tls.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	replace(t, filepath.Join(dir, "tls.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))

	certificate, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return certificate
}

func replace(t *testing.T, path string, data []byte) {
	t.Helper()

	temporary := path + ".new"
	require.NoError(t, os.WriteFile(temporary, data, 0o600))
	require.NoError(t, os.Rename(temporary, path))
}

// servedSerial connects to addr and returns the serial number of the certificate the server presents.
func servedSerial(t *testing.T, addr string) *big.Int {
	t.Helper()

	dialer := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}} //nolint:gosec // the test reads the served certificate, it doesn't trust it
	conn, err := dialer.DialContext(t.Context(), "tcp", addr)
	require.NoError(t, err)

	defer func() { _ = conn.Close() }()

	return conn.(*tls.Conn).ConnectionState().PeerCertificates[0].SerialNumber
}

func TestReplacedCertificateIsServedToNewConnections(t *testing.T) {
	dir := t.TempDir()
	first := writeKeyPair(t, dir, "first")

	certificate := server.NewCertificate(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), nil)
	require.True(t, certificate.Loaded())

	watched := make(chan error, 1)

	go func() { watched <- certificate.Watch(t.Context()) }()

	listener, err := tls.Listen("tcp", "127.0.0.1:0", certificate.TLSConfig())
	require.NoError(t, err)

	httpServer := &http.Server{
		Handler:           http.NotFoundHandler(),
		ReadHeaderTimeout: time.Second,
	}

	go func() { _ = httpServer.Serve(listener) }()

	t.Cleanup(func() { _ = httpServer.Close() })

	addr := listener.Addr().String()
	require.Equal(t, first.SerialNumber, servedSerial(t, addr))

	second := writeKeyPair(t, dir, "second")

	require.Eventually(t, func() bool {
		return servedSerial(t, addr).Cmp(second.SerialNumber) == 0
	}, 10*time.Second, 50*time.Millisecond, "new connections get the replaced certificate")

	replace(t, filepath.Join(dir, "tls.crt"), []byte("not a certificate"))

	// A failed reload keeps the last good pair.
	certificate.Reload()
	assert.Equal(t, second.SerialNumber, servedSerial(t, addr))

	select {
	case err := <-watched:
		require.NoError(t, err, "the watch stopped early")
	default:
	}
}

func TestMissingCertificateIsNotLoaded(t *testing.T) {
	dir := t.TempDir()

	certificate := server.NewCertificate(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), nil)
	assert.False(t, certificate.Loaded())

	writeKeyPair(t, dir, "late")
	certificate.Reload()
	assert.True(t, certificate.Loaded())
}
