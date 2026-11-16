// Package model turns registrygate's custom resources, the Namespaces they
// admit and the Secrets they reference into one immutable Snapshot. Serving,
// the status controller and the webhook all read the snapshot, so status
// always says exactly what serving does. No other package interprets the
// objects.
package model

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/url"

	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/glob"
	"github.com/solidDoWant/registrygate/internal/secrets"
)

const redacted = "[redacted]"

// Status is the computed readiness of one object or rule. When Pending is
// true a Secret it needs (directly or through a reference) hasn't loaded
// yet: it has no readiness, and Ready, Reason and Message are unset.
type Status struct {
	Pending bool
	Ready   bool
	// Reason is one of the v1alpha1 Reason constants.
	Reason  string
	Message string
	// ObservedGeneration is the generation of the object the status was
	// computed from.
	ObservedGeneration int64
}

// Credentials are a username and password read from a Secret. They hide
// their values from fmt and slog.
type Credentials struct {
	Username string
	Password string
}

// String hides the values.
func (Credentials) String() string { return redacted }

// GoString hides the values from %#v.
func (Credentials) GoString() string { return redacted }

// LogValue hides the values from slog.
func (Credentials) LogValue() slog.Value { return slog.StringValue(redacted) }

// Upstream is an UpstreamRegistry from an admin namespace.
type Upstream struct {
	Object *v1alpha1.UpstreamRegistry
	Status Status
	// URL is the parsed spec.url; nil when the spec is invalid.
	URL *url.URL
	// Certificate is the serving key pair; set when the upstream is ready.
	Certificate *tls.Certificate
	// CertificateResourceVersion is the serving Secret's resource version.
	CertificateResourceVersion string
	// CAs trusts the upstream's server; nil means the system roots.
	CAs *x509.CertPool
	// CAResourceVersion is the CA Secret's resource version, empty without one.
	CAResourceVersion string
	// DefaultMirror is the usable default mirror, or nil.
	DefaultMirror *Mirror

	admission admission
}

// Name returns the upstream's namespace and name.
func (u *Upstream) Name() types.NamespacedName { return nameOf(u.Object.Namespace, u.Object.Name) }

// Credential is an UpstreamCredential.
type Credential struct {
	Object *v1alpha1.UpstreamCredential
	Status Status
	// Upstream is the upstream the credential refers to, when it exists.
	Upstream *Upstream
	// Credentials are set when the credential is ready.
	Credentials Credentials
	// SecretResourceVersion is the Secret's resource version.
	SecretResourceVersion string

	ceiling   []ceilingEntry
	admission admission
}

// Name returns the credential's namespace and name.
func (c *Credential) Name() types.NamespacedName {
	return nameOf(c.Object.Namespace, c.Object.Name)
}

// Mirror is a RegistryMirror.
type Mirror struct {
	Object *v1alpha1.RegistryMirror
	Status Status
	// Upstream is the upstream the mirror refers to, when it exists.
	Upstream *Upstream
	// URL is the parsed spec.url; nil when the spec is invalid.
	URL *url.URL
	// Credentials are the mirror's username and password; nil without a
	// secretRef.
	Credentials *Credentials
	// SecretResourceVersion is the credentials Secret's resource version.
	SecretResourceVersion string
	// CAs trusts the mirror's server; nil means the system roots.
	CAs *x509.CertPool
	// CAResourceVersion is the CA Secret's resource version.
	CAResourceVersion string
	// FallbackToUpstream is spec.fallbackToUpstream with its default.
	FallbackToUpstream bool

	admission admission
}

// Name returns the mirror's namespace and name.
func (m *Mirror) Name() types.NamespacedName { return nameOf(m.Object.Namespace, m.Object.Name) }

// Policy is a RegistryAccessPolicy.
type Policy struct {
	Object *v1alpha1.RegistryAccessPolicy
	// Status is the policy's Ready condition.
	Status Status
	// Rules has one entry per spec rule, in spec order.
	Rules []*Rule
}

// Name returns the policy's namespace and name.
func (p *Policy) Name() types.NamespacedName { return nameOf(p.Object.Namespace, p.Object.Name) }

// Rule is one rule of a policy and its Accepted status.
type Rule struct {
	Policy *Policy
	Name   string
	Status Status
	// Upstream is the rule's upstream, resolved through its credential or
	// mirror.
	Upstream *Upstream
	// Exactly one of Credential and Mirror is set once the reference
	// resolves.
	Credential *Credential
	Mirror     *Mirror
	// Repositories are the rule's parsed globs.
	Repositories []glob.Glob
	// Actions are the rule's actions plus the pull that push and delete
	// imply.
	Actions []v1alpha1.Action
}

// Usable reports whether resolution treats the rule as a candidate: it is
// accepted, or it is a mirror rule whose mirror isn't ready (a failed
// mirror), or it waits on a pending Secret.
func (r *Rule) Usable() bool {
	return r.Status.Pending || r.Status.Ready ||
		(r.Mirror != nil && r.Status.Reason == v1alpha1.ReasonRefNotReady)
}

// Allows reports whether the rule covers the action on the repository.
func (r *Rule) Allows(repository string, action v1alpha1.Action) bool {
	if !containsAction(r.Actions, action) {
		return false
	}

	for _, pattern := range r.Repositories {
		if pattern.Match(repository) {
			return true
		}
	}

	return false
}

// ServiceAccount names a ServiceAccount.
type ServiceAccount struct {
	Namespace string
	Name      string
}

// Snapshot is the immutable model built from one set of inputs. Nothing in
// it may be modified after Build returns it.
type Snapshot struct {
	// Upstreams holds the UpstreamRegistries of the admin namespaces;
	// others are ignored.
	Upstreams   map[types.NamespacedName]*Upstream
	Credentials map[types.NamespacedName]*Credential
	Mirrors     map[types.NamespacedName]*Mirror
	Policies    map[types.NamespacedName]*Policy

	hosts map[string]*Upstream
	rules map[ServiceAccount][]*Rule

	// pendingSecrets is the number of referenced Secrets still pending.
	pendingSecrets int
	// referenced lists every Secret the objects reference.
	referenced []secrets.Key
}

// Host returns the ready upstream that serves the lower-case host, or nil.
func (s *Snapshot) Host(host string) *Upstream { return s.hosts[host] }

// Rules returns the rules resolution considers for the ServiceAccount:
// accepted rules, mirror rules whose mirror isn't ready, and rules waiting
// on a pending Secret (Status.Pending).
func (s *Snapshot) Rules(serviceAccount ServiceAccount) []*Rule { return s.rules[serviceAccount] }

// PendingSecrets reports whether any Secret the snapshot references is
// still pending.
func (s *Snapshot) PendingSecrets() bool { return s.pendingSecrets > 0 }

// Referenced returns the Secrets the objects reference, sorted.
func (s *Snapshot) Referenced() []secrets.Key { return s.referenced }

func nameOf(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}

func ready(generation int64) Status {
	return Status{Ready: true, Reason: v1alpha1.ReasonReady, ObservedGeneration: generation}
}

func notReady(generation int64, reason, format string, args ...any) Status {
	return Status{Reason: reason, Message: fmt.Sprintf(format, args...), ObservedGeneration: generation}
}

func pending(generation int64) Status {
	return Status{Pending: true, ObservedGeneration: generation}
}
