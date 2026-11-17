package upstream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/solidDoWant/registrygate/internal/metrics"
	"github.com/solidDoWant/registrygate/internal/model"
)

const (
	// challengeLifetime is how long a ping's challenge is reused.
	challengeLifetime = 5 * time.Minute
	// defaultResponseHeaderTimeout applies when the upstream sets none.
	defaultResponseHeaderTimeout = 30 * time.Second
	// defaultTokenTimeout applies when the upstream sets none.
	defaultTokenTimeout = 10 * time.Second
)

// ErrForeignHost is a request to a host other than the client's upstream.
// The client never sends it, so no credential can reach that host.
var ErrForeignHost = errors.New("request is not for the upstream")

// ClientOptions are the parts of a Client that don't come from the
// UpstreamRegistry.
type ClientOptions struct {
	Logger *slog.Logger
	// Metrics counts token fetches; nil turns it off.
	Metrics *metrics.Metrics
}

// Client sends requests to one UpstreamRegistry and authenticates them as
// its challenge asks: no header, Basic with the credential, or a bearer
// token from the token endpoint. It never follows redirects and sends a
// credential only to the upstream's own host and to an allowed token realm.
type Client struct {
	url          *url.URL
	registry     name.Registry
	transport    *http.Transport
	allowed      []string
	aliases      []string
	tokenTimeout time.Duration
	logger       *slog.Logger
	metrics      *metrics.Metrics
	name         string // "<namespace>/<name>", the metrics label
	tokens       *tokenCache
	now          func() time.Time

	mu          sync.Mutex
	challenge   *transport.Challenge
	challengeAt time.Time
}

// NewClient returns a client for the ready upstream u.
func NewClient(u *model.Upstream, options ClientOptions) (*Client, error) {
	if u == nil || u.URL == nil || u.URL.Host == "" {
		return nil, errors.New("upstream has no URL")
	}

	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}

	var nameOptions []name.Option
	if u.URL.Scheme == "http" {
		nameOptions = append(nameOptions, name.Insecure)
	}

	registry, err := name.NewRegistry(u.URL.Host, nameOptions...)
	if err != nil {
		return nil, fmt.Errorf("upstream host: %w", err)
	}

	responseHeader, token := defaultResponseHeaderTimeout, defaultTokenTimeout

	timeouts := u.Object.Spec.Timeouts
	if timeouts.ResponseHeader != nil && timeouts.ResponseHeader.Duration > 0 {
		responseHeader = timeouts.ResponseHeader.Duration
	}

	if timeouts.Token != nil && timeouts.Token.Duration > 0 {
		token = timeouts.Token.Duration
	}

	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("http.DefaultTransport is not an *http.Transport")
	}

	httpTransport := base.Clone()
	httpTransport.ResponseHeaderTimeout = responseHeader
	httpTransport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: u.CAs}

	allowed := []string{CanonicalHost(u.URL.Scheme, u.URL.Host)}
	for _, host := range u.Object.Spec.TokenRealmHosts {
		allowed = append(allowed, CanonicalHost(u.URL.Scheme, host))
	}

	now := time.Now

	return &Client{
		url: u.URL, registry: registry, transport: httpTransport, allowed: allowed, aliases: u.Object.Spec.AliasHosts,
		tokenTimeout: token, logger: logger, metrics: options.Metrics, name: u.Name().String(),
		tokens: newTokenCache(now), now: now,
	}, nil
}

// Name is the upstream's "<namespace>/<name>", the value of the metrics' upstream label.
func (c *Client) Name() string { return c.name }

// URL is the upstream's URL.
func (c *Client) URL() *url.URL { return c.url }

// OntoURL moves a URL on one of the upstream's alias hosts onto its url. It
// returns false for any other URL.
func (c *Client) OntoURL(target *url.URL) (*url.URL, bool) {
	return model.OntoURL(target, c.url, c.aliases)
}

// Transport is the client's transport. It follows no redirects and adds no
// credential.
func (c *Client) Transport() http.RoundTripper { return c.transport }

// Do sends req, which must be for the upstream's scheme and host, with cred
// for scopes. When the upstream answers 401 with a challenge, that challenge
// replaces the cached one and the request is sent once more if its body
// can be replayed. The response, whatever its status, is returned as is.
// Errors never contain a credential or token.
func (c *Client) Do(req *http.Request, cred Credential, scopes []string) (*http.Response, error) {
	if !strings.EqualFold(req.URL.Scheme, c.url.Scheme) ||
		CanonicalHost(req.URL.Scheme, req.URL.Host) != CanonicalHost(c.url.Scheme, c.url.Host) {
		return nil, ErrForeignHost
	}

	challenge, err := c.currentChallenge(req.Context())
	if err != nil {
		return nil, err
	}

	resp, token, err := c.send(req, challenge, cred, scopes)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusUnauthorized || len(resp.Header.Values("WWW-Authenticate")) == 0 {
		return resp, nil
	}

	if token != nil {
		c.tokens.drop(*token)
	}

	fresh, err := c.parseChallenge(req.Context(), resp.Header)
	if err != nil {
		discard(resp)
		return nil, err
	}

	if fresh, err = c.checkChallenge(fresh); err != nil {
		discard(resp)
		return nil, err
	}

	c.setChallenge(fresh)

	retry, ok := replay(req)
	if !ok {
		return resp, nil
	}

	discard(resp)

	resp, _, err = c.send(retry, fresh, cred, scopes)

	return resp, err
}

// send authenticates a copy of req as challenge asks and sends it. token
// is the bearer token it used, if any.
func (c *Client) send(req *http.Request, challenge *transport.Challenge, cred Credential, scopes []string) (*http.Response, *credentialToken, error) {
	out := req.Clone(req.Context())
	out.Header.Del("Authorization")

	var used *credentialToken

	switch strings.ToLower(challenge.Scheme) {
	case "":
	case "basic":
		if !cred.Anonymous() {
			out.SetBasicAuth(cred.Username, cred.Password)
		}
	case "bearer":
		token, err := c.token(req.Context(), challenge, cred, scopes)
		if err != nil {
			return nil, nil, err
		}

		out.Header.Set("Authorization", "Bearer "+token.value)
		used = &token
	default:
		c.logger.Error("upstream challenge scheme is not supported", "scheme", challenge.Scheme)
		return nil, nil, ErrUnavailable
	}

	resp, err := c.transport.RoundTrip(out)
	if err != nil {
		return nil, nil, err
	}

	return resp, used, nil
}

// token returns a cached bearer token for cred and scopes, or exchanges one.
func (c *Client) token(ctx context.Context, challenge *transport.Challenge, cred Credential, scopes []string) (credentialToken, error) {
	key := keyOf(cred, scopes)
	if value, ok := c.tokens.get(key); ok {
		return credentialToken{value: value, key: key}, nil
	}

	auth := authn.Anonymous
	if !cred.Anonymous() {
		auth = authn.FromConfig(authn.AuthConfig{Username: cred.Username, Password: cred.Password})
	}

	exchangeCtx, cancel := context.WithTimeout(ctx, c.tokenTimeout)
	defer cancel()

	fetched, err := transport.Exchange(exchangeCtx, c.registry, auth, c.transport, strings.Fields(key.scopes), challenge)
	if err != nil {
		failure := c.exchangeFailure(cred, err)
		c.metrics.UpstreamTokenFetch(c.name, metrics.TargetUpstream, tokenFetchResult(failure))

		return credentialToken{}, failure
	}

	c.metrics.UpstreamTokenFetch(c.name, metrics.TargetUpstream, metrics.ResultOK)

	value := fetched.Token
	if value == "" {
		value = fetched.AccessToken
	}

	c.tokens.put(key, value, fetched.ExpiresIn)

	return credentialToken{value: value, key: key}, nil
}

func (c *Client) exchangeFailure(cred Credential, err error) error {
	c.logger.Error("upstream token exchange failed", "anonymous", cred.Anonymous(), "error", err.Error())

	var registryErr *transport.Error
	if errors.As(err, &registryErr) &&
		(registryErr.StatusCode == http.StatusUnauthorized || registryErr.StatusCode == http.StatusForbidden) {
		if cred.Anonymous() {
			return ErrAnonymousRefused
		}

		return ErrCredentialRejected
	}

	return ErrUnavailable
}

// currentChallenge returns the cached challenge, pinging the upstream when
// there is none or it is older than challengeLifetime.
func (c *Client) currentChallenge(ctx context.Context) (*transport.Challenge, error) {
	c.mu.Lock()
	challenge, at := c.challenge, c.challengeAt
	c.mu.Unlock()

	if challenge != nil && c.now().Sub(at) < challengeLifetime {
		return challenge, nil
	}

	pingCtx, cancel := context.WithTimeout(ctx, c.tokenTimeout)
	defer cancel()

	fresh, err := transport.Ping(pingCtx, c.registry, c.transport)
	if err != nil {
		c.logger.Error("upstream ping failed", "error", err.Error())
		return nil, ErrUnavailable
	}

	if fresh, err = c.checkChallenge(fresh); err != nil {
		return nil, err
	}

	c.setChallenge(fresh)

	return fresh, nil
}

func (c *Client) setChallenge(challenge *transport.Challenge) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.challenge, c.challengeAt = challenge, c.now()
}

// checkChallenge refuses a bearer challenge whose realm is outside the
// upstream's host, its token realm hosts and its alias hosts, so no
// credential goes there. A realm on an alias host is moved onto the
// upstream's url in the returned challenge.
func (c *Client) checkChallenge(challenge *transport.Challenge) (*transport.Challenge, error) {
	if !strings.EqualFold(challenge.Scheme, "bearer") {
		return challenge, nil
	}

	realm := challenge.Parameters["realm"]

	parsed, err := url.Parse(realm)
	if err == nil {
		if moved, ok := c.OntoURL(parsed); ok {
			return withRealm(challenge, moved.String()), nil
		}
	}

	if err != nil || !strings.EqualFold(parsed.Scheme, c.url.Scheme) || parsed.Host == "" || parsed.User != nil ||
		!slices.Contains(c.allowed, CanonicalHost(parsed.Scheme, parsed.Host)) {
		c.logger.Error("upstream token realm is not allowed; no credential sent", "realm", realm)
		return nil, ErrRealmNotAllowed
	}

	return challenge, nil
}

// withRealm returns a copy of challenge with another realm.
func withRealm(challenge *transport.Challenge, realm string) *transport.Challenge {
	moved := *challenge
	moved.Parameters = maps.Clone(challenge.Parameters)
	moved.Parameters["realm"] = realm

	return &moved
}

// parseChallenge reads the challenge of a 401 answer with
// go-containerregistry's own challenge parser: it hands Ping a transport
// that answers with the header instead of contacting the upstream.
func (c *Client) parseChallenge(ctx context.Context, header http.Header) (*transport.Challenge, error) {
	answer := challengeAnswer{header: header.Clone()}

	// Ping only tries https when the registry isn't marked insecure, so it
	// asks the canned transport exactly once.
	registry, err := name.NewRegistry(c.registry.RegistryStr())
	if err != nil {
		return nil, ErrUnavailable
	}

	challenge, err := transport.Ping(ctx, registry, answer)
	if err != nil {
		c.logger.Error("upstream challenge could not be read", "error", err.Error())
		return nil, ErrUnavailable
	}

	challenge.Insecure = strings.EqualFold(c.url.Scheme, "http")

	return challenge, nil
}

// challengeAnswer answers every request with 401 and the stored
// WWW-Authenticate headers.
type challengeAnswer struct {
	header http.Header
}

func (a challengeAnswer) RoundTrip(req *http.Request) (*http.Response, error) {
	header := http.Header{}
	for _, value := range a.header.Values("WWW-Authenticate") {
		header.Add("WWW-Authenticate", value)
	}

	return &http.Response{
		StatusCode: http.StatusUnauthorized, Header: header, Body: http.NoBody, Request: req,
		ProtoMajor: 1, ProtoMinor: 1,
	}, nil
}

// replay returns a copy of req that can be sent again, or false when its
// body can't be read a second time.
func replay(req *http.Request) (*http.Request, bool) {
	retry := req.Clone(req.Context())
	if req.Body == nil || req.Body == http.NoBody {
		return retry, true
	}

	if req.GetBody == nil {
		return nil, false
	}

	body, err := req.GetBody()
	if err != nil {
		return nil, false
	}

	retry.Body = body

	return retry, true
}

func discard(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
