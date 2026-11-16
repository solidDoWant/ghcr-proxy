package server

import (
	"crypto/tls"
	"errors"
	"strings"

	"github.com/solidDoWant/registrygate/internal/model"
)

var (
	errNoServerName = errors.New("TLS handshake has no server name")
	errUnknownHost  = errors.New("no ready upstream serves the requested server name")
	errNoSnapshot   = errors.New("registrygate has no snapshot yet")
)

// SNICertificates returns a server TLS configuration that picks the serving certificate by server name from the
// snapshot current at the time of each handshake, so a renewed certificate applies to new connections. A handshake
// with no server name, or for a host no ready upstream serves (including a host in conflict), fails.
func SNICertificates(snapshot func() *model.Snapshot) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return certificateFor(snapshot(), hello.ServerName)
		},
	}
}

func certificateFor(snapshot *model.Snapshot, serverName string) (*tls.Certificate, error) {
	if serverName == "" {
		return nil, errNoServerName
	}

	if snapshot == nil {
		return nil, errNoSnapshot
	}

	upstream := snapshot.Host(strings.ToLower(serverName))
	if upstream == nil || upstream.Certificate == nil {
		return nil, errUnknownHost
	}

	return upstream.Certificate, nil
}
