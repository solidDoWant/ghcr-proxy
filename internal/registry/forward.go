package registry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/solidDoWant/ghcr-proxy/api/v1alpha1"
	"github.com/solidDoWant/ghcr-proxy/internal/metrics"
	"github.com/solidDoWant/ghcr-proxy/internal/mirror"
	"github.com/solidDoWant/ghcr-proxy/internal/upstream"
)

// maxManifestBytes is the largest manifest the proxy buffers for a push.
const maxManifestBytes = 4 << 20

// Credential is the upstream credential a forwarded request carries.
type Credential int

const (
	// CredentialAnonymous is a token fetched with no credential.
	CredentialAnonymous Credential = iota
	// CredentialPAT is a token fetched with the PAT, scoped to the request.
	CredentialPAT
)

// Forward is what the forwarder needs to know about an authorized request.
type Forward struct {
	Request Request
	// Credential is PAT-backed only when a policy and its grant allow the
	// request; everything else goes upstream with an anonymous token.
	Credential Credential
	// AnonymousCaller is set when the client sent no credential. Refused
	// anonymous pulls are then answered with a challenge for 404 too.
	AnonymousCaller bool
}

// Scopes are the token scopes the request needs upstream.
func (r Request) Scopes() []string {
	switch r.Action {
	case v1alpha1.ActionPush:
		scopes := []string{"repository:" + r.Name + ":pull,push"}
		if r.Mount != nil {
			scopes = append(scopes, "repository:"+r.Mount.From+":pull")
		}

		return scopes
	case v1alpha1.ActionDelete:
		return []string{"repository:" + r.Name + ":delete"}
	case v1alpha1.ActionPull:
		return []string{"repository:" + r.Name + ":pull"}
	}

	return nil
}

// TokenSource fetches upstream registry tokens.
type TokenSource interface {
	Token(ctx context.Context, kind upstream.Kind, scopes []string) (upstream.Token, error)
	Invalidate(token upstream.Token)
}

// ForwarderConfig configures a Forwarder.
type ForwarderConfig struct {
	// Upstream is the upstream registry URL; only scheme and host are used.
	Upstream *url.URL
	// Transport carries forwarded requests. It must not follow redirects or
	// add credentials. http.DefaultTransport when nil.
	Transport http.RoundTripper
	Tokens    TokenSource
	// Timeout bounds one whole upstream request, body included.
	Timeout time.Duration
	Logger  *slog.Logger
	// PolicyMirror serves pulls a policy allows; AnonymousMirror serves
	// anonymous and not-allowed pulls. Nil when not configured.
	PolicyMirror    MirrorClient
	AnonymousMirror MirrorClient
	// Metrics counts upstream requests and mirror fallbacks; nil turns it off.
	Metrics *metrics.Metrics
}

// MirrorClient sends pulls to one mirror with the mirror's own credential.
type MirrorClient interface {
	URL() *url.URL
	PathPrefix() string
	FallbackToUpstream() bool
	// SameHost reports whether u is on the mirror's scheme and host.
	SameHost(u *url.URL) bool
	// Send sends req, already pointed at the mirror, with a pull token for
	// scope.
	Send(req *http.Request, scope string) (*http.Response, error)
}

// Forwarder sends classified, authorized requests to the upstream with the
// proxy's own credential and passes the answer back.
type Forwarder struct {
	cfg   ForwarderConfig
	proxy *httputil.ReverseProxy
}

// NewForwarder returns a Forwarder for cfg.
func NewForwarder(cfg ForwarderConfig) *Forwarder {
	if cfg.Transport == nil {
		cfg.Transport = http.DefaultTransport
	}

	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	forwarder := &Forwarder{cfg: cfg}
	forwarder.proxy = &httputil.ReverseProxy{
		Rewrite:        forwarder.rewrite,
		Transport:      &tokenTransport{forwarder: forwarder},
		ModifyResponse: forwarder.modifyResponse,
		ErrorHandler:   forwarder.handleError,
		// Flush at once so streamed blobs never collect in the proxy.
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(cfg.Logger.Handler(), slog.LevelError),
	}

	return forwarder
}

// exchange carries one request's state between the reverse proxy's hooks.
type exchange struct {
	forward    Forward
	clientHost string
	// manifest is the buffered body of a manifest push, for a retry.
	manifest []byte
	// upstreamStatus is the status of the last upstream answer.
	upstreamStatus int
}

type exchangeKey struct{}

// ServeForward forwards the request and writes the answer. It returns the
// status the upstream answered with, or 0 if nothing came back.
func (f *Forwarder) ServeForward(resp http.ResponseWriter, req *http.Request, forward Forward) int {
	state := &exchange{forward: forward, clientHost: req.Host}

	if forward.Request.Route == RouteManifest && req.Method == http.MethodPut {
		manifest, err := io.ReadAll(io.LimitReader(req.Body, maxManifestBytes+1))
		if err != nil {
			WriteError(resp, errBadGateway(messageUnavailable))
			return 0
		}

		if len(manifest) > maxManifestBytes {
			WriteError(resp, errManifestTooLarge())
			return 0
		}

		state.manifest = manifest
		req.Body = io.NopCloser(bytes.NewReader(manifest))
		req.ContentLength = int64(len(manifest))
	}

	ctx := context.WithValue(req.Context(), exchangeKey{}, state)

	if f.cfg.Timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, f.cfg.Timeout)
		defer cancel()
	}

	f.proxy.ServeHTTP(&noInterimWriter{ResponseWriter: resp}, req.WithContext(ctx))

	return state.upstreamStatus
}

func exchangeOf(req *http.Request) *exchange {
	return req.Context().Value(exchangeKey{}).(*exchange)
}

// rewrite builds the outbound request from the classified fields only, and
// drops every header that carries the client's credential or identity.
func (f *Forwarder) rewrite(proxyReq *httputil.ProxyRequest) {
	state := exchangeOf(proxyReq.In)
	target := state.forward.Request.UpstreamURL()

	out := proxyReq.Out
	out.URL = &url.URL{
		Scheme:   f.cfg.Upstream.Scheme,
		Host:     f.cfg.Upstream.Host,
		Path:     target.Path,
		RawQuery: target.RawQuery,
	}
	out.Host = f.cfg.Upstream.Host

	for name := range out.Header {
		if isClientIdentityHeader(name) {
			out.Header.Del(name)
		}
	}
}

func isClientIdentityHeader(name string) bool {
	canonical := http.CanonicalHeaderKey(name)

	switch canonical {
	case "Authorization", "Cookie", "Forwarded", "Via", "X-Real-Ip":
		return true
	}

	return strings.HasPrefix(canonical, "Proxy-") || strings.HasPrefix(canonical, "X-Forwarded-")
}

// modifyResponse removes upstream challenges and points upstream Location
// headers back at the host the client used.
func (f *Forwarder) modifyResponse(resp *http.Response) error {
	resp.Header.Del("Www-Authenticate")

	location := resp.Header.Get("Location")
	if location == "" {
		return nil
	}

	parsed, err := url.Parse(location)
	if err != nil || !parsed.IsAbs() {
		return nil
	}

	if !strings.EqualFold(parsed.Scheme, f.cfg.Upstream.Scheme) ||
		upstream.CanonicalHost(parsed.Scheme, parsed.Host) != upstream.CanonicalHost(f.cfg.Upstream.Scheme, f.cfg.Upstream.Host) {
		return nil
	}

	parsed.Scheme = "https"
	parsed.Host = exchangeOf(resp.Request).clientHost
	resp.Header.Set("Location", parsed.String())

	return nil
}

// Errors the token transport returns for the error handler to answer.
var (
	errAnonymousRefused   = errors.New("upstream refused the anonymous request")
	errCredentialRejected = errors.New("upstream rejected the PAT-backed token")
	errMirrorUnavailable  = errors.New("mirror unavailable")
)

// handleError answers every failure with a fixed message. The underlying
// error goes only to the process log.
func (f *Forwarder) handleError(resp http.ResponseWriter, req *http.Request, err error) {
	var answer *Error

	switch {
	case errors.Is(err, errAnonymousRefused), errors.Is(err, upstream.ErrAnonymousRefused):
		answer = errUnauthorized()
	case errors.Is(err, errCredentialRejected), errors.Is(err, upstream.ErrCredentialRejected):
		answer = errBadGateway(messageCredentialRejected)
	case errors.Is(err, upstream.ErrRealmNotAllowed):
		answer = errBadGateway(messageRealmNotAllowed)
	case errors.Is(err, upstream.ErrPATNotLoaded):
		answer = errNotReady()
	case errors.Is(err, errMirrorUnavailable):
		answer = errBadGateway(messageMirrorUnavailable)
	default:
		f.cfg.Logger.Error("upstream request failed", "method", req.Method, "path", req.URL.EscapedPath(), "error", err.Error())

		answer = errBadGateway(messageUnavailable)
	}

	WriteError(resp, answer)
}

// tokenTransport sets the upstream token on each outbound request and
// does nothing else: it never widens scopes and never follows redirects. It
// resends only after an upstream 401 on a PAT-backed request whose body can
// be replayed.
type tokenTransport struct {
	forwarder *Forwarder
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	state := exchangeOf(req)
	forward := state.forward

	if target := t.forwarder.mirrorFor(forward); target != nil {
		resp, served, err := t.tryMirror(req, state, target)
		if served {
			return resp, err
		}
	}

	kind := upstream.KindAnonymous
	scopes := []string{"repository:" + forward.Request.Name + ":pull"}

	if forward.Credential == CredentialPAT {
		kind = upstream.KindPAT
		scopes = forward.Request.Scopes()
	}

	resp, token, err := t.send(req, kind, scopes)
	if err != nil {
		return nil, err
	}

	if kind == upstream.KindPAT && resp.StatusCode == http.StatusUnauthorized {
		discard(resp)
		t.forwarder.cfg.Tokens.Invalidate(token)

		retry, replayable := t.replay(req, state)
		if !replayable {
			return nil, errCredentialRejected
		}

		if resp, _, err = t.send(retry, kind, scopes); err != nil {
			return nil, err
		}

		if resp.StatusCode == http.StatusUnauthorized {
			discard(resp)
			return nil, errCredentialRejected
		}
	}

	if kind == upstream.KindAnonymous && refusedAnonymously(resp.StatusCode, forward.AnonymousCaller) {
		discard(resp)
		return nil, errAnonymousRefused
	}

	return resp, nil
}

func (t *tokenTransport) send(req *http.Request, kind upstream.Kind, scopes []string) (*http.Response, upstream.Token, error) {
	token, err := t.forwarder.cfg.Tokens.Token(req.Context(), kind, scopes)
	if err != nil {
		return nil, token, err
	}

	req.Header.Set("Authorization", "Bearer "+token.Value)

	started := time.Now()
	resp, err := t.forwarder.cfg.Transport.RoundTrip(req)
	t.forwarder.cfg.Metrics.UpstreamRequest(metrics.TargetUpstream, statusOf(resp), time.Since(started))

	if err != nil {
		return nil, token, err
	}

	exchangeOf(req).upstreamStatus = resp.StatusCode

	return resp, token, nil
}

// mirrorFor returns the mirror that serves the request first, or nil. Only
// manifest and blob downloads and manifest checks go to a mirror: a blob check
// always goes upstream, because a push uses it to decide what to upload and a
// mirror can still hold a blob the upstream has dropped.
func (f *Forwarder) mirrorFor(forward Forward) MirrorClient {
	request := forward.Request

	isPull := request.Action == v1alpha1.ActionPull &&
		((request.Route == RouteManifest && (request.Method == http.MethodGet || request.Method == http.MethodHead)) ||
			(request.Route == RouteBlob && request.Method == http.MethodGet))
	if !isPull {
		return nil
	}

	if forward.Credential == CredentialPAT {
		return f.cfg.PolicyMirror
	}

	return f.cfg.AnonymousMirror
}

// tryMirror sends the request to target. served is false when the mirror
// failed in a way that falls back to the upstream; otherwise resp or err is
// the answer to give.
func (t *tokenTransport) tryMirror(req *http.Request, state *exchange, target MirrorClient) (resp *http.Response, served bool, err error) {
	forward := state.forward
	viaPolicy := forward.Credential == CredentialPAT
	prefix := target.PathPrefix()

	mirrorName, mirrorTarget := metrics.MirrorAnonymous, metrics.TargetAnonymousMirror
	if viaPolicy {
		mirrorName, mirrorTarget = metrics.MirrorPolicy, metrics.TargetPolicyMirror
	}

	out := req.Clone(req.Context())
	out.URL = &url.URL{
		Scheme:   target.URL().Scheme,
		Host:     target.URL().Host,
		Path:     "/v2/" + prefix + "/" + strings.TrimPrefix(req.URL.Path, "/v2/"),
		RawQuery: req.URL.RawQuery,
	}
	out.Host = target.URL().Host
	out.Header.Del("Authorization")

	// failed applies the fallback rule to a mirror failure.
	failed := func(reason string) (*http.Response, bool, error) {
		if target.FallbackToUpstream() {
			t.forwarder.cfg.Metrics.MirrorFallback(mirrorName)
			t.forwarder.cfg.Logger.Warn("mirror failed; falling back to the upstream",
				"mirror", target.URL().String(), "path", out.URL.Path, "reason", reason)

			return nil, false, nil
		}

		t.forwarder.cfg.Logger.Error("mirror failed", "mirror", target.URL().String(), "path", out.URL.Path, "reason", reason)

		return nil, true, errMirrorUnavailable
	}

	started := time.Now()
	resp, err = target.Send(out, "repository:"+prefix+"/"+forward.Request.Name+":pull")
	t.forwarder.cfg.Metrics.UpstreamRequest(mirrorTarget, statusOf(resp), time.Since(started))

	switch {
	case err != nil && req.Context().Err() != nil:
		return nil, true, err
	case errors.Is(err, mirror.ErrCredentialRejected):
		if viaPolicy {
			t.forwarder.cfg.Logger.Error("mirror rejected its credentials", "mirror", target.URL().String())
			return nil, true, errMirrorUnavailable
		}

		return nil, true, errAnonymousRefused
	case err != nil:
		return failed(err.Error())
	}

	state.upstreamStatus = resp.StatusCode

	switch status := resp.StatusCode; {
	case status >= 500 || status == http.StatusNotFound:
		if status == http.StatusNotFound && !target.FallbackToUpstream() {
			break
		}

		discard(resp)

		return failed(http.StatusText(status))
	case viaPolicy && (status == http.StatusUnauthorized || status == http.StatusForbidden):
		// A mirror credential problem: shown, never hidden by a fallback.
		discard(resp)
		t.forwarder.cfg.Logger.Error("mirror refused its credentials", "mirror", target.URL().String(), "status", status)

		return nil, true, errMirrorUnavailable
	}

	if !mapMirrorLocation(resp, out.URL, target, state.clientHost) {
		discard(resp)
		return failed("Location outside the mirror's path prefix")
	}

	if !viaPolicy && refusedAnonymously(resp.StatusCode, forward.AnonymousCaller) {
		discard(resp)
		return nil, true, errAnonymousRefused
	}

	return resp, true, nil
}

// mapMirrorLocation points a Location on the mirror's host back at the client
// host, removing the mirror's path prefix. It reports false for a mirror-host
// Location outside the prefix, which must not reach the client. Locations on
// other hosts (blob storage) are left as they are.
func mapMirrorLocation(resp *http.Response, requestURL *url.URL, target MirrorClient, clientHost string) bool {
	location := resp.Header.Get("Location")
	if location == "" {
		return true
	}

	parsed, err := url.Parse(location)
	if err != nil {
		return false
	}

	resolved := requestURL.ResolveReference(parsed)
	if !target.SameHost(resolved) {
		return true
	}

	rest, found := strings.CutPrefix(resolved.Path, "/v2/"+target.PathPrefix()+"/")
	if !found {
		return false
	}

	mapped := &url.URL{Scheme: "https", Host: clientHost, Path: "/v2/" + rest, RawQuery: resolved.RawQuery}
	resp.Header.Set("Location", mapped.String())

	return true
}

// replay returns a copy of req to resend, if its body can be sent again.
func (t *tokenTransport) replay(req *http.Request, state *exchange) (*http.Request, bool) {
	retry := req.Clone(req.Context())

	switch {
	case state.manifest != nil:
		retry.Body = io.NopCloser(bytes.NewReader(state.manifest))
		retry.ContentLength = int64(len(state.manifest))
	case req.Body == nil || req.Body == http.NoBody:
		retry.Body = req.Body
	default:
		return nil, false
	}

	return retry, true
}

// refusedAnonymously reports whether an anonymous upstream answer means the
// client must authenticate. For a caller with no credential a missing image
// is also answered with a challenge, as GHCR does.
func refusedAnonymously(status int, anonymousCaller bool) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	case http.StatusNotFound:
		return anonymousCaller
	}

	return false
}

// statusOf is the status of resp, or 0 when there is no response.
func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}

	return resp.StatusCode
}

func discard(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// noInterimWriter drops interim (1xx) responses. The reverse proxy relays
// them from the upstream with every header, bypassing modifyResponse, so an
// upstream challenge or Location could reach the client unfiltered.
type noInterimWriter struct {
	http.ResponseWriter
}

func (w *noInterimWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		return
	}

	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *noInterimWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
