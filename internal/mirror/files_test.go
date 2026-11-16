package mirror_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"go/parser"
	"go/token"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/internal/config"
	"github.com/solidDoWant/registrygate/internal/mirror"
)

func caPEM(t *testing.T) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mirror test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func writeFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, contents, 0o600))
}

func TestMirrorCredentialsFileLoading(t *testing.T) {
	tests := []struct {
		name            string
		initial         *string
		replacement     *string
		wantCredentials mirror.Credentials
		wantGeneration  uint64
		wantAvailable   bool
	}{
		{name: "reads user and password", initial: ptr("robot:s3cret\n"), wantCredentials: mirror.Credentials{Username: "robot", Password: "s3cret"}, wantGeneration: 1, wantAvailable: true},
		{name: "password may contain a colon", initial: ptr("robot:s3:cret"), wantCredentials: mirror.Credentials{Username: "robot", Password: "s3:cret"}, wantGeneration: 1, wantAvailable: true},
		{name: "missing file makes the mirror unavailable", wantAvailable: false},
		{name: "empty file makes the mirror unavailable", initial: ptr("\n"), wantAvailable: false},
		{name: "file without a colon makes the mirror unavailable", initial: ptr("s3cret"), wantAvailable: false},
		{name: "empty user makes the mirror unavailable", initial: ptr(":s3cret"), wantAvailable: false},
		{name: "empty password makes the mirror unavailable", initial: ptr("robot:"), wantAvailable: false},
		{name: "changed file starts a new generation", initial: ptr("robot:s3cret"), replacement: ptr("robot:n3w"), wantCredentials: mirror.Credentials{Username: "robot", Password: "n3w"}, wantGeneration: 2, wantAvailable: true},
		{name: "unchanged file keeps the generation", initial: ptr("robot:s3cret"), replacement: ptr("robot:s3cret\n"), wantCredentials: mirror.Credentials{Username: "robot", Password: "s3cret"}, wantGeneration: 1, wantAvailable: true},
		{name: "invalid reload keeps the previous credentials", initial: ptr("robot:s3cret"), replacement: ptr("garbage"), wantCredentials: mirror.Credentials{Username: "robot", Password: "s3cret"}, wantGeneration: 1, wantAvailable: true},
		{name: "credentials appearing later are loaded", replacement: ptr("robot:l4te"), wantCredentials: mirror.Credentials{Username: "robot", Password: "l4te"}, wantGeneration: 1, wantAvailable: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials")
			if test.initial != nil {
				writeFile(t, path, []byte(*test.initial))
			}

			var logs bytes.Buffer

			files := mirror.NewFiles("policy", &config.Mirror{CredentialsFile: path}, slog.New(slog.NewTextHandler(&logs, nil)))

			if test.replacement != nil {
				writeFile(t, path, []byte(*test.replacement))
				files.Reload()
			}

			state, available := files.Get()
			assert.Equal(t, test.wantAvailable, available)
			assert.Equal(t, test.wantAvailable, files.Available())

			if test.wantAvailable {
				require.NotNil(t, state.Credentials)
				assert.Equal(t, test.wantCredentials, *state.Credentials)
				assert.Equal(t, test.wantGeneration, state.Generation)
			} else {
				assert.Nil(t, state.Credentials)
			}

			assert.NotContains(t, logs.String(), "cret")
			assert.NotContains(t, logs.String(), "n3w")
		})
	}
}

func TestMirrorCAFileLoading(t *testing.T) {
	ca := caPEM(t)

	tests := []struct {
		name          string
		initial       []byte
		replacement   []byte
		wantAvailable bool
	}{
		{name: "reads a PEM certificate", initial: ca, wantAvailable: true},
		{name: "missing file makes the mirror unavailable", wantAvailable: false},
		{name: "file without a certificate makes the mirror unavailable", initial: []byte("not a certificate"), wantAvailable: false},
		{name: "invalid reload keeps the previous CA", initial: ca, replacement: []byte("not a certificate"), wantAvailable: true},
		{name: "CA appearing later is loaded", replacement: ca, wantAvailable: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ca.crt")
			if test.initial != nil {
				writeFile(t, path, test.initial)
			}

			files := mirror.NewFiles("anonymous", &config.Mirror{CAFile: path}, nil)

			if test.replacement != nil {
				writeFile(t, path, test.replacement)
				files.Reload()
			}

			state, available := files.Get()
			assert.Equal(t, test.wantAvailable, available)

			if test.wantAvailable {
				require.NotNil(t, state.RootCAs)

				block, _ := pem.Decode(ca)
				certificate, err := x509.ParseCertificate(block.Bytes)
				require.NoError(t, err)
				_, err = certificate.Verify(x509.VerifyOptions{Roots: state.RootCAs})
				assert.NoError(t, err, "the configured CA is trusted")
			}
		})
	}
}

func TestMirrorWithoutFilesIsAvailableAnonymously(t *testing.T) {
	files := mirror.NewFiles("anonymous", &config.Mirror{}, nil)

	state, available := files.Get()
	assert.True(t, available)
	assert.Nil(t, state.Credentials)
	assert.Nil(t, state.RootCAs)
}

func TestMirrorUnavailableWhenOneOfItsFilesIsMissing(t *testing.T) {
	dir := t.TempDir()
	credentials := filepath.Join(dir, "credentials")
	writeFile(t, credentials, []byte("robot:s3cret"))

	files := mirror.NewFiles("policy", &config.Mirror{CredentialsFile: credentials, CAFile: filepath.Join(dir, "missing.crt")}, nil)
	assert.False(t, files.Available())
}

func TestMirrorFilesWatchReloadsOnChange(t *testing.T) {
	dir := t.TempDir()
	credentials := filepath.Join(dir, "credentials")
	ca := filepath.Join(t.TempDir(), "ca.crt")

	files := mirror.NewFiles("policy", &config.Mirror{CredentialsFile: credentials, CAFile: ca}, nil)
	require.False(t, files.Available())

	watchDone := make(chan error, 1)

	ctx := t.Context()
	go func() { watchDone <- files.Watch(ctx) }()

	writeFile(t, ca, caPEM(t))

	for index := range 3 {
		password := "pw" + strconv.Itoa(index)
		writeFile(t, credentials, []byte("robot:"+password))
		require.Eventually(t, func() bool {
			state, ok := files.Get()
			return ok && state.Credentials.Password == password
		}, 10*time.Second, 10*time.Millisecond)
	}
}

func TestMirrorCredentialsNeverFormatTheirValues(t *testing.T) {
	credentials := mirror.Credentials{Username: "robot", Password: "s3cret"}

	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("x", "credentials", credentials)

	for _, formatted := range []string{
		fmt.Sprint(credentials), fmt.Sprintf("%v %+v %#v %s", credentials, credentials, credentials, credentials), logs.String(),
	} {
		assert.NotContains(t, formatted, "s3cret")
		assert.NotContains(t, formatted, "robot")
	}
}

// The mirror package must not be able to reach the PAT: it may not import the
// package that reads it.
func TestMirrorPackageDoesNotImportThePATReader(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	require.NoError(t, err)

	for _, source := range sources {
		file, err := parser.ParseFile(token.NewFileSet(), source, nil, parser.ImportsOnly)
		require.NoError(t, err)

		for _, spec := range file.Imports {
			assert.NotEqual(t, `"github.com/solidDoWant/registrygate/internal/upstream"`, spec.Path.Value, source)
		}
	}
}

func ptr(value string) *string { return &value }
