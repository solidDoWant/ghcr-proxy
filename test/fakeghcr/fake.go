// Package fakeghcr is an in-process test double for ghcr.io: a registry with
// bearer-token authentication, absolute upload Location headers and blob
// downloads redirected to a separate storage server, and the GitHub REST
// endpoints for container package versions on a third server. It wraps
// go-containerregistry's registry package for storage and is not a GHCR clone.
package fakeghcr

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Options configure a Fake. The zero value works for in-process tests.
type Options struct {
	// PAT is the personal access token the fake accepts. Random when empty.
	PAT string
	// TokenTTL is how long issued tokens last. Default 300 seconds.
	TokenTTL time.Duration
	// RegistryAddr and StorageAddr are the listen addresses. Default 127.0.0.1:0.
	RegistryAddr string
	StorageAddr  string
	APIAddr      string
	// RegistryHost and StorageHost are the host:port clients are told to use
	// for the registry (Location headers, token realm) and for blob redirects.
	// They default to the listener addresses.
	RegistryHost string
	StorageHost  string
	APIHost      string
	// DNSNames are extra names the server certificate covers.
	DNSNames []string
	// BlobDir holds blob data. A new temporary directory when empty.
	BlobDir string
}

// Fake is a running GHCR double.
type Fake struct {
	pat          string
	ttl          time.Duration
	auth         *authority
	blobs        *redirectingBlobs
	registry     *http.Server
	storage      *http.Server
	api          *http.Server
	registryHost string
	storageHost  string
	apiHost      string
	ownedBlobDir string

	tokens   tokenStore
	log      requestLog
	packages packageStore
	// registryStore is the registry behind authentication, for the packages
	// API to delete versions through.
	registryStore http.Handler

	mu     sync.Mutex
	public map[string]bool
	realm  string
	sigs   map[string]bool
}

// New starts a Fake and stops it when the test ends.
func New(t testing.TB, opts Options) *Fake {
	t.Helper()

	if opts.BlobDir == "" {
		opts.BlobDir = t.TempDir()
	}

	fake, err := Start(opts)
	if err != nil {
		t.Fatalf("start fakeghcr: %v", err)
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

	if opts.RegistryAddr == "" {
		opts.RegistryAddr = "127.0.0.1:0"
	}

	if opts.StorageAddr == "" {
		opts.StorageAddr = "127.0.0.1:0"
	}

	if opts.APIAddr == "" {
		opts.APIAddr = "127.0.0.1:0"
	}

	authority, err := newAuthority(opts.DNSNames)
	if err != nil {
		return nil, err
	}

	fake := &Fake{
		pat: opts.PAT, ttl: opts.TokenTTL, auth: authority,
		public: map[string]bool{}, sigs: map[string]bool{},
	}
	if opts.BlobDir == "" {
		if opts.BlobDir, err = os.MkdirTemp("", "fakeghcr-blobs-"); err != nil {
			return nil, err
		}

		fake.ownedBlobDir = opts.BlobDir
	}

	tlsConfig := &tls.Config{Certificates: []tls.Certificate{authority.leaf}, MinVersion: tls.VersionTLS12}

	registryListener, err := tls.Listen("tcp", opts.RegistryAddr, tlsConfig)
	if err != nil {
		return nil, err
	}

	storageListener, err := tls.Listen("tcp", opts.StorageAddr, tlsConfig)
	if err != nil {
		_ = registryListener.Close()
		return nil, err
	}

	apiListener, err := tls.Listen("tcp", opts.APIAddr, tlsConfig)
	if err != nil {
		_ = registryListener.Close()
		_ = storageListener.Close()

		return nil, err
	}

	fake.registryHost = hostOr(opts.RegistryHost, registryListener)
	fake.storageHost = hostOr(opts.StorageHost, storageListener)
	fake.apiHost = hostOr(opts.APIHost, apiListener)
	fake.realm = "https://" + fake.registryHost + "/token"

	fake.blobs = &redirectingBlobs{
		disk: registry.NewDiskBlobHandler(opts.BlobDir),
		fake: fake,
	}
	fake.registry = &http.Server{Handler: fake.registryHandler(), ReadHeaderTimeout: 30 * time.Second}

	fake.storage = &http.Server{Handler: fake.storageHandler(), ReadHeaderTimeout: 30 * time.Second}
	fake.api = &http.Server{Handler: fake.apiHandler(), ReadHeaderTimeout: 30 * time.Second}

	go func() { _ = fake.registry.Serve(registryListener) }()
	go func() { _ = fake.storage.Serve(storageListener) }()
	go func() { _ = fake.api.Serve(apiListener) }()

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
	err := errors.Join(f.registry.Close(), f.storage.Close(), f.api.Close())
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

// APIURL is the base URL of the GitHub REST API double, ending in "/".
func (f *Fake) APIURL() string { return "https://" + f.apiHost + "/" }

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
// the name carries one) using the PAT.
func (f *Fake) Push(repo string, image v1.Image) error {
	ref, err := name.ParseReference(f.registryHost+"/"+repo, name.WithDefaultTag("latest"))
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
	f.registryStore = f.trackManifests(registry.New(registry.WithBlobHandler(f.blobs), registry.Logger(discardLogger())))
	mux := http.NewServeMux()
	mux.HandleFunc("/token", f.tokenEndpoint)
	mux.Handle("/", f.authorize(f.registryStore))

	return f.record("registry", "https://"+f.registryHost, mux)
}

// record logs each request after it is served and makes Location headers absolute.
func (f *Fake) record(server, absoluteTo string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		cred, _ := f.credentialOf(req)
		recorder := &statusRecorder{ResponseWriter: resp, status: http.StatusOK, absoluteTo: absoluteTo}
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
		Location: fmt.Sprintf("https://%s/%s?sig=%s", b.fake.storageHost, h.String(), sig),
		Code:     http.StatusTemporaryRedirect,
	}
}

// storageHandler serves blobs for redirects the registry issued.
func (f *Fake) storageHandler() http.Handler {
	return f.record("storage", "", http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
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
