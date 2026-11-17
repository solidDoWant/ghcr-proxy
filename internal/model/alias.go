package model

import (
	"net/url"
	"strings"
)

// OntoURL returns a copy of target moved onto base's scheme and host when
// target is an http or https URL on one of aliases, and false otherwise. An
// alias without a port matches the default port of target's scheme. It lets
// a token realm or Location on another name of a registry (Harbor's
// externalURL, say) reach the registry through the address the proxy uses,
// so nothing is ever sent to the alias itself.
func OntoURL(target, base *url.URL, aliases []string) (*url.URL, bool) {
	if target == nil || base == nil || target.Host == "" || target.User != nil {
		return nil, false
	}

	scheme := strings.ToLower(target.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, false
	}

	host := CanonicalHost(scheme, target.Host)

	for _, alias := range aliases {
		if host == CanonicalHost(scheme, alias) {
			moved := *target
			moved.Scheme, moved.Host = base.Scheme, base.Host

			return &moved, true
		}
	}

	return nil, false
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
