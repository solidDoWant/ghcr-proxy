package upstream

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	lru "github.com/hashicorp/golang-lru/v2"
)

// Kind is the credential a token was fetched with.
type Kind string

// Token kinds.
const (
	// KindPAT is a token fetched with the PAT.
	KindPAT Kind = "pat"
	// KindAnonymous is a token fetched with no credential.
	KindAnonymous Kind = "anonymous"
)

// Failures of a token fetch. Each maps to one fixed client answer; the
// underlying library error goes only to the process log.
var (
	// ErrPATNotLoaded is a PAT-backed token requested while no PAT is loaded.
	ErrPATNotLoaded = errors.New("PAT not loaded")
	// ErrCredentialRejected is the token endpoint refusing the PAT.
	ErrCredentialRejected = errors.New("upstream rejected the proxy's credential")
	// ErrAnonymousRefused is the token endpoint refusing an anonymous token.
	ErrAnonymousRefused = errors.New("upstream refused an anonymous token")
	// ErrRealmNotAllowed is a token realm outside the upstream and tokenHosts.
	ErrRealmNotAllowed = errors.New("upstream token endpoint not allowed")
	// ErrUnavailable is the upstream or its token endpoint failing otherwise.
	ErrUnavailable = errors.New("upstream unavailable")
)

const (
	// patUsername is the Basic user name sent with the PAT; GHCR ignores it.
	patUsername = "ghcr-proxy"
	// maxCachedTokens bounds the token cache.
	maxCachedTokens = 4096
	// expiryMargin is how long before its expiry a token stops being reused.
	expiryMargin = 30 * time.Second
	// defaultTokenLifetime is assumed when the token endpoint gives none.
	defaultTokenLifetime = 60 * time.Second
)

// PATSource provides the current PAT and its generation.
type PATSource interface {
	Get() (pat string, generation uint64, ok bool)
}

// TokensConfig configures Tokens.
type TokensConfig struct {
	// URL is the upstream registry, for example https://ghcr.io.
	URL *url.URL
	// TokenHosts are extra hosts allowed as token realms.
	TokenHosts []string
	// Transport carries ping and token requests. http.DefaultTransport when nil.
	Transport http.RoundTripper
	// TokenTimeout bounds one ping or token exchange.
	TokenTimeout time.Duration
	PAT          PATSource
	Logger       *slog.Logger
}

// Token is a registry token for one scope set.
type Token struct {
	Value string
	key   cacheKey
}

// Tokens fetches and caches upstream registry tokens. It only ever asks for
// the scopes it is given and never sends the PAT anywhere but the allowed
// token realm.
type Tokens struct {
	cfg      TokensConfig
	registry name.Registry

	mu             sync.Mutex
	challenge      *transport.Challenge
	challengeGen   uint64
	cache          *lru.Cache[cacheKey, cacheEntry]
	timeNow        func() time.Time
	allowedRealmTo []string
}

type cacheKey struct {
	kind       Kind
	generation uint64
	scopes     string
}

type cacheEntry struct {
	key        cacheKey
	value      string
	validUntil time.Time
}

// NewTokens returns a token source for the upstream in cfg.
func NewTokens(cfg TokensConfig) (*Tokens, error) {
	if cfg.URL == nil || cfg.URL.Host == "" {
		return nil, errors.New("upstream URL has no host")
	}

	if cfg.Transport == nil {
		cfg.Transport = http.DefaultTransport
	}

	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	var options []name.Option
	if cfg.URL.Scheme == "http" {
		options = append(options, name.Insecure)
	}

	registry, err := name.NewRegistry(cfg.URL.Host, options...)
	if err != nil {
		return nil, err
	}

	allowed := []string{CanonicalHost(cfg.URL.Scheme, cfg.URL.Host)}
	for _, host := range cfg.TokenHosts {
		allowed = append(allowed, CanonicalHost(cfg.URL.Scheme, host))
	}

	cache, err := lru.New[cacheKey, cacheEntry](maxCachedTokens)
	if err != nil {
		return nil, err
	}

	return &Tokens{
		cfg: cfg, registry: registry, cache: cache,
		timeNow: time.Now, allowedRealmTo: allowed,
	}, nil
}

// Token returns a token for scopes, fetched with the PAT for KindPAT and with
// no credential for KindAnonymous. Tokens are reused until shortly before
// they expire; a new PAT generation never reuses an older token.
func (t *Tokens) Token(ctx context.Context, kind Kind, scopes []string) (Token, error) {
	auth := authn.Anonymous

	var generation uint64

	if kind == KindPAT {
		pat, gen, ok := t.cfg.PAT.Get()
		if !ok {
			return Token{}, ErrPATNotLoaded
		}

		generation = gen
		auth = authn.FromConfig(authn.AuthConfig{Username: patUsername, Password: pat})
	} else if t.cfg.PAT != nil {
		_, generation, _ = t.cfg.PAT.Get()
	}

	sorted := slices.Clone(scopes)
	slices.Sort(sorted)
	key := cacheKey{kind: kind, generation: generation, scopes: strings.Join(sorted, " ")}

	if value, ok := t.cached(key); ok {
		return Token{Value: value, key: key}, nil
	}

	challenge, err := t.ping(ctx, generation)
	if err != nil {
		return Token{}, err
	}

	exchangeCtx, cancel := context.WithTimeout(ctx, t.cfg.TokenTimeout)
	defer cancel()

	fetched, err := transport.Exchange(exchangeCtx, t.registry, auth, t.cfg.Transport, sorted, challenge)
	if err != nil {
		return Token{}, t.exchangeFailure(kind, err)
	}

	value := fetched.Token
	if value == "" {
		value = fetched.AccessToken
	}

	lifetime := defaultTokenLifetime
	if fetched.ExpiresIn > 0 {
		lifetime = time.Duration(fetched.ExpiresIn) * time.Second
	}

	t.store(cacheEntry{key: key, value: value, validUntil: t.timeNow().Add(lifetime - expiryMargin)})

	return Token{Value: value, key: key}, nil
}

// Invalidate drops token from the cache, so the next request fetches a new one.
func (t *Tokens) Invalidate(token Token) {
	if entry, ok := t.cache.Peek(token.key); ok && entry.value == token.Value {
		t.cache.Remove(token.key)
	}
}

func (t *Tokens) cached(key cacheKey) (string, bool) {
	entry, ok := t.cache.Get(key)
	if !ok {
		return "", false
	}

	if !t.timeNow().Before(entry.validUntil) {
		t.cache.Remove(key)
		return "", false
	}

	return entry.value, true
}

func (t *Tokens) store(entry cacheEntry) {
	if t.timeNow().Before(entry.validUntil) {
		t.cache.Add(entry.key, entry)
	}
}

// ping returns the upstream's challenge, pinging again after a PAT change.
func (t *Tokens) ping(ctx context.Context, generation uint64) (*transport.Challenge, error) {
	t.mu.Lock()
	challenge, pinged := t.challenge, t.challengeGen
	t.mu.Unlock()

	if challenge == nil || pinged != generation {
		pingCtx, cancel := context.WithTimeout(ctx, t.cfg.TokenTimeout)
		defer cancel()

		fresh, err := transport.Ping(pingCtx, t.registry, t.cfg.Transport)
		if err != nil {
			t.cfg.Logger.Error("upstream ping failed", "error", err.Error())
			return nil, ErrUnavailable
		}

		t.mu.Lock()
		t.challenge, t.challengeGen = fresh, generation
		t.mu.Unlock()

		challenge = fresh
	}

	if !strings.EqualFold(challenge.Scheme, "bearer") {
		t.cfg.Logger.Error("upstream challenge is not a bearer challenge", "scheme", challenge.Scheme)
		return nil, ErrUnavailable
	}

	if !t.realmAllowed(challenge.Parameters["realm"]) {
		t.cfg.Logger.Error("upstream token realm is not allowed; no credential sent", "realm", challenge.Parameters["realm"])
		return nil, ErrRealmNotAllowed
	}

	return challenge, nil
}

func (t *Tokens) realmAllowed(realm string) bool {
	parsed, err := url.Parse(realm)
	if err != nil || !strings.EqualFold(parsed.Scheme, t.cfg.URL.Scheme) || parsed.Host == "" || parsed.User != nil {
		return false
	}

	return slices.Contains(t.allowedRealmTo, CanonicalHost(parsed.Scheme, parsed.Host))
}

func (t *Tokens) exchangeFailure(kind Kind, err error) error {
	t.cfg.Logger.Error("upstream token exchange failed", "kind", string(kind), "error", err.Error())

	var registryErr *transport.Error
	if errors.As(err, &registryErr) &&
		(registryErr.StatusCode == http.StatusUnauthorized || registryErr.StatusCode == http.StatusForbidden) {
		if kind == KindPAT {
			return ErrCredentialRejected
		}

		return ErrAnonymousRefused
	}

	return ErrUnavailable
}

// CanonicalHost lower-cases host and removes the scheme's default port, for
// comparing hosts.
func CanonicalHost(scheme, host string) string {
	host = strings.ToLower(host)

	switch strings.ToLower(scheme) {
	case "https":
		host = strings.TrimSuffix(host, ":443")
	case "http":
		host = strings.TrimSuffix(host, ":80")
	}

	return host
}
