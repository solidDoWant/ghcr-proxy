// Package server is the proxy's HTTPS front end: the request pipeline (classify, authenticate, check readiness,
// decide, answer or forward), the TLS certificate with reload, the health and metrics listener, and shutdown.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/solidDoWant/ghcr-proxy/internal/audit"
	"github.com/solidDoWant/ghcr-proxy/internal/authn"
	"github.com/solidDoWant/ghcr-proxy/internal/authz"
	"github.com/solidDoWant/ghcr-proxy/internal/metrics"
	"github.com/solidDoWant/ghcr-proxy/internal/registry"
)

// Fixed client-facing messages of the pipeline.
const (
	messageUnauthorized      = "authentication required"
	messageNotReady          = "ghcr-proxy is not ready"
	messageReviewUnavailable = "token review unavailable"
)

// apiVersionHeader tells registry clients the endpoint speaks the v2 protocol, as GHCR's own /v2/ does.
const apiVersionHeader = "Docker-Distribution-API-Version"

// Reviewer turns a ServiceAccount token into an identity.
type Reviewer interface {
	Review(ctx context.Context, token string) (authn.Identity, error)
}

// Decider decides a classified request for an identity.
type Decider interface {
	Decide(identity authn.Identity, request registry.Request) authz.Decision
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
	// Ready reports whether the replica may serve authenticated requests: policies synced and the PAT loaded.
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
		h.Metrics.Request(entry.Action, entry.Decision, entry.Status)
	}()

	h.serve(recorder, req, &entry, recorder)
}

// Audit decision kinds.
const (
	decisionAllowed         = "allowed"
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

	decision := h.Decider.Decide(identity, request)

	switch decision.Outcome {
	case authz.OutcomeVersionCheck:
		entry.Decision = decisionAllowed

		resp.Header().Set("Content-Type", "application/json")
		resp.Header().Set(apiVersionHeader, "registry/2.0")
		resp.WriteHeader(http.StatusOK)
		_, _ = resp.Write([]byte("{}"))
	case authz.OutcomeChallenge:
		entry.Decision = decisionUnauthenticated

		resp.Header().Set(apiVersionHeader, "registry/2.0")
		registry.WriteError(resp, unauthorized())
	case authz.OutcomeDenied:
		entry.Decision = decisionDenied

		registry.WriteError(resp, &registry.Error{Status: http.StatusForbidden, Code: "DENIED", Message: decision.Message})
	case authz.OutcomeForward:
		entry.Decision, entry.Policy = decisionAnonymous, decision.Policy
		if decision.Forward.Credential == registry.CredentialPAT {
			entry.Decision = decisionAllowed
		}

		if mount := decision.Forward.Request.Mount; mount != nil {
			entry.From = mount.From
		}

		entry.Upstream = h.Forwarder.ServeForward(resp, req, decision.Forward)

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
