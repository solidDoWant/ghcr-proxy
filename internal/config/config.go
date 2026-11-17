// Package config loads and validates the proxy's YAML config file: the server's own settings. Upstreams,
// credentials, mirrors and access come from the custom resources, never from this file.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Config is the validated proxy configuration. Build it with Load or Parse.
type Config struct {
	Authentication Authentication
	Listen         Listen
	Server         Server
	Audit          Audit
	Webhook        Webhook
	// AdminNamespaces are the namespaces whose UpstreamRegistry objects count.
	AdminNamespaces []string
}

// Authentication configures ServiceAccount token review.
type Authentication struct {
	Audience      string
	CacheTTL      time.Duration
	ReviewTimeout time.Duration
}

// Webhook configures the validating admission webhook.
type Webhook struct {
	Enabled  bool
	Listen   string
	CertFile string
	KeyFile  string
}

// Listen holds the listener addresses.
type Listen struct {
	HTTPS  string
	Health string
}

// Server holds HTTP server limits.
type Server struct {
	ReadHeaderTimeout   time.Duration
	IdleTimeout         time.Duration
	MaxHeaderBytes      int
	ShutdownGracePeriod time.Duration
}

// Audit configures the audit log.
type Audit struct {
	Enabled bool
}

const (
	minCacheTTL = time.Second
	maxCacheTTL = 30 * time.Second
)

type rawConfig struct {
	Authentication struct {
		Audience      string `json:"audience"`
		CacheTTL      string `json:"cacheTTL"`
		ReviewTimeout string `json:"reviewTimeout"`
	} `json:"authentication"`
	Listen struct {
		HTTPS  string `json:"https"`
		Health string `json:"health"`
	} `json:"listen"`
	Server struct {
		ReadHeaderTimeout   string `json:"readHeaderTimeout"`
		IdleTimeout         string `json:"idleTimeout"`
		MaxHeaderBytes      *int   `json:"maxHeaderBytes"`
		ShutdownGracePeriod string `json:"shutdownGracePeriod"`
	} `json:"server"`
	Audit struct {
		Enabled *bool `json:"enabled"`
	} `json:"audit"`
	AdminNamespaces []string `json:"adminNamespaces"`
	Webhook         struct {
		Enabled  bool   `json:"enabled"`
		Listen   string `json:"listen"`
		CertFile string `json:"certFile"`
		KeyFile  string `json:"keyFile"`
	} `json:"webhook"`
}

// Load reads and validates the config file at path. The error lists every
// problem found.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	return Parse(data)
}

// removedSections are sections of earlier versions whose settings are custom resources now.
var removedSections = []string{"upstream", "grants", "mirrors", "tls"}

// Parse validates config file contents.
func Parse(data []byte) (*Config, error) {
	var sections map[string]any
	if err := yaml.Unmarshal(data, &sections); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	for _, section := range removedSections {
		if _, present := sections[section]; present {
			return nil, fmt.Errorf("parse config: the %q section is no longer read: upstreams, credentials, mirrors "+
				"and access are set with the UpstreamRegistry, UpstreamCredential, RegistryMirror and "+
				"RegistryAccessPolicy custom resources", section)
		}
	}

	var raw rawConfig
	if err := yaml.UnmarshalStrict(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	var errs []error

	addf := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	duration := func(field, value, def string, check func(time.Duration) bool, constraint string) time.Duration {
		if value == "" {
			value = def
		}

		d, err := time.ParseDuration(value)
		if err != nil {
			addf("%s: invalid duration %q", field, value)
			return 0
		}

		if !check(d) {
			addf("%s: %s, got %s", field, constraint, value)
			return 0
		}

		return d
	}
	positive := func(d time.Duration) bool { return d > 0 }
	orDefault := func(value, def string) string {
		if value == "" {
			return def
		}

		return value
	}

	cfg := &Config{}

	au := raw.Authentication
	cfg.Authentication.Audience = orDefault(au.Audience, "registrygate")
	cfg.Authentication.CacheTTL = duration("authentication.cacheTTL", au.CacheTTL, "30s",
		func(d time.Duration) bool { return d >= minCacheTTL && d <= maxCacheTTL }, "must be between 1s and 30s")
	cfg.Authentication.ReviewTimeout = duration("authentication.reviewTimeout", au.ReviewTimeout, "5s", positive, "must be positive")

	cfg.Listen.HTTPS = orDefault(raw.Listen.HTTPS, ":8443")
	cfg.Listen.Health = orDefault(raw.Listen.Health, ":8080")

	cfg.AdminNamespaces = raw.AdminNamespaces
	cfg.Webhook.Enabled = raw.Webhook.Enabled
	cfg.Webhook.Listen = orDefault(raw.Webhook.Listen, ":9443")
	cfg.Webhook.CertFile = orDefault(raw.Webhook.CertFile, "/var/run/registrygate/webhook-tls/tls.crt")
	cfg.Webhook.KeyFile = orDefault(raw.Webhook.KeyFile, "/var/run/registrygate/webhook-tls/tls.key")

	sv := raw.Server
	cfg.Server.ReadHeaderTimeout = duration("server.readHeaderTimeout", sv.ReadHeaderTimeout, "10s", positive, "must be positive")
	cfg.Server.IdleTimeout = duration("server.idleTimeout", sv.IdleTimeout, "120s", positive, "must be positive")
	cfg.Server.ShutdownGracePeriod = duration("server.shutdownGracePeriod", sv.ShutdownGracePeriod, "10m",
		func(d time.Duration) bool { return d >= 0 }, "must not be negative")

	cfg.Server.MaxHeaderBytes = 65536
	if sv.MaxHeaderBytes != nil {
		if *sv.MaxHeaderBytes <= 0 {
			addf("server.maxHeaderBytes: must be positive, got %d", *sv.MaxHeaderBytes)
		}

		cfg.Server.MaxHeaderBytes = *sv.MaxHeaderBytes
	}

	cfg.Audit.Enabled = true
	if raw.Audit.Enabled != nil {
		cfg.Audit.Enabled = *raw.Audit.Enabled
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid config:\n%w", joinLines(errs))
	}

	return cfg, nil
}

type lineErrors []error

func (l lineErrors) Error() string {
	lines := make([]string, len(l))
	for i, err := range l {
		lines[i] = "  - " + err.Error()
	}

	return strings.Join(lines, "\n")
}

func (l lineErrors) Unwrap() []error { return l }

func joinLines(errs []error) error { return errors.Join(lineErrors(errs)...) }
