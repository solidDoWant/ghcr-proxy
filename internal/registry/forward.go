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

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/metrics"
	"github.com/solidDoWant/registrygate/internal/mirror"
	"github.com/solidDoWant/registrygate/internal/model"
	"github.com/solidDoWant/registrygate/internal/uploadtoken"
	"github.com/solidDoWant/registrygate/internal/upstream"
)

// maxManifestBytes is the largest manifest the proxy buffers for a push.
const maxManifestBytes = 4 << 20

// Forward is what the forwarder needs to know about a resolved request.
type Forward struct {
	Request Request
	// Upstream is the upstream serving the host.
	Upstream *model.Upstream
	// Credential is the credential the request goes upstream with; nil sends
	// it with no credential.
	Credential *model.Credential
	// Mirror serves manifest and blob downloads first; nil for none.
	Mirror *model.Mirror
	// MirrorRule is set when Mirror is a rule's mirror rather than the
	// upstream's default mirror: a 401 or 403 from it is then a failed
	// mirror (502), never a challenge.
	MirrorRule bool
	// MirrorFailed is set when the rule's mirror isn't ready: its pulls take
	// the fallback rule without contacting it.
	MirrorFailed bool
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

// ForwarderConfig configures a Forwarder.
type ForwarderConfig struct {
	// Upstreams holds the client of each upstream.
	Upstreams *upstream.Clients
	// RuleMirrors and DefaultMirrors hold the clients of rules' mirrors and
	// of upstreams' default mirrors. They are separate caches so their token
	// fetches are counted apart.
	RuleMirrors    *mirror.Cache
	DefaultMirrors *mirror.Cache
	// UploadTokens returns the codec for upload session tokens, or nil while
	// the signing key isn't loaded.
	UploadTokens func() *uploadtoken.Codec
	Logger       *slog.Logger
	// Metrics counts upstream requests and mirror fallbacks; nil turns it off.
	Metrics *metrics.Metrics
}

// Forwarder sends resolved requests to the upstream or a mirror with that
// endpoint's own credential and passes the answer back.
type Forwarder struct {
	cfg   ForwarderConfig
	proxy *httputil.ReverseProxy
}

// NewForwarder returns a Forwarder for cfg.
func NewForwarder(cfg ForwarderConfig) *Forwarder {
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
	client     *upstream.Client
	codec      *uploadtoken.Codec
	target     *url.URL
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
	client, err := f.cfg.Upstreams.Get(forward.Upstream)
	if err != nil {
		f.cfg.Logger.Error("upstream client not built", "upstream", forward.Upstream.Name().String(), "error", err.Error())
		WriteError(resp, errBadGateway(messageUnavailable))

		return 0
	}

	state := &exchange{forward: forward, client: client, clientHost: req.Host, target: forward.Request.UpstreamURL()}

	if f.cfg.UploadTokens != nil {
		state.codec = f.cfg.UploadTokens()
	}

	if forward.Request.Route == RouteUploadSession {
		target, answer := state.openSession()
		if answer != nil {
			WriteError(resp, answer)
			return 0
		}

		state.target = target
	}

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

	f.proxy.ServeHTTP(&noInterimWriter{ResponseWriter: resp}, req.WithContext(ctx))

	return state.upstreamStatus
}

// openSession turns the request's upload session token into the upstream's
// session URL, with the client's query added. A token that wasn't issued for
// this upstream and repository is 404 and nothing is sent.
func (state *exchange) openSession() (*url.URL, *Error) {
	if state.codec == nil {
		return nil, errNotReady()
	}

	request := state.forward.Request

	target, err := state.codec.Open(string(state.forward.Upstream.Object.UID), request.Name, request.Session)
	if err != nil {
		return nil, errBlobUploadUnknown()
	}

	if err := uploadtoken.AddClientQuery(target, request.Method, request.RawQuery); err != nil {
		return nil, errSessionQuery()
	}

	return target, nil
}

func exchangeOf(req *http.Request) *exchange {
	return req.Context().Value(exchangeKey{}).(*exchange)
}

// rewrite builds the outbound request from the classified fields only, and
// drops every header that carries the client's credential or identity.
func (f *Forwarder) rewrite(proxyReq *httputil.ProxyRequest) {
	state := exchangeOf(proxyReq.In)
	base := state.client.URL()

	out := proxyReq.Out
	out.URL = &url.URL{
		Scheme:   base.Scheme,
		Host:     base.Host,
		Path:     state.target.Path,
		RawQuery: state.target.RawQuery,
	}
	out.Host = base.Host

	if state.manifest != nil {
		manifest := state.manifest
		out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(manifest)), nil }
	}

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

// modifyResponse removes upstream challenges, turns the upload location of a
// 202 answer into a signed session token, and points other upstream Location
// headers back at the host the client used.
func (f *Forwarder) modifyResponse(resp *http.Response) error {
	resp.Header.Del("Www-Authenticate")

	state := exchangeOf(resp.Request)

	location := resp.Header.Get("Location")
	if location == "" {
		return nil
	}

	// A Location on an alias host is handled as if it were on the upstream's url.
	if parsed, err := url.Parse(location); err == nil {
		if moved, ok := state.client.OntoURL(parsed); ok {
			location = moved.String()
			resp.Header.Set("Location", location)
		}
	}

	route := state.forward.Request.Route
	if resp.StatusCode == http.StatusAccepted && resp.Request.URL.Host == state.client.URL().Host &&
		(route == RouteUploadStart || route == RouteUploadSession) {
		return state.issueSession(resp, location)
	}

	parsed, err := url.Parse(location)
	if err != nil || !parsed.IsAbs() {
		return nil
	}

	base := state.client.URL()
	if !strings.EqualFold(parsed.Scheme, base.Scheme) ||
		upstream.CanonicalHost(parsed.Scheme, parsed.Host) != upstream.CanonicalHost(base.Scheme, base.Host) {
		return nil
	}

	parsed.Scheme = "https"
	parsed.Host = state.clientHost
	resp.Header.Set("Location", parsed.String())

	return nil
}

// issueSession replaces the upstream's upload location with the client's
// session token URL.
func (state *exchange) issueSession(resp *http.Response, location string) error {
	if state.codec == nil {
		discard(resp)
		return errUploadUnusable
	}

	issued, err := state.codec.Issue(string(state.forward.Upstream.Object.UID), state.forward.Request.Name, resp.Request.URL, location)
	if err != nil {
		discard(resp)
		return errUploadUnusable
	}

	resp.Header.Set("Location", issued)

	return nil
}

// Errors the token transport returns for the error handler to answer.
var (
	errAnonymousRefused   = errors.New("upstream refused the anonymous request")
	errCredentialRejected = errors.New("upstream rejected the credential")
	errMirrorUnavailable  = errors.New("mirror unavailable")
	errUploadUnusable     = errors.New("upstream returned an unusable upload location")
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
	case errors.Is(err, errMirrorUnavailable):
		answer = errBadGateway(messageMirrorUnavailable)
	case errors.Is(err, errUploadUnusable):
		answer = errBadGateway(messageUploadUnusable)
	default:
		f.cfg.Logger.Error("upstream request failed", "method", req.Method, "path", req.URL.EscapedPath(), "error", err.Error())

		answer = errBadGateway(messageUnavailable)
	}

	WriteError(resp, answer)
}

// tokenTransport sends each outbound request to a mirror or to the upstream
// with that endpoint's own credential and nothing else: it never widens
// scopes and never follows redirects.
type tokenTransport struct {
	forwarder *Forwarder
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	state := exchangeOf(req)
	forward := state.forward

	if servesFromMirror(forward) {
		resp, served, err := t.tryMirror(req, state)
		if served {
			return resp, err
		}
	}

	var cred upstream.Credential

	scopes := []string{"repository:" + forward.Request.Name + ":pull"}

	if forward.Credential != nil {
		cred = upstream.Credential{
			ID:       string(forward.Credential.Object.UID),
			Version:  forward.Credential.SecretResourceVersion,
			Username: forward.Credential.Credentials.Username,
			Password: forward.Credential.Credentials.Password,
		}
		scopes = forward.Request.Scopes()
	}

	started := time.Now()
	resp, err := state.client.Do(req, cred, scopes)
	t.forwarder.cfg.Metrics.UpstreamRequest(upstreamName(forward), metrics.TargetUpstream, statusOf(resp), time.Since(started))

	if err != nil {
		return nil, err
	}

	state.upstreamStatus = resp.StatusCode

	if !cred.Anonymous() && resp.StatusCode == http.StatusUnauthorized {
		discard(resp)
		return nil, errCredentialRejected
	}

	if cred.Anonymous() && refusedAnonymously(resp.StatusCode, forward.AnonymousCaller) {
		discard(resp)
		return nil, errAnonymousRefused
	}

	return resp, nil
}

// servesFromMirror reports whether the request goes to a mirror first. Only
// manifest and blob downloads and manifest checks do: a blob check always
// goes upstream, because a push uses it to decide what to upload and a
// mirror can still hold a blob the upstream has dropped.
func servesFromMirror(forward Forward) bool {
	request := forward.Request

	return forward.Mirror != nil && forward.Credential == nil && request.Action == v1alpha1.ActionPull &&
		((request.Route == RouteManifest && (request.Method == http.MethodGet || request.Method == http.MethodHead)) ||
			(request.Route == RouteBlob && request.Method == http.MethodGet))
}

// tryMirror sends the request to the forward's mirror. served is false when
// the mirror failed in a way that falls back to the upstream; otherwise resp
// or err is the answer to give.
func (t *tokenTransport) tryMirror(req *http.Request, state *exchange) (resp *http.Response, served bool, err error) {
	forward := state.forward
	viaRule := forward.MirrorRule
	logger := t.forwarder.cfg.Logger
	mirrorName := forward.Mirror.Name().String()

	cache := t.forwarder.cfg.DefaultMirrors
	if viaRule {
		cache = t.forwarder.cfg.RuleMirrors
	}

	// failed applies the fallback rule to a mirror failure.
	failed := func(reason string) (*http.Response, bool, error) {
		if forward.Mirror.FallbackToUpstream {
			t.forwarder.cfg.Metrics.MirrorFallback(upstreamName(forward))
			logger.Warn("mirror failed; falling back to the upstream", "mirror", mirrorName, "reason", reason)

			return nil, false, nil
		}

		logger.Error("mirror failed", "mirror", mirrorName, "reason", reason)

		return nil, true, errMirrorUnavailable
	}

	if forward.MirrorFailed {
		return failed("mirror not ready")
	}

	target, err := cache.Client(forward.Mirror)
	if err != nil {
		return failed(err.Error())
	}

	out := req.Clone(req.Context())
	out.URL = target.RequestURL(req.URL.Path, req.URL.RawQuery)
	out.Host = out.URL.Host
	out.Header.Del("Authorization")

	started := time.Now()
	resp, err = target.Send(out, "repository:"+target.PathPrefix()+"/"+forward.Request.Name+":pull")
	t.forwarder.cfg.Metrics.UpstreamRequest(upstreamName(forward), metrics.TargetMirror, statusOf(resp), time.Since(started))

	switch {
	case err != nil && req.Context().Err() != nil:
		return nil, true, err
	case errors.Is(err, mirror.ErrCredentialRejected):
		if viaRule {
			logger.Error("mirror rejected its credentials", "mirror", mirrorName)
			return nil, true, errMirrorUnavailable
		}

		return nil, true, errAnonymousRefused
	case err != nil:
		return failed(err.Error())
	}

	state.upstreamStatus = resp.StatusCode

	switch status := resp.StatusCode; {
	case status >= 500 || status == http.StatusNotFound:
		if status == http.StatusNotFound && !forward.Mirror.FallbackToUpstream {
			break
		}

		discard(resp)

		return failed(http.StatusText(status))
	case viaRule && (status == http.StatusUnauthorized || status == http.StatusForbidden):
		// A mirror credential problem: shown, never hidden by a fallback.
		discard(resp)
		logger.Error("mirror refused its credentials", "mirror", mirrorName, "status", status)

		return nil, true, errMirrorUnavailable
	}

	if !mapMirrorLocation(resp, out.URL, target, state.clientHost) {
		discard(resp)
		return failed("Location outside the mirror's path prefix")
	}

	if !viaRule && refusedAnonymously(resp.StatusCode, forward.AnonymousCaller) {
		discard(resp)
		return nil, true, errAnonymousRefused
	}

	return resp, true, nil
}

// mapMirrorLocation points a Location on the mirror's host (or one of its
// alias hosts) back at the client
// host, turning `<url path>/v2/<pathPrefix>/` back into /v2/. It reports false
// for a mirror-host Location outside that prefix, which must not reach the
// client. Locations on other hosts (blob storage) are left as they are.
func mapMirrorLocation(resp *http.Response, requestURL *url.URL, target *mirror.Client, clientHost string) bool {
	location := resp.Header.Get("Location")
	if location == "" {
		return true
	}

	parsed, err := url.Parse(location)
	if err != nil {
		return false
	}

	resolved := requestURL.ResolveReference(parsed)
	if moved, ok := target.OntoURL(resolved); ok {
		resolved = moved
	}

	if !target.SameHost(resolved) {
		return true
	}

	prefix := strings.TrimSuffix(target.URL().Path, "/") + "/v2/" + target.PathPrefix() + "/"

	rest, found := strings.CutPrefix(resolved.Path, prefix)
	if !found {
		return false
	}

	mapped := &url.URL{Scheme: "https", Host: clientHost, Path: "/v2/" + rest, RawQuery: resolved.RawQuery}
	resp.Header.Set("Location", mapped.String())

	return true
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

// upstreamName is the forward's upstream as "<namespace>/<name>", the metrics' upstream label.
func upstreamName(forward Forward) string {
	if forward.Upstream == nil {
		return metrics.NoUpstream
	}

	return forward.Upstream.Name().String()
}
