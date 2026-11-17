// Package server is the proxy's HTTPS front end: the request pipeline (classify, authenticate, check readiness,
// decide, answer or forward), the TLS certificate with reload, the health and metrics listener, and shutdown.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/solidDoWant/registrygate/internal/audit"
	"github.com/solidDoWant/registrygate/internal/authn"
	"github.com/solidDoWant/registrygate/internal/authz"
	"github.com/solidDoWant/registrygate/internal/metrics"
	"github.com/solidDoWant/registrygate/internal/registry"
)

// Fixed client-facing messages of the pipeline.
const (
	messageUnauthorized      = "authentication required"
	messageNotReady          = "registrygate is not ready"
	messageReviewUnavailable = "token review unavailable"
	messageWrongHost         = "Host header does not name the server name's registry"
)

// apiVersionHeader tells registry clients the endpoint speaks the v2 protocol, as GHCR's own /v2/ does.
const apiVersionHeader = "Docker-Distribution-API-Version"

// Reviewer turns a ServiceAccount token into an identity.
type Reviewer interface {
	Review(ctx context.Context, token string) (authn.Identity, error)
}

// Decider resolves a classified request for an identity on the host the client reached.
type Decider interface {
	Decide(host string, identity authn.Identity, request registry.Request) authz.Resolution
}

// Forwarder sends an authorized request upstream and writes the answer.
type Forwarder interface {
	ServeForward(resp http.ResponseWriter, req *http.Request, forward registry.Forward) int
}

// Handler is the registry request pipeline.
type Handler struct {
	Reviewer  Reviewer
	Decider   Decider
	Forwarder Forwarder
	// Ready reports whether the replica may serve authenticated requests: the model has a snapshot with no
	// pending Secret and the upload signing key is loaded.
	Ready  func() bool
	Logger *slog.Logger
	// Audit receives one entry per request; nil turns the audit log off.
	Audit *audit.Logger
	// Metrics counts requests; nil turns it off.
	Metrics *metrics.Metrics
}

// ServeHTTP runs one request through the pipeline: classify (unknown routes are 404 with nothing upstream),
// authenticate, require readiness for authenticated callers, then decide. Anything the proxy can't decide is
// refused; it never falls back to the PAT. Every request writes one audit entry.
func (h *Handler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	started := time.Now()
	recorder := &recordingWriter{ResponseWriter: resp}
	entry := audit.Entry{
		Time: started.UTC(), ClientIP: clientIP(req), Method: req.Method, Path: req.URL.EscapedPath(), Action: "unknown",
	}

	defer func() {
		entry.Status = recorder.Status()
		entry.Bytes = recorder.bytes
		entry.DurationMS = time.Since(started).Milliseconds()

		h.Audit.Log(entry)
		h.Metrics.Request(entry.Upstream, entry.Action, entry.Decision, entry.Status)
	}()

	h.serve(recorder, req, &entry, recorder)
}

// Audit decision kinds.
const (
	decisionAllowed         = "allowed"
	decisionMirror          = "mirror"
	decisionAmbiguous       = "ambiguous"
	decisionAnonymous       = "anonymous"
	decisionDenied          = "denied"
	decisionUnauthenticated = "unauthenticated"
	decisionUnavailable     = "unavailable"
	decisionNotFound        = "notfound"
	decisionInvalid         = "invalid"
)

func (h *Handler) serve(resp http.ResponseWriter, req *http.Request, entry *audit.Entry, recorder *recordingWriter) {
	request, err := registry.Classify(req)
	if err != nil {
		var registryErr *registry.Error
		if !errors.As(err, &registryErr) {
			registryErr = &registry.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "not found"}
		}

		entry.Decision = decisionNotFound
		if registryErr.Status != http.StatusNotFound {
			entry.Decision = decisionInvalid
		}

		registry.WriteError(resp, registryErr)

		return
	}

	entry.Repository = request.Name
	entry.Action = string(request.Action)

	if request.Action == "" {
		entry.Action = "none"
	}

	identity, answer := h.authenticate(req)
	if answer != nil {
		entry.Decision = decisionUnauthenticated
		if answer.Status == http.StatusServiceUnavailable {
			entry.Decision = decisionUnavailable
		}

		registry.WriteError(resp, answer)

		return
	}

	entry.Namespace, entry.SA, entry.Pod = identity.Namespace, identity.ServiceAccount, identity.Pod

	if identity.ServiceAccount != "" && !h.Ready() {
		entry.Decision = decisionUnavailable

		registry.WriteError(resp, notReady())

		return
	}

	entry.Host = serverName(req)

	resolution := h.Decider.Decide(entry.Host, identity, request)

	if resolution.Upstream != nil {
		entry.Upstream = resolution.Upstream.Name().String()
	}

	entry.Policy, entry.Rule = policyAndRuleNames(resolution.Rules)

	if resolution.Upstream != nil && !hostNamesUpstream(req.Host, resolution.Upstream.Object.Spec.Hosts) {
		entry.Decision = decisionInvalid

		registry.WriteError(resp, &registry.Error{Status: http.StatusBadRequest, Code: "UNSUPPORTED", Message: messageWrongHost})

		return
	}

	switch resolution.Result {
	case authz.ResultVersionCheck:
		entry.Decision = decisionAllowed

		resp.Header().Set("Content-Type", "application/json")
		resp.Header().Set(apiVersionHeader, "registry/2.0")
		resp.WriteHeader(http.StatusOK)
		_, _ = resp.Write([]byte("{}"))
	case authz.ResultChallenge:
		entry.Decision = decisionUnauthenticated

		resp.Header().Set(apiVersionHeader, "registry/2.0")
		registry.WriteError(resp, unauthorized())
	case authz.ResultDenied, authz.ResultAmbiguous:
		entry.Decision = decisionDenied
		if resolution.Result == authz.ResultAmbiguous {
			entry.Decision = decisionAmbiguous
		}

		registry.WriteError(resp, &registry.Error{Status: http.StatusForbidden, Code: "DENIED", Message: resolution.Message})
	case authz.ResultNotFound:
		entry.Decision = decisionNotFound

		registry.WriteError(resp, &registry.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "not found"})
	case authz.ResultCredential, authz.ResultMirror, authz.ResultAnonymous:
		forward := registry.Forward{
			Request:         resolution.Request,
			Upstream:        resolution.Upstream,
			Credential:      resolution.Credential,
			Mirror:          resolution.Mirror,
			MirrorRule:      resolution.Result == authz.ResultMirror,
			MirrorFailed:    resolution.MirrorFailed,
			AnonymousCaller: identity.ServiceAccount == "",
		}

		switch resolution.Result {
		case authz.ResultCredential:
			entry.Decision = decisionAllowed
			entry.Credential = resolution.Credential.Name().String()
		case authz.ResultMirror:
			entry.Decision = decisionMirror
		default:
			entry.Decision = decisionAnonymous
		}

		if resolution.Mirror != nil {
			entry.Mirror = resolution.Mirror.Name().String()
		}

		if mount := forward.Request.Mount; mount != nil {
			entry.From = mount.From
		}

		entry.UpstreamStatus = h.Forwarder.ServeForward(resp, req, forward)

		if isManifestWrite(request) {
			entry.Digest = recorder.digest
			if entry.Digest == "" && digest.Digest(request.Reference).Validate() == nil {
				entry.Digest = request.Reference
			}
		}
	default:
		entry.Decision = decisionUnavailable

		registry.WriteError(resp, notReady())
	}
}

// policyAndRuleNames splits the rules (namespace/policy/rule) that decided a request into the policies
// (namespace/policy) and the rule names, each joined by "," without repeats.
func policyAndRuleNames(rules []string) (policies, ruleNames string) {
	var policyList, ruleList []string

	for _, rule := range rules {
		parts := strings.SplitN(rule, "/", 3)
		if len(parts) != 3 {
			continue
		}

		if policy := parts[0] + "/" + parts[1]; !slices.Contains(policyList, policy) {
			policyList = append(policyList, policy)
		}

		if !slices.Contains(ruleList, parts[2]) {
			ruleList = append(ruleList, parts[2])
		}
	}

	return strings.Join(policyList, ","), strings.Join(ruleList, ",")
}

// serverName is the TLS server name the client asked for, which decides the upstream. A request without TLS
// names no host.
func serverName(req *http.Request) string {
	if req.TLS == nil {
		return ""
	}

	return req.TLS.ServerName
}

// hostNamesUpstream reports whether the Host header names one of the upstream's hosts, with or without the
// default HTTPS port. A request whose Host disagrees with the server name is refused, so an intermediary that
// routes by Host can't be steered to another upstream than the one decided on.
func hostNamesUpstream(host string, hosts []string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ":443")

	return slices.Contains(hosts, host)
}

func isManifestWrite(request registry.Request) bool {
	return request.Route == registry.RouteManifest && (request.Method == http.MethodPut || request.Method == http.MethodDelete)
}

// clientIP is the address of the TCP peer. Forwarding headers are the client's claim, so they are not used.
func clientIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}

	return host
}

// recordingWriter remembers what the client got: the status, the body size and the upstream's digest header.
type recordingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	digest string
}

// Status is the status the client got, 200 when the handler wrote only a body.
func (w *recordingWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}

	return w.status
}

func (w *recordingWriter) WriteHeader(status int) {
	if w.status == 0 && (status >= 200 || status == http.StatusSwitchingProtocols) {
		w.status = status
		w.digest = w.Header().Get("Docker-Content-Digest")
	}

	w.ResponseWriter.WriteHeader(status)
}

func (w *recordingWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
		w.digest = w.Header().Get("Docker-Content-Digest")
	}

	n, err := w.ResponseWriter.Write(data)
	w.bytes += int64(n)

	return n, err
}

// Unwrap lets http.ResponseController reach the real writer (flushing streamed bodies).
func (w *recordingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// authenticate returns the caller's identity, the zero Identity for a caller that sent no credential, or the
// answer to give when the credential can't be accepted.
func (h *Handler) authenticate(req *http.Request) (authn.Identity, *registry.Error) {
	values := req.Header.Values("Authorization")
	if len(values) == 0 {
		return authn.Identity{}, nil
	}

	if len(values) > 1 {
		return authn.Identity{}, unauthorized()
	}

	// BasicAuth accepts only the Basic scheme and valid base64; every other scheme is refused here.
	_, token, ok := req.BasicAuth()
	if !ok || token == "" {
		return authn.Identity{}, unauthorized()
	}

	identity, err := h.Reviewer.Review(req.Context(), token)

	switch {
	case err == nil && identity.ServiceAccount != "":
		return identity, nil
	case err == nil, errors.Is(err, authn.ErrRejected):
		return authn.Identity{}, unauthorized()
	default:
		h.logger().Error("token review failed", "error", err.Error())

		return authn.Identity{}, &registry.Error{
			Status: http.StatusServiceUnavailable, Code: "UNAVAILABLE", Message: messageReviewUnavailable,
		}
	}
}

func (h *Handler) logger() *slog.Logger {
	if h.Logger == nil {
		return slog.Default()
	}

	return h.Logger
}

func unauthorized() *registry.Error {
	return &registry.Error{Status: http.StatusUnauthorized, Code: "UNAUTHORIZED", Message: messageUnauthorized}
}

func notReady() *registry.Error {
	return &registry.Error{Status: http.StatusServiceUnavailable, Code: "UNAVAILABLE", Message: messageNotReady}
}
