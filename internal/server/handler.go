// Package server is the proxy's HTTPS front end: the request pipeline (classify, authenticate, check readiness,
// decide, answer or forward), the TLS certificate with reload, the health and metrics listener, and shutdown.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/solidDoWant/ghcr-proxy/internal/authn"
	"github.com/solidDoWant/ghcr-proxy/internal/authz"
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
}

// ServeHTTP runs one request through the pipeline: classify (unknown routes are 404 with nothing upstream),
// authenticate, require readiness for authenticated callers, then decide. Anything the proxy can't decide is
// refused; it never falls back to the PAT.
func (h *Handler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	request, err := registry.Classify(req)
	if err != nil {
		var registryErr *registry.Error
		if !errors.As(err, &registryErr) {
			registryErr = &registry.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "not found"}
		}

		registry.WriteError(resp, registryErr)

		return
	}

	identity, answer := h.authenticate(req)
	if answer != nil {
		registry.WriteError(resp, answer)
		return
	}

	if identity.ServiceAccount != "" && !h.Ready() {
		registry.WriteError(resp, notReady())
		return
	}

	decision := h.Decider.Decide(identity, request)

	switch decision.Outcome {
	case authz.OutcomeVersionCheck:
		resp.Header().Set("Content-Type", "application/json")
		resp.Header().Set(apiVersionHeader, "registry/2.0")
		resp.WriteHeader(http.StatusOK)
		_, _ = resp.Write([]byte("{}"))
	case authz.OutcomeChallenge:
		resp.Header().Set(apiVersionHeader, "registry/2.0")
		registry.WriteError(resp, unauthorized())
	case authz.OutcomeDenied:
		registry.WriteError(resp, &registry.Error{Status: http.StatusForbidden, Code: "DENIED", Message: decision.Message})
	case authz.OutcomeForward:
		h.Forwarder.ServeForward(resp, req, decision.Forward)
	default:
		registry.WriteError(resp, notReady())
	}
}

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
