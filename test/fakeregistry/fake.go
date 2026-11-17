// Package fakeregistry is an in-process test double for an OCI registry: bearer,
// basic or no authentication, GHCR's singular or the standard plural upload
// session paths, blob downloads redirected to a separate storage server, and
// delete that is supported or answered with 405 UNSUPPORTED, as ghcr.io does.
// It wraps go-containerregistry's registry package for storage and is not a
// clone of any registry.
package fakeregistry

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Auth is how the fake authenticates clients.
type Auth string

// UploadPath is the form of upload session paths the fake hands out.
type UploadPath string

// Delete says whether the fake lets clients delete.
type Delete string

const (
	// Bearer is a token endpoint that trades the PAT, or nothing for public
	// repositories, for a scoped bearer token. It is the default.
	Bearer Auth = "bearer"
	// Basic takes the PAT as the Basic password on every request.
	Basic Auth = "basic"
	// None accepts every request without credentials.
	None Auth = "none"

	// Uploads is the standard plural form (blobs/uploads/<session>). It is the default.
	Uploads UploadPath = "uploads"
	// Upload is GHCR's singular form (blobs/upload/<session>).
	Upload UploadPath = "upload"

	// Supported lets clients delete. It is the default.
	Supported Delete = "supported"
	// Unsupported answers every DELETE with 405 and the UNSUPPORTED error code.
	Unsupported Delete = "unsupported"
)

// Options configure a Fake. The zero value works for in-process tests: bearer
// authentication, plural upload paths and delete supported. GHCR's own
// behaviour is Bearer, Upload and Unsupported.
type Options struct {
	// Auth defaults to Bearer.
	Auth Auth
	// UploadPath defaults to Uploads.
	UploadPath UploadPath
	// Delete defaults to Supported.
	Delete Delete
	// PAT is the personal access token the fake accepts. Random when empty.
	PAT string
	// TokenTTL is how long issued tokens last. Default 300 seconds.
	TokenTTL time.Duration
	// RegistryAddr and StorageAddr are the listen addresses. Default 127.0.0.1:0.
	RegistryAddr string
	StorageAddr  string
	// RegistryHost and StorageHost are the host:port clients are told to use
	// for the registry (Location headers, token realm) and for blob redirects.
	// They default to the listener addresses.
	RegistryHost string
	StorageHost  string
	// DNSNames are extra names the server certificate covers.
	DNSNames []string
	// BlobDir holds blob data. A new temporary directory when empty.
	BlobDir string
	// PlainHTTP serves the registry and blob storage over plain HTTP instead of TLS.
	PlainHTTP bool
}

// Fake is a running registry double.
type Fake struct {
	pat          string
	ttl          time.Duration
	auth         *authority
	blobs        *redirectingBlobs
	registry     *http.Server
	storage      *http.Server
	registryHost string
	storageHost  string
	scheme       string
	ownedBlobDir string

	mode       Auth
	uploadPath UploadPath
	delete     Delete

	tokens tokenStore
	log    requestLog

	mu     sync.Mutex
	public map[string]bool
	realm  string
	// locationBase replaces the registry's own scheme and host in Location headers when set.
	locationBase string
	sigs         map[string]bool
}

// New starts a Fake and stops it when the test ends.
func New(t testing.TB, opts Options) *Fake {
	t.Helper()

	if opts.BlobDir == "" {
		opts.BlobDir = t.TempDir()
	}

	fake, err := Start(opts)
	if err != nil {
		t.Fatalf("start fakeregistry: %v", err)
	}

	t.Cleanup(func() { _ = fake.Close() })

	return fake
}

// Start starts a Fake. Call Close to stop it.
func Start(opts Options) (*Fake, error) {
	if opts.PAT == "" {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}

		opts.PAT = "ghp_fake" + hex.EncodeToString(raw)
	}

	if opts.TokenTTL == 0 {
		opts.TokenTTL = 300 * time.Second
	}

	if opts.Auth == "" {
		opts.Auth = Bearer
	}

	if opts.UploadPath == "" {
		opts.UploadPath = Uploads
	}

	if opts.Delete == "" {
		opts.Delete = Supported
	}

	if !slices.Contains([]Auth{Bearer, Basic, None}, opts.Auth) ||
		!slices.Contains([]UploadPath{Uploads, Upload}, opts.UploadPath) ||
		!slices.Contains([]Delete{Supported, Unsupported}, opts.Delete) {
		return nil, fmt.Errorf("unknown option: auth %q, upload path %q, delete %q", opts.Auth, opts.UploadPath, opts.Delete)
	}

	if opts.RegistryAddr == "" {
		opts.RegistryAddr = "127.0.0.1:0"
	}

	if opts.StorageAddr == "" {
		opts.StorageAddr = "127.0.0.1:0"
	}

	authority, err := newAuthority(opts.DNSNames)
	if err != nil {
		return nil, err
	}

	fake := &Fake{
		pat: opts.PAT, ttl: opts.TokenTTL, auth: authority,
		public: map[string]bool{}, sigs: map[string]bool{},
		mode: opts.Auth, uploadPath: opts.UploadPath, delete: opts.Delete,
	}
	if opts.BlobDir == "" {
		if opts.BlobDir, err = os.MkdirTemp("", "fakeregistry-blobs-"); err != nil {
			return nil, err
		}

		fake.ownedBlobDir = opts.BlobDir
	}

	tlsConfig := &tls.Config{Certificates: []tls.Certificate{authority.leaf}, MinVersion: tls.VersionTLS12}

	fake.scheme = "https"
	listen := func(addr string) (net.Listener, error) { return tls.Listen("tcp", addr, tlsConfig) }

	if opts.PlainHTTP {
		listen = func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }
		fake.scheme = "http"
	}

	registryListener, err := listen(opts.RegistryAddr)
	if err != nil {
		return nil, err
	}

	storageListener, err := listen(opts.StorageAddr)
	if err != nil {
		_ = registryListener.Close()
		return nil, err
	}

	fake.registryHost = hostOr(opts.RegistryHost, registryListener)
	fake.storageHost = hostOr(opts.StorageHost, storageListener)
	fake.realm = fake.scheme + "://" + fake.registryHost + "/token"

	fake.blobs = &redirectingBlobs{
		disk: registry.NewDiskBlobHandler(opts.BlobDir),
		fake: fake,
	}
	fake.registry = &http.Server{Handler: fake.registryHandler(), ReadHeaderTimeout: 30 * time.Second}

	fake.storage = &http.Server{Handler: fake.storageHandler(), ReadHeaderTimeout: 30 * time.Second}

	go func() { _ = fake.registry.Serve(registryListener) }()
	go func() { _ = fake.storage.Serve(storageListener) }()

	return fake, nil
}

func hostOr(host string, listener net.Listener) string {
	if host != "" {
		return host
	}

	return listener.Addr().String()
}

// Close stops the servers and removes blob data the fake created itself.
func (f *Fake) Close() error {
	err := errors.Join(f.registry.Close(), f.storage.Close())
	if f.ownedBlobDir != "" {
		err = errors.Join(err, os.RemoveAll(f.ownedBlobDir))
	}

	return err
}

// PAT is the personal access token the fake accepts.
func (f *Fake) PAT() string { return f.pat }

// RegistryHost is the host:port of the registry.
func (f *Fake) RegistryHost() string { return f.registryHost }

// StorageHost is the host:port blob redirects point to.
func (f *Fake) StorageHost() string { return f.storageHost }

// CAPEM is the PEM of the CA that signed the servers' certificate.
func (f *Fake) CAPEM() []byte { return f.auth.caPEM }

// CAPool holds the CA, for clients to trust.
func (f *Fake) CAPool() *x509.CertPool { return f.auth.pool }

// Transport is an HTTP transport that trusts the fake's CA.
func (f *Fake) Transport() *http.Transport {
	return &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.auth.pool, MinVersion: tls.VersionTLS12}}
}

// MakePublic lets anonymous clients pull the repository.
func (f *Fake) MakePublic(repo string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.public[repo] = true
}

func (f *Fake) isPublic(repo string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.public[repo]
}

// SetRealm changes the token realm in challenges, to test the realm check.
func (f *Fake) SetRealm(url string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.realm = url
}

// SetLocationBase changes the scheme and host ("https://host[:port]") that
// relative registry Location headers are made absolute with, as a registry
// that knows itself by another name (Harbor's externalURL) answers.
func (f *Fake) SetLocationBase(base string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.locationBase = base
}

// LocationBase is the scheme and host registry Location headers currently use.
func (f *Fake) LocationBase() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.locationBase != "" {
		return f.locationBase
	}

	return f.scheme + "://" + f.registryHost
}

// Realm is the token realm currently put in challenges.
func (f *Fake) Realm() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.realm
}

// Log returns the requests seen so far, oldest first.
func (f *Fake) Log() []Entry { return f.log.snapshot() }

// ResetLog forgets the requests seen so far.
func (f *Fake) ResetLog() { f.log.reset() }

// Push writes image to repo (a name such as "owner/app", tag "latest" unless
// the name carries one) using the PAT, or without credentials when Auth is None.
func (f *Fake) Push(repo string, image v1.Image) error {
	options := []name.Option{name.WithDefaultTag("latest")}
	if f.scheme == "http" {
		options = append(options, name.Insecure)
	}

	ref, err := name.ParseReference(f.registryHost+"/"+repo, options...)
	if err != nil {
		return err
	}

	return remote.Write(ref, image, f.RemoteOptions(true)...)
}

// RemoteOptions are go-containerregistry options that trust the fake's CA and
// authenticate with the PAT, or anonymously when withPAT is false.
func (f *Fake) RemoteOptions(withPAT bool) []remote.Option {
	auth := authn.Anonymous
	if withPAT {
		auth = &authn.Basic{Username: "fake", Password: f.pat}
	}

	return []remote.Option{remote.WithAuth(auth), remote.WithTransport(f.Transport())}
}

func (f *Fake) registryHandler() http.Handler {
	store := f.deletes(registry.New(registry.WithBlobHandler(f.blobs), registry.Logger(discardLogger())))
	if f.uploadPath == Upload {
		store = ghcrUploadPaths(store)
	}

	mux := http.NewServeMux()
	if f.mode == Bearer {
		mux.HandleFunc("/token", f.tokenEndpoint)
	}

	mux.Handle("/", f.authorize(store))

	return f.record("registry", f.LocationBase, mux)
}

// deletes answers DELETE with 405 UNSUPPORTED when delete is unsupported.
func (f *Fake) deletes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		if f.delete == Unsupported && req.Method == http.MethodDelete {
			resp.Header().Set("Content-Type", "application/json")
			resp.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(resp).Encode(map[string]any{
				"errors": []map[string]string{{"code": "UNSUPPORTED", "message": "The operation is unsupported."}},
			})

			return
		}

		next.ServeHTTP(resp, req)
	})
}

// record logs each request after it is served and makes Location headers absolute.
func (f *Fake) record(server string, absoluteTo func() string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		cred, _ := f.credentialOf(req)

		recorder := &statusRecorder{ResponseWriter: resp, status: http.StatusOK}
		if absoluteTo != nil {
			recorder.absoluteTo = absoluteTo()
		}

		next.ServeHTTP(recorder, req)
		f.log.add(Entry{
			Server: server, Method: req.Method, Path: req.URL.Path, EscapedPath: req.URL.EscapedPath(),
			QueryKeys: queryKeys(req), Credential: cred, Status: recorder.status,
		})
	})
}

// redirectingBlobs stores blobs on disk and answers blob reads with a
// redirect to the storage server.
type redirectingBlobs struct {
	disk registry.BlobHandler
	fake *Fake
}

func (b *redirectingBlobs) Stat(ctx context.Context, repo string, h v1.Hash) (int64, error) {
	return b.disk.(registry.BlobStatHandler).Stat(ctx, repo, h)
}

func (b *redirectingBlobs) Put(ctx context.Context, repo string, h v1.Hash, rc io.ReadCloser) error {
	return b.disk.(registry.BlobPutHandler).Put(ctx, repo, h, rc)
}

func (b *redirectingBlobs) Delete(ctx context.Context, repo string, h v1.Hash) error {
	return b.disk.(registry.BlobDeleteHandler).Delete(ctx, repo, h)
}

func (b *redirectingBlobs) Get(_ context.Context, _ string, h v1.Hash) (io.ReadCloser, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}

	sig := hex.EncodeToString(raw)

	b.fake.mu.Lock()
	b.fake.sigs[sig] = true
	b.fake.mu.Unlock()

	return nil, registry.RedirectError{
		Location: fmt.Sprintf("%s://%s/%s?sig=%s", b.fake.scheme, b.fake.storageHost, h.String(), sig),
		Code:     http.StatusTemporaryRedirect,
	}
}

// storageHandler serves blobs for redirects the registry issued.
func (f *Fake) storageHandler() http.Handler {
	return f.record("storage", nil, http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		f.mu.Lock()
		signed := f.sigs[req.URL.Query().Get("sig")]
		f.mu.Unlock()

		hash, err := v1.NewHash(req.URL.Path[1:])
		if req.Method != http.MethodGet || !signed || err != nil || req.URL.Path == "" {
			http.NotFound(resp, req)
			return
		}

		rc, err := f.blobs.disk.Get(req.Context(), "", hash)
		if err != nil {
			http.NotFound(resp, req)
			return
		}
		defer func() { _ = rc.Close() }()

		resp.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.Copy(resp, rc)
	}))
}
