package mirror

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/solidDoWant/registrygate/internal/metrics"
	"github.com/solidDoWant/registrygate/internal/model"
)

// StateSource supplies the credentials and CAs a Client uses. The boolean is
// false while the mirror can't be used.
type StateSource interface {
	Get() (State, bool)
}

// Failures of a mirror request before the mirror answered it.
var (
	// ErrUnavailable is the mirror being unreachable, its files not loaded,
	// or its token endpoint failing or not on an allowed host.
	ErrUnavailable = errors.New("mirror unavailable")
	// ErrCredentialRejected is the mirror's token endpoint refusing the
	// mirror's credentials (or an anonymous token).
	ErrCredentialRejected = errors.New("mirror rejected its credentials")
)

const (
	maxCachedTokens      = 4096
	expiryMargin         = 30 * time.Second
	defaultTokenLifetime = 60 * time.Second
)

// ClientConfig configures a Client.
type ClientConfig struct {
	Mirror *Settings
	// TokenTimeout bounds one ping or token exchange.
	TokenTimeout time.Duration
	Logger       *slog.Logger
	// Metrics counts token fetches under the mirror target and Upstream; nil turns it off.
	Metrics *metrics.Metrics
	// Upstream is the "<namespace>/<name>" of the mirror's upstream, the metrics' upstream label.
	Upstream string
}

// Client sends pull requests to one mirror with a token fetched with the
// mirror's own credentials. It has its own token cache, accepts only a token
// realm on the mirror's own scheme and host, its token realm hosts or its
// alias hosts, and never sees the PAT.
type Client struct {
	cfg      ClientConfig
	source   StateSource
	registry name.Registry

	mu            sync.Mutex
	transport     *http.Transport
	transportCAs  *x509.CertPool
	challenge     *transport.Challenge
	challengeGen  uint64
	challengeCAs  *x509.CertPool
	open          bool
	tokens        *lru.Cache[tokenKey, tokenEntry]
	responseLimit time.Duration
}

type tokenKey struct {
	generation uint64
	scope      string
}

type tokenEntry struct {
	value      string
	validUntil time.Time
}

func newClient(cfg ClientConfig, source StateSource) (*Client, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	var options []name.Option
	if cfg.Mirror.URL.Scheme == "http" {
		options = append(options, name.Insecure)
	}

	registry, err := name.NewRegistry(cfg.Mirror.URL.Host, options...)
	if err != nil {
		return nil, err
	}

	tokens, err := lru.New[tokenKey, tokenEntry](maxCachedTokens)
	if err != nil {
		return nil, err
	}

	return &Client{cfg: cfg, source: source, registry: registry, tokens: tokens, responseLimit: cfg.Mirror.ResponseHeaderTimeout}, nil
}

// URL is the mirror's base URL.
func (c *Client) URL() *url.URL { return c.cfg.Mirror.URL }

// PathPrefix is the mirror's repository path prefix.
func (c *Client) PathPrefix() string { return c.cfg.Mirror.PathPrefix }

// FallbackToUpstream reports whether failed pulls go on to the upstream.
func (c *Client) FallbackToUpstream() bool { return c.cfg.Mirror.FallbackToUpstream }

// Send sends req, which must already point at the mirror and carry no
// credential, with a token for scope. Only the bearer token is set; redirects
// are not followed. A 401 with a cached token is retried once with a new
// token; req must then have no body.
func (c *Client) Send(req *http.Request, scope string) (*http.Response, error) {
	state, ok := c.source.Get()
	if !ok {
		return nil, ErrUnavailable
	}

	roundTripper := c.transportFor(state.RootCAs)

	for attempt := 0; ; attempt++ {
		token, key, err := c.token(req.Context(), roundTripper, state, scope)
		if err != nil {
			return nil, err
		}

		out := req.Clone(req.Context())
		out.Header.Del("Authorization")
		// An empty token means the mirror needs no authentication: send no credential.
		if token != "" {
			out.Header.Set("Authorization", "Bearer "+token)
		}

		resp, err := roundTripper.RoundTrip(out)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return resp, nil
		}

		c.tokens.Remove(key)

		_ = resp.Body.Close()
	}
}

// transportFor returns the transport trusting pool, rebuilt when the CA
// file changes.
func (c *Client) transportFor(pool *x509.CertPool) *http.Transport {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.transport != nil && c.transportCAs == pool {
		return c.transport
	}

	built := http.DefaultTransport.(*http.Transport).Clone()
	built.ResponseHeaderTimeout = c.responseLimit

	if pool != nil {
		built.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}

	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}

	c.transport, c.transportCAs = built, pool

	return built
}

func (c *Client) token(ctx context.Context, roundTripper http.RoundTripper, state State, scope string) (string, tokenKey, error) {
	key := tokenKey{generation: state.Generation, scope: scope}

	if entry, ok := c.tokens.Get(key); ok {
		if time.Now().Before(entry.validUntil) {
			return entry.value, key, nil
		}

		c.tokens.Remove(key)
	}

	challenge, err := c.ping(ctx, roundTripper, state)
	if err != nil {
		c.cfg.Metrics.UpstreamTokenFetch(c.cfg.Upstream, metrics.TargetMirror, metrics.ResultError)
		return "", key, err
	}

	if challenge == nil {
		// The mirror answered the ping with 200: no authentication, no token request.
		return "", key, nil
	}

	auth := authn.Anonymous
	if state.Credentials != nil {
		auth = authn.FromConfig(authn.AuthConfig{Username: state.Credentials.Username, Password: state.Credentials.Password})
	}

	exchangeCtx, cancel := context.WithTimeout(ctx, c.cfg.TokenTimeout)
	defer cancel()

	fetched, err := transport.Exchange(exchangeCtx, c.registry, auth, roundTripper, []string{scope}, challenge)
	if err != nil {
		c.cfg.Logger.Error("mirror token exchange failed", "url", c.cfg.Mirror.URL.String(), "error", err.Error())

		var registryErr *transport.Error
		if errors.As(err, &registryErr) &&
			(registryErr.StatusCode == http.StatusUnauthorized || registryErr.StatusCode == http.StatusForbidden) {
			c.cfg.Metrics.UpstreamTokenFetch(c.cfg.Upstream, metrics.TargetMirror, metrics.ResultRejected)
			return "", key, ErrCredentialRejected
		}

		c.cfg.Metrics.UpstreamTokenFetch(c.cfg.Upstream, metrics.TargetMirror, metrics.ResultError)

		return "", key, ErrUnavailable
	}

	c.cfg.Metrics.UpstreamTokenFetch(c.cfg.Upstream, metrics.TargetMirror, metrics.ResultOK)

	value := fetched.Token
	if value == "" {
		value = fetched.AccessToken
	}

	lifetime := defaultTokenLifetime
	if fetched.ExpiresIn > 0 {
		lifetime = time.Duration(fetched.ExpiresIn) * time.Second
	}

	if validUntil := time.Now().Add(lifetime - expiryMargin); time.Now().Before(validUntil) {
		c.tokens.Add(key, tokenEntry{value: value, validUntil: validUntil})
	}

	return value, key, nil
}

// ping returns the mirror's challenge, pinging again after a credentials or
// CA change. It returns a nil challenge and no error when the mirror answers
// the ping with 200 and so needs no authentication.
func (c *Client) ping(ctx context.Context, roundTripper http.RoundTripper, state State) (*transport.Challenge, error) {
	c.mu.Lock()
	challenge, open := c.challenge, c.open
	fresh := (challenge != nil || open) && c.challengeGen == state.Generation && c.challengeCAs == state.RootCAs
	c.mu.Unlock()

	if !fresh {
		pingCtx, cancel := context.WithTimeout(ctx, c.cfg.TokenTimeout)
		defer cancel()

		recorder := &statusRecorder{inner: roundTripper}

		pinged, err := transport.Ping(pingCtx, c.registry, recorder)
		if err != nil {
			c.cfg.Logger.Error("mirror ping failed", "url", c.cfg.Mirror.URL.String(), "error", err.Error())
			return nil, ErrUnavailable
		}

		open = recorder.ok.Load()
		if open {
			pinged = nil
		}

		c.mu.Lock()
		c.challenge, c.open, c.challengeGen, c.challengeCAs = pinged, open, state.Generation, state.RootCAs
		c.mu.Unlock()

		challenge = pinged
	}

	if open {
		return nil, nil //nolint:nilnil // nil challenge means no authentication
	}

	if !strings.EqualFold(challenge.Scheme, "bearer") {
		c.cfg.Logger.Error("mirror challenge is not a bearer challenge", "url", c.cfg.Mirror.URL.String(), "scheme", challenge.Scheme)
		return nil, ErrUnavailable
	}

	checked, ok := c.checkRealm(challenge)
	if !ok {
		c.cfg.Logger.Error("mirror token realm is not on the mirror's host or its token realm hosts; no credential sent",
			"url", c.cfg.Mirror.URL.String(), "realm", challenge.Parameters["realm"])

		return nil, ErrUnavailable
	}

	return checked, nil
}

// statusRecorder notes whether any response was a 200, which the ping result
// alone cannot tell from a 401 without a challenge.
type statusRecorder struct {
	inner http.RoundTripper
	ok    atomic.Bool
}

func (r *statusRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.inner.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusOK {
		r.ok.Store(true)
	}

	return resp, err
}

// checkRealm accepts a bearer challenge whose realm is on the mirror's own
// scheme and host, or on one of its token realm hosts with the mirror's
// scheme. A realm on an alias host is accepted and moved onto the mirror's
// URL in the returned challenge, so nothing is sent to the alias.
func (c *Client) checkRealm(challenge *transport.Challenge) (*transport.Challenge, bool) {
	parsed, err := url.Parse(challenge.Parameters["realm"])
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return nil, false
	}

	if moved, ok := c.OntoURL(parsed); ok {
		checked := *challenge
		checked.Parameters = maps.Clone(challenge.Parameters)
		checked.Parameters["realm"] = moved.String()

		return &checked, true
	}

	if c.SameHost(parsed) {
		return challenge, true
	}

	mirrorURL := c.cfg.Mirror.URL
	if !strings.EqualFold(parsed.Scheme, mirrorURL.Scheme) {
		return nil, false
	}

	host := model.CanonicalHost(parsed.Scheme, parsed.Host)
	for _, allowed := range c.cfg.Mirror.TokenRealmHosts {
		if host == model.CanonicalHost(parsed.Scheme, allowed) {
			return challenge, true
		}
	}

	return nil, false
}

// SameHost reports whether u is on the mirror's scheme and host.
func (c *Client) SameHost(u *url.URL) bool {
	mirrorURL := c.cfg.Mirror.URL

	return strings.EqualFold(u.Scheme, mirrorURL.Scheme) &&
		model.CanonicalHost(u.Scheme, u.Host) == model.CanonicalHost(mirrorURL.Scheme, mirrorURL.Host)
}

// OntoURL moves a URL on one of the mirror's alias hosts onto the mirror's
// scheme and host. It returns false for any other URL.
func (c *Client) OntoURL(target *url.URL) (*url.URL, bool) {
	return model.OntoURL(target, c.cfg.Mirror.URL, c.cfg.Mirror.AliasHosts)
}
