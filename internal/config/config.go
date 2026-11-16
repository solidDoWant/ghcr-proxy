// Package config loads and validates the proxy's YAML config file and expands
// its namespace grants.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Action is a registry action a grant can allow.
type Action string

// Registry actions.
const (
	ActionPull   Action = "pull"
	ActionPush   Action = "push"
	ActionDelete Action = "delete"
)

// Config is the validated proxy configuration. Build it with Load or Parse.
type Config struct {
	Upstream       Upstream
	Authentication Authentication
	Grants         Grants
	TLS            TLS
	Listen         Listen
	Server         Server
	Audit          Audit
	Mirrors        Mirrors
	Webhook        Webhook
	// AdminNamespaces are the namespaces whose UpstreamRegistry objects count.
	AdminNamespaces []string
}

// Mirrors holds the optional pull mirrors. A nil mirror is not configured.
type Mirrors struct {
	Policy    *Mirror
	Anonymous *Mirror
}

// Mirror is one pull mirror: a registry that serves GHCR's content under
// PathPrefix. Its credentials come from CredentialsFile only, never from the
// PAT.
type Mirror struct {
	URL                   *url.URL
	PathPrefix            string
	CredentialsFile       string
	CAFile                string
	FallbackToUpstream    bool
	ResponseHeaderTimeout time.Duration
}

// Upstream describes the registry the proxy forwards to.
type Upstream struct {
	URL          *url.URL
	TokenHosts   []string
	PATFile      string
	CAFile       string
	Timeout      time.Duration
	TokenTimeout time.Duration
	// APIURL is the GitHub REST API that deletes by digest go to.
	APIURL *url.URL
	// APITimeout bounds one REST delete, listing included.
	APITimeout time.Duration
}

// Authentication configures ServiceAccount token review.
type Authentication struct {
	Audience      string
	CacheTTL      time.Duration
	ReviewTimeout time.Duration
}

// TLS holds the serving certificate paths.
type TLS struct {
	CertFile string
	KeyFile  string
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
	Upstream struct {
		URL          string   `json:"url"`
		TokenHosts   []string `json:"tokenHosts"`
		PATFile      string   `json:"patFile"`
		CAFile       string   `json:"caFile"`
		Timeout      string   `json:"timeout"`
		TokenTimeout string   `json:"tokenTimeout"`
		APIURL       string   `json:"apiURL"`
		APITimeout   string   `json:"apiTimeout"`
	} `json:"upstream"`
	Authentication struct {
		Audience      string `json:"audience"`
		CacheTTL      string `json:"cacheTTL"`
		ReviewTimeout string `json:"reviewTimeout"`
	} `json:"authentication"`
	Grants []rawGrant `json:"grants"`
	TLS    struct {
		CertFile string `json:"certFile"`
		KeyFile  string `json:"keyFile"`
	} `json:"tls"`
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
	Mirrors struct {
		Policy    *rawMirror `json:"policy"`
		Anonymous *rawMirror `json:"anonymous"`
	} `json:"mirrors"`
	AdminNamespaces []string `json:"adminNamespaces"`
	Webhook         struct {
		Enabled  bool   `json:"enabled"`
		Listen   string `json:"listen"`
		CertFile string `json:"certFile"`
		KeyFile  string `json:"keyFile"`
	} `json:"webhook"`
}

type rawMirror struct {
	URL                   string `json:"url"`
	PathPrefix            string `json:"pathPrefix"`
	CredentialsFile       string `json:"credentialsFile"`
	CAFile                string `json:"caFile"`
	FallbackToUpstream    *bool  `json:"fallbackToUpstream"`
	ResponseHeaderTimeout string `json:"responseHeaderTimeout"`
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

// Parse validates config file contents. It checks that the PAT file exists.
func Parse(data []byte) (*Config, error) {
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

	up := raw.Upstream

	rawURL := orDefault(up.URL, "https://ghcr.io")
	if u, err := url.Parse(rawURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		addf("upstream.url: %q is not an http or https URL", rawURL)
	} else {
		cfg.Upstream.URL = u
	}

	cfg.Upstream.TokenHosts = up.TokenHosts

	cfg.Upstream.PATFile = orDefault(up.PATFile, "/var/run/registrygate/pat/token")
	if info, err := os.Stat(cfg.Upstream.PATFile); err != nil || info.IsDir() {
		addf("upstream.patFile: %q is not a readable file", cfg.Upstream.PATFile)
	}

	cfg.Upstream.CAFile = up.CAFile
	cfg.Upstream.Timeout = duration("upstream.timeout", up.Timeout, "30m", positive, "must be positive")
	cfg.Upstream.TokenTimeout = duration("upstream.tokenTimeout", up.TokenTimeout, "10s", positive, "must be positive")

	rawAPIURL := orDefault(up.APIURL, "https://api.github.com/")
	if u, err := url.Parse(rawAPIURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		addf("upstream.apiURL: %q is not an http or https URL", rawAPIURL)
	} else {
		cfg.Upstream.APIURL = u
	}

	cfg.Upstream.APITimeout = duration("upstream.apiTimeout", up.APITimeout, "30s", positive, "must be positive")

	au := raw.Authentication
	cfg.Authentication.Audience = orDefault(au.Audience, "registrygate")
	cfg.Authentication.CacheTTL = duration("authentication.cacheTTL", au.CacheTTL, "30s",
		func(d time.Duration) bool { return d >= minCacheTTL && d <= maxCacheTTL }, "must be between 1s and 30s")
	cfg.Authentication.ReviewTimeout = duration("authentication.reviewTimeout", au.ReviewTimeout, "5s", positive, "must be positive")

	grants, grantErrs := compileGrants(raw.Grants)
	cfg.Grants = grants

	errs = append(errs, grantErrs...)

	cfg.TLS.CertFile = orDefault(raw.TLS.CertFile, "/var/run/registrygate/tls/tls.crt")
	cfg.TLS.KeyFile = orDefault(raw.TLS.KeyFile, "/var/run/registrygate/tls/tls.key")
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

	mirror := func(field string, rm *rawMirror) *Mirror {
		if rm == nil {
			return nil
		}

		m := &Mirror{
			PathPrefix:         rm.PathPrefix,
			CredentialsFile:    rm.CredentialsFile,
			CAFile:             rm.CAFile,
			FallbackToUpstream: true,
		}

		if u, err := parseMirrorURL(rm.URL); err != nil {
			addf("%s.url: %w", field, err)
		} else {
			m.URL = u
		}

		if err := validatePathPrefix(rm.PathPrefix); err != nil {
			addf("%s.pathPrefix: %w", field, err)
		}

		if rm.CredentialsFile != "" && sameFile(rm.CredentialsFile, cfg.Upstream.PATFile) {
			addf("%s.credentialsFile: %q is the PAT file; a mirror must have its own credentials", field, rm.CredentialsFile)
		}

		if rm.FallbackToUpstream != nil {
			m.FallbackToUpstream = *rm.FallbackToUpstream
		}

		m.ResponseHeaderTimeout = duration(field+".responseHeaderTimeout", rm.ResponseHeaderTimeout, "10s", positive, "must be positive")

		return m
	}
	cfg.Mirrors.Policy = mirror("mirrors.policy", raw.Mirrors.Policy)
	cfg.Mirrors.Anonymous = mirror("mirrors.anonymous", raw.Mirrors.Anonymous)

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
