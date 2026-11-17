// Package upstream sends requests to an UpstreamRegistry, authenticated as its challenge asks.
package upstream

import (
	"errors"
	"strings"
	"time"

	"github.com/solidDoWant/registrygate/internal/metrics"
)

// Failures of a token fetch. Each maps to one fixed client answer; the
// underlying library error goes only to the process log.
var (
	// ErrCredentialRejected is the token endpoint refusing the credential.
	ErrCredentialRejected = errors.New("upstream rejected the proxy's credential")
	// ErrAnonymousRefused is the token endpoint refusing an anonymous token.
	ErrAnonymousRefused = errors.New("upstream refused an anonymous token")
	// ErrRealmNotAllowed is a token realm outside the upstream's host and its tokenRealmHosts.
	ErrRealmNotAllowed = errors.New("upstream token endpoint not allowed")
	// ErrUnavailable is the upstream or its token endpoint failing otherwise.
	ErrUnavailable = errors.New("upstream unavailable")
)

const (
	// maxCachedTokens bounds the token cache.
	maxCachedTokens = 4096
	// expiryMargin is how long before its expiry a token stops being reused.
	expiryMargin = 30 * time.Second
	// defaultTokenLifetime is assumed when the token endpoint gives none.
	defaultTokenLifetime = 60 * time.Second
)

// tokenFetchResult maps a failed fetch to the metric's result label: the upstream refusing a credential is
// rejected; anything else is an error.
func tokenFetchResult(err error) string {
	if errors.Is(err, ErrCredentialRejected) || errors.Is(err, ErrAnonymousRefused) {
		return metrics.ResultRejected
	}

	return metrics.ResultError
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
