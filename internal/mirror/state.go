// Package mirror holds what the proxy needs to reach a pull mirror. Its
// credentials are a type of their own, read only from the mirror's Secret;
// this package never sees an upstream credential.
package mirror

import (
	"crypto/x509"
	"log/slog"
	"net/url"
	"time"
)

const redacted = "[redacted]"

// Credentials are a mirror's own user name and password. They are never sent
// anywhere but the mirror's token endpoint.
type Credentials struct {
	Username string
	Password string
}

// String hides the credentials so they can't reach a log or an error by
// accident.
func (Credentials) String() string { return redacted }

// GoString hides the credentials from %#v.
func (Credentials) GoString() string { return redacted }

// LogValue hides the credentials from slog.
func (Credentials) LogValue() slog.Value { return slog.StringValue(redacted) }

// State is the mirror's credentials and CA.
type State struct {
	// Credentials is nil when the mirror is reached anonymously.
	Credentials *Credentials
	// RootCAs is nil when only the system roots are trusted.
	RootCAs *x509.CertPool
	// Generation changes whenever the credentials change.
	Generation uint64
}

// Settings are a mirror's endpoint and behaviour.
type Settings struct {
	URL                   *url.URL
	PathPrefix            string
	FallbackToUpstream    bool
	ResponseHeaderTimeout time.Duration
	// TokenRealmHosts are hosts (host or host:port) allowed as the token
	// endpoint besides the URL's host, on the URL's scheme.
	TokenRealmHosts []string
	// AliasHosts are other names of the mirror; a realm or Location on one is
	// treated as on URL.
	AliasHosts []string
}
