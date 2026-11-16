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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/model"
	"github.com/solidDoWant/registrygate/internal/secrets"
	"github.com/solidDoWant/registrygate/internal/server"
)

type secretMap map[secrets.Key]secrets.Secret

func (m secretMap) Get(key secrets.Key) (secrets.Secret, bool) {
	secret, ok := m[key]
	if !ok {
		return secrets.Secret{State: secrets.NotFound}, true
	}

	return secret, true
}

func servingPair(t *testing.T, host string) (string, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)

	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, &x509.Certificate{SerialNumber: serial}, &key.PublicKey, key)
	require.NoError(t, err)

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

const servingAdminNamespace = "registrygate"

// upstreamSpec describes one UpstreamRegistry and its serving certificate host.
type upstreamSpec struct {
	name, host, certificateHost, version string
}

func buildSnapshot(t *testing.T, specs ...upstreamSpec) *model.Snapshot {
	t.Helper()

	store := secretMap{}
	inputs := model.Inputs{
		AdminNamespaces: []string{servingAdminNamespace},
		Namespaces:      map[string]labels.Set{servingAdminNamespace: {}},
		Secrets:         store,
	}

	for _, spec := range specs {
		certificate, key := servingPair(t, spec.certificateHost)
		store[secrets.Key{Namespace: servingAdminNamespace, Name: spec.name + "-tls"}] = secrets.Secret{
			State: secrets.Present, ResourceVersion: spec.version,
			Data: secrets.Data{"tls.crt": []byte(certificate), "tls.key": []byte(key)},
		}
		inputs.Upstreams = append(inputs.Upstreams, v1alpha1.UpstreamRegistry{
			ObjectMeta: metav1.ObjectMeta{Namespace: servingAdminNamespace, Name: spec.name, Generation: 1},
			Spec: v1alpha1.UpstreamRegistrySpec{
				Hosts: []string{spec.host},
				URL:   "https://" + spec.host + ".example",
				TLS:   v1alpha1.ServingTLS{SecretRef: v1alpha1.LocalRef{Name: spec.name + "-tls"}},
			},
		})
	}

	return model.Build(inputs)
}

// handshake connects to a TLS server using the configuration and returns the leaf certificate the client saw.
func handshake(t *testing.T, config *tls.Config, serverName string) (*x509.Certificate, error) {
	t.Helper()

	client, serverSide := net.Pipe()

	defer func() { _ = client.Close() }()

	server := tls.Server(serverSide, config)
	serverDone := make(chan struct{})

	go func() {
		defer close(serverDone)
		defer func() { _ = serverSide.Close() }()

		_ = server.HandshakeContext(t.Context())
	}()

	conn := tls.Client(client, &tls.Config{ServerName: serverName, InsecureSkipVerify: true}) //nolint:gosec // only the presented certificate is inspected
	err := conn.HandshakeContext(t.Context())

	_ = client.Close()

	<-serverDone

	if err != nil {
		return nil, err
	}

	return conn.ConnectionState().PeerCertificates[0], nil
}

func TestSNICertificatesPickByServerName(t *testing.T) {
	snapshot := buildSnapshot(t,
		upstreamSpec{name: "ghcr", host: "ghcr.io", certificateHost: "ghcr.io"},
		upstreamSpec{name: "quay", host: "quay.io", certificateHost: "quay.io"},
	)
	config := server.SNICertificates(func() *model.Snapshot { return snapshot })

	tests := []struct {
		name       string
		serverName string
		wantHost   string
		wantErr    bool
	}{
		{name: "first upstream", serverName: "ghcr.io", wantHost: "ghcr.io"},
		{name: "second upstream", serverName: "quay.io", wantHost: "quay.io"},
		{name: "upper case name", serverName: "QUAY.io", wantHost: "quay.io"},
		{name: "unclaimed name", serverName: "docker.example", wantErr: true},
		{name: "no server name", serverName: "", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			leaf, err := handshake(t, config, test.serverName)
			if test.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, []string{test.wantHost}, leaf.DNSNames)
		})
	}
}

func TestSNICertificatesFollowTheCurrentSnapshot(t *testing.T) {
	var current atomic.Pointer[model.Snapshot]

	current.Store(buildSnapshot(t, upstreamSpec{name: "ghcr", host: "ghcr.io", certificateHost: "ghcr.io", version: "1"}))

	config := server.SNICertificates(current.Load)

	before, err := handshake(t, config, "ghcr.io")
	require.NoError(t, err)

	current.Store(buildSnapshot(t, upstreamSpec{name: "ghcr", host: "ghcr.io", certificateHost: "ghcr.io", version: "2"}))

	after, err := handshake(t, config, "ghcr.io")
	require.NoError(t, err)
	assert.NotEqual(t, before.Raw, after.Raw, "a renewed certificate applies to new connections")
}

func TestSNICertificatesRefuseHostsInConflict(t *testing.T) {
	snapshot := buildSnapshot(t,
		upstreamSpec{name: "one", host: "ghcr.io", certificateHost: "ghcr.io"},
		upstreamSpec{name: "two", host: "ghcr.io", certificateHost: "ghcr.io"},
	)
	config := server.SNICertificates(func() *model.Snapshot { return snapshot })

	_, err := handshake(t, config, "ghcr.io")
	require.Error(t, err)
}

func TestSNICertificatesFailBeforeTheFirstSnapshot(t *testing.T) {
	config := server.SNICertificates(func() *model.Snapshot { return nil })

	_, err := handshake(t, config, "ghcr.io")
	require.Error(t, err)
}
