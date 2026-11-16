package model

import (
	"cmp"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/glob"
	"github.com/solidDoWant/registrygate/internal/secrets"
)

// Secret keys the objects read.
const (
	keyTLSCertificate  = "tls.crt"
	keyTLSKey          = "tls.key"
	keyCA              = "ca.crt"
	defaultUsernameKey = "username"
	defaultPasswordKey = "password"
)

var (
	dnsNamePattern    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	pathPrefixPattern = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
)

// SecretSource is what the model needs from the Secret store. Get's boolean
// is false when the store doesn't track the Secret yet; the model treats
// that as pending.
type SecretSource interface {
	Get(key secrets.Key) (secrets.Secret, bool)
}

// Inputs are everything a snapshot is built from.
type Inputs struct {
	Upstreams   []v1alpha1.UpstreamRegistry
	Credentials []v1alpha1.UpstreamCredential
	Mirrors     []v1alpha1.RegistryMirror
	Policies    []v1alpha1.RegistryAccessPolicy
	// Namespaces maps each Namespace in the cache to its labels. A
	// namespace that isn't here is admitted only by its own objects.
	Namespaces map[string]labels.Set
	Secrets    SecretSource
	// AdminNamespaces may hold UpstreamRegistries.
	AdminNamespaces []string
	// AllowHTTP accepts http URLs; tests only.
	AllowHTTP bool
}

// Build computes the readiness of every object and the snapshot's indexes.
// It never modifies its inputs; the snapshot keeps pointers to copies.
func Build(in Inputs) *Snapshot {
	builder := &builder{
		in:         in,
		referenced: map[secrets.Key]secrets.Secret{},
		snapshot: &Snapshot{
			Upstreams:   map[types.NamespacedName]*Upstream{},
			Credentials: map[types.NamespacedName]*Credential{},
			Mirrors:     map[types.NamespacedName]*Mirror{},
			Policies:    map[types.NamespacedName]*Policy{},
			hosts:       map[string]*Upstream{},
			rules:       map[ServiceAccount][]*Rule{},
		},
	}

	builder.buildUpstreams()
	builder.buildMirrors()
	builder.buildCredentials()
	builder.buildPolicies()
	builder.buildDefaultMirrors()
	builder.buildIndexes()

	return builder.snapshot
}

type builder struct {
	in         Inputs
	snapshot   *Snapshot
	referenced map[secrets.Key]secrets.Secret
}

// secret reads a Secret and records the reference. A Secret the store
// doesn't track yet is pending.
func (b *builder) secret(namespace, name string) secrets.Secret {
	key := secrets.Key{Namespace: namespace, Name: name}
	if secret, seen := b.referenced[key]; seen {
		return secret
	}

	secret := secrets.Secret{State: secrets.Pending}
	if b.in.Secrets != nil {
		if got, tracked := b.in.Secrets.Get(key); tracked {
			secret = got
		}
	}

	b.referenced[key] = secret

	return secret
}

// secretProblem maps a Secret that isn't present to a status; nil means
// the Secret is present.
func secretProblem(generation int64, key secrets.Key, secret secrets.Secret) *Status {
	switch secret.State {
	case secrets.Present:
		return nil
	case secrets.Pending:
		status := pending(generation)
		return &status
	default:
		status := notReady(generation, v1alpha1.ReasonSecretUnavailable,
			"Secret %s does not exist or the proxy may not read it", key)

		return &status
	}
}

func (b *builder) readCA(generation int64, namespace string, ref *v1alpha1.LocalRef) (*x509.CertPool, string, *Status) {
	if ref == nil {
		return nil, "", nil
	}

	key := secrets.Key{Namespace: namespace, Name: ref.Name}

	secret := b.secret(namespace, ref.Name)
	if problem := secretProblem(generation, key, secret); problem != nil {
		return nil, "", problem
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(secret.Data[keyCA]) {
		status := notReady(generation, v1alpha1.ReasonSecretInvalid,
			"Secret %s has no valid PEM certificate in key %s", key, keyCA)

		return nil, "", &status
	}

	return pool, secret.ResourceVersion, nil
}

func (b *builder) readCredentials(generation int64, namespace string, ref v1alpha1.CredentialsSecretRef) (Credentials, string, *Status) {
	key := secrets.Key{Namespace: namespace, Name: ref.Name}

	secret := b.secret(namespace, ref.Name)
	if problem := secretProblem(generation, key, secret); problem != nil {
		return Credentials{}, "", problem
	}

	usernameKey := cmp.Or(ref.UsernameKey, defaultUsernameKey)
	passwordKey := cmp.Or(ref.PasswordKey, defaultPasswordKey)

	for _, name := range []string{usernameKey, passwordKey} {
		if len(secret.Data[name]) == 0 {
			status := notReady(generation, v1alpha1.ReasonSecretInvalid,
				"Secret %s has no non-empty key %s", key, name)

			return Credentials{}, "", &status
		}
	}

	credentials := Credentials{Username: string(secret.Data[usernameKey]), Password: string(secret.Data[passwordKey])}

	return credentials, secret.ResourceVersion, nil
}

func (b *builder) buildUpstreams() {
	claims := map[string][]*Upstream{}
	valid := map[*Upstream]bool{}

	for index := range b.in.Upstreams {
		object := b.in.Upstreams[index].DeepCopy()
		if !slices.Contains(b.in.AdminNamespaces, object.Namespace) {
			continue
		}

		upstream := &Upstream{Object: object}
		b.snapshot.Upstreams[upstream.Name()] = upstream

		parsed, admission, err := b.validateUpstream(object)
		if err != nil {
			upstream.Status = notReady(object.Generation, v1alpha1.ReasonInvalidSpec, "%s", err)
			continue
		}

		upstream.URL = parsed
		upstream.admission = admission
		valid[upstream] = true

		for _, host := range object.Spec.Hosts {
			claims[host] = append(claims[host], upstream)
		}
	}

	for _, upstream := range sortedValues(b.snapshot.Upstreams) {
		if valid[upstream] {
			upstream.Status = b.upstreamStatus(upstream, claims)
		}
	}
}

func (b *builder) upstreamStatus(upstream *Upstream, claims map[string][]*Upstream) Status {
	object := upstream.Object
	generation := object.Generation
	key := secrets.Key{Namespace: object.Namespace, Name: object.Spec.TLS.SecretRef.Name}

	serving := b.secret(key.Namespace, key.Name)
	// Read the CA Secret too, so it is referenced whatever the serving
	// Secret's state.
	pool, caVersion, caProblem := b.readCA(generation, object.Namespace, object.Spec.CASecretRef)

	if problem := secretProblem(generation, key, serving); problem != nil {
		return *problem
	}

	certificate, err := tls.X509KeyPair(serving.Data[keyTLSCertificate], serving.Data[keyTLSKey])
	if err != nil {
		return notReady(generation, v1alpha1.ReasonSecretInvalid,
			"Secret %s has no valid key pair in keys %s and %s", key, keyTLSCertificate, keyTLSKey)
	}

	for _, host := range object.Spec.Hosts {
		if err := certificate.Leaf.VerifyHostname(host); err != nil {
			return notReady(generation, v1alpha1.ReasonCertificateMismatch,
				"the certificate in Secret %s does not cover host %s", key, host)
		}
	}

	if caProblem != nil {
		return *caProblem
	}

	for _, host := range object.Spec.Hosts {
		for _, other := range claims[host] {
			if other != upstream {
				return notReady(generation, v1alpha1.ReasonHostConflict,
					"host %s is also claimed by UpstreamRegistry %s", host, other.Name())
			}
		}
	}

	upstream.Certificate = &certificate
	upstream.CertificateResourceVersion = serving.ResourceVersion
	upstream.CAs = pool
	upstream.CAResourceVersion = caVersion

	return ready(generation)
}

func (b *builder) validateUpstream(object *v1alpha1.UpstreamRegistry) (*url.URL, admission, error) {
	spec := object.Spec
	if len(spec.Hosts) == 0 {
		return nil, admission{}, fmt.Errorf("spec.hosts is empty")
	}

	for _, host := range spec.Hosts {
		if !dnsNamePattern.MatchString(host) {
			return nil, admission{}, fmt.Errorf("spec.hosts: %q is not a lower-case DNS name", host)
		}
	}

	if spec.TLS.SecretRef.Name == "" {
		return nil, admission{}, fmt.Errorf("spec.tls.secretRef.name is empty")
	}

	parsed, err := parseURL(spec.URL, b.in.AllowHTTP, false)
	if err != nil {
		return nil, admission{}, fmt.Errorf("spec.url: %w", err)
	}

	switch spec.AnonymousPulls {
	case "", v1alpha1.AnonymousPullsChallenge, v1alpha1.AnonymousPullsForward:
	default:
		return nil, admission{}, fmt.Errorf("spec.anonymousPulls: %q is not Challenge or Forward", spec.AnonymousPulls)
	}

	for _, prefix := range spec.AllowedMirrorURLs {
		if _, err := parseURL(prefix, b.in.AllowHTTP, true); err != nil {
			return nil, admission{}, fmt.Errorf("spec.allowedMirrorURLs: %q: %w", prefix, err)
		}
	}

	admission, err := newAdmission(object.Namespace, spec.AllowedNamespaces)
	if err != nil {
		return nil, admission, err
	}

	return parsed, admission, nil
}

func (b *builder) buildMirrors() {
	for index := range b.in.Mirrors {
		object := b.in.Mirrors[index].DeepCopy()
		mirror := &Mirror{Object: object, FallbackToUpstream: object.Spec.FallbackToUpstream == nil || *object.Spec.FallbackToUpstream}
		b.snapshot.Mirrors[mirror.Name()] = mirror
		mirror.Status = b.mirrorStatus(mirror)
	}
}

func (b *builder) mirrorStatus(mirror *Mirror) Status {
	object := mirror.Object
	spec := object.Spec
	generation := object.Generation

	// Read every Secret first so each is referenced whatever happens below.
	var (
		credentials     Credentials
		credentialsRV   string
		credentialsProb *Status
	)

	if spec.SecretRef != nil {
		credentials, credentialsRV, credentialsProb = b.readCredentials(generation, object.Namespace, *spec.SecretRef)
	}

	pool, caVersion, caProblem := b.readCA(generation, object.Namespace, spec.CASecretRef)

	parsed, err := parseURL(spec.URL, b.in.AllowHTTP, true)
	if err != nil {
		return notReady(generation, v1alpha1.ReasonInvalidSpec, "spec.url: %s", err)
	}

	if !pathPrefixPattern.MatchString(spec.PathPrefix) || len(spec.PathPrefix) > 255 {
		return notReady(generation, v1alpha1.ReasonInvalidSpec, "spec.pathPrefix %q is not a repository name", spec.PathPrefix)
	}

	admission, err := newAdmission(object.Namespace, spec.AllowedNamespaces)
	if err != nil {
		return notReady(generation, v1alpha1.ReasonInvalidSpec, "%s", err)
	}

	mirror.URL = parsed
	mirror.admission = admission

	upstream, problem := b.upstreamRef(generation, object.Namespace, spec.UpstreamRef)
	mirror.Upstream = upstream

	if problem != nil {
		return *problem
	}

	if !mirrorURLAllowed(parsed, upstream.Object.Spec.AllowedMirrorURLs) {
		return notReady(generation, v1alpha1.ReasonURLNotAllowed,
			"spec.url does not match any allowedMirrorURLs of UpstreamRegistry %s", upstream.Name())
	}

	for _, problem := range []*Status{credentialsProb, caProblem} {
		if problem != nil {
			return *problem
		}
	}

	if spec.SecretRef != nil {
		mirror.Credentials = &credentials
		mirror.SecretResourceVersion = credentialsRV
	}

	mirror.CAs = pool
	mirror.CAResourceVersion = caVersion

	return ready(generation)
}

// upstreamRef resolves an upstream reference of an object in namespace and
// checks that it exists, admits the namespace and is ready.
func (b *builder) upstreamRef(generation int64, namespace string, ref v1alpha1.ObjectRef) (*Upstream, *Status) {
	name := nameOf(cmp.Or(ref.Namespace, namespace), ref.Name)

	upstream := b.snapshot.Upstreams[name]
	if upstream == nil {
		status := notReady(generation, v1alpha1.ReasonRefNotFound, "UpstreamRegistry %s not found", name)
		return nil, &status
	}

	if !upstream.Status.Pending && upstream.Status.Reason == v1alpha1.ReasonInvalidSpec {
		status := notReady(generation, v1alpha1.ReasonRefNotReady, "UpstreamRegistry %s is not ready", name)
		return upstream, &status
	}

	if !upstream.admission.admits(namespace, b.in.Namespaces) {
		status := notReady(generation, v1alpha1.ReasonNotAllowed,
			"UpstreamRegistry %s does not allow namespace %s", name, namespace)

		return upstream, &status
	}

	if upstream.Status.Pending {
		status := pending(generation)
		return upstream, &status
	}

	if !upstream.Status.Ready {
		status := notReady(generation, v1alpha1.ReasonRefNotReady, "UpstreamRegistry %s is not ready", name)
		return upstream, &status
	}

	return upstream, nil
}

func (b *builder) buildCredentials() {
	for index := range b.in.Credentials {
		object := b.in.Credentials[index].DeepCopy()
		credential := &Credential{Object: object}
		b.snapshot.Credentials[credential.Name()] = credential
		credential.Status = b.credentialStatus(credential)
	}
}

func (b *builder) credentialStatus(credential *Credential) Status {
	object := credential.Object
	spec := object.Spec
	generation := object.Generation

	credentials, resourceVersion, secretProblem := b.readCredentials(generation, object.Namespace, spec.SecretRef)

	ceiling, err := parseCeiling(spec.Ceiling)
	if err != nil {
		return notReady(generation, v1alpha1.ReasonInvalidSpec, "spec.ceiling: %s", err)
	}

	admission, err := newAdmission(object.Namespace, spec.AllowedNamespaces)
	if err != nil {
		return notReady(generation, v1alpha1.ReasonInvalidSpec, "%s", err)
	}

	credential.ceiling = ceiling
	credential.admission = admission

	upstream, problem := b.upstreamRef(generation, object.Namespace, spec.UpstreamRef)
	credential.Upstream = upstream

	if problem != nil {
		return *problem
	}

	if secretProblem != nil {
		return *secretProblem
	}

	credential.Credentials = credentials
	credential.SecretResourceVersion = resourceVersion

	return ready(generation)
}

func (b *builder) buildPolicies() {
	for index := range b.in.Policies {
		object := b.in.Policies[index].DeepCopy()
		policy := &Policy{Object: object}
		b.snapshot.Policies[policy.Name()] = policy

		var (
			notAccepted []string
			isPending   bool
		)

		for _, spec := range object.Spec.Rules {
			rule := &Rule{Policy: policy, Name: spec.Name}
			rule.Status = b.ruleStatus(rule, spec)
			policy.Rules = append(policy.Rules, rule)

			switch {
			case rule.Status.Pending:
				isPending = true
			case !rule.Status.Ready:
				notAccepted = append(notAccepted, rule.Name)
			}
		}

		generation := object.Generation

		switch {
		case isPending:
			policy.Status = pending(generation)
		case len(notAccepted) > 0:
			policy.Status = notReady(generation, v1alpha1.ReasonRulesNotAccepted,
				"rules not accepted: %s", strings.Join(notAccepted, ", "))
		default:
			policy.Status = ready(generation)
		}
	}
}

func (b *builder) ruleStatus(rule *Rule, spec v1alpha1.AccessRule) Status {
	generation := rule.Policy.Object.Generation

	accepted := Status{Ready: true, Reason: v1alpha1.ReasonAccepted, ObservedGeneration: generation}

	for _, pattern := range spec.Repositories {
		parsed, err := glob.Parse(pattern)
		if err != nil {
			return notReady(generation, v1alpha1.ReasonInvalidSpec, "%s", err)
		}

		rule.Repositories = append(rule.Repositories, parsed)
	}

	actions, err := impliedActions(spec.Actions)
	if err != nil {
		return notReady(generation, v1alpha1.ReasonInvalidSpec, "%s", err)
	}

	rule.Actions = actions

	switch {
	case spec.CredentialRef != nil && spec.MirrorRef == nil:
		return b.credentialRuleStatus(rule, *spec.CredentialRef, accepted)
	case spec.MirrorRef != nil && spec.CredentialRef == nil:
		if !slices.Equal(spec.Actions, []v1alpha1.Action{v1alpha1.ActionPull}) {
			return notReady(generation, v1alpha1.ReasonInvalidSpec, "a mirror rule may only allow pull")
		}

		return b.mirrorRuleStatus(rule, *spec.MirrorRef, accepted)
	default:
		return notReady(generation, v1alpha1.ReasonInvalidSpec, "exactly one of credentialRef and mirrorRef must be set")
	}
}

func (b *builder) credentialRuleStatus(rule *Rule, ref v1alpha1.ObjectRef, accepted Status) Status {
	namespace := rule.Policy.Object.Namespace
	generation := accepted.ObservedGeneration
	name := nameOf(cmp.Or(ref.Namespace, namespace), ref.Name)

	credential := b.snapshot.Credentials[name]
	if credential == nil {
		return notReady(generation, v1alpha1.ReasonRefNotFound, "UpstreamCredential %s not found", name)
	}

	rule.Credential = credential
	rule.Upstream = credential.Upstream

	if problem := refAdmission(generation, "UpstreamCredential", name, credential.admission, credential.Upstream, namespace, b.in.Namespaces); problem != nil {
		return *problem
	}

	if credential.Status.Pending {
		return pending(generation)
	}

	if !credential.Status.Ready {
		return notReady(generation, v1alpha1.ReasonRefNotReady, "UpstreamCredential %s is not ready", name)
	}

	if outside := credential.outsideCeiling(rule); len(outside) > 0 {
		return notReady(generation, v1alpha1.ReasonOutsideCeiling,
			"outside the ceiling of UpstreamCredential %s: %s", name, strings.Join(outside, ", "))
	}

	return accepted
}

func (b *builder) mirrorRuleStatus(rule *Rule, ref v1alpha1.ObjectRef, accepted Status) Status {
	namespace := rule.Policy.Object.Namespace
	generation := accepted.ObservedGeneration
	name := nameOf(cmp.Or(ref.Namespace, namespace), ref.Name)

	mirror := b.snapshot.Mirrors[name]
	if mirror == nil {
		return notReady(generation, v1alpha1.ReasonRefNotFound, "RegistryMirror %s not found", name)
	}

	rule.Mirror = mirror
	rule.Upstream = mirror.Upstream

	if problem := refAdmission(generation, "RegistryMirror", name, mirror.admission, mirror.Upstream, namespace, b.in.Namespaces); problem != nil {
		return *problem
	}

	if mirror.Status.Pending {
		return pending(generation)
	}

	if !mirror.Status.Ready {
		return notReady(generation, v1alpha1.ReasonRefNotReady, "RegistryMirror %s is not ready", name)
	}

	return accepted
}

// refAdmission checks that a rule's credential or mirror, and the upstream
// behind it, both admit the policy's namespace.
func refAdmission(generation int64, kind string, name types.NamespacedName, refAdmission admission, upstream *Upstream, namespace string, namespaces map[string]labels.Set) *Status {
	if !refAdmission.admits(namespace, namespaces) {
		status := notReady(generation, v1alpha1.ReasonNotAllowed, "%s %s does not allow namespace %s", kind, name, namespace)
		return &status
	}

	if upstream != nil && upstream.URL != nil && !upstream.admission.admits(namespace, namespaces) {
		status := notReady(generation, v1alpha1.ReasonNotAllowed,
			"UpstreamRegistry %s does not allow namespace %s", upstream.Name(), namespace)

		return &status
	}

	return nil
}

func (b *builder) buildDefaultMirrors() {
	for _, upstream := range b.snapshot.Upstreams {
		ref := upstream.Object.Spec.DefaultMirrorRef
		if ref == nil || !upstream.Status.Ready {
			continue
		}

		name := nameOf(cmp.Or(ref.Namespace, upstream.Object.Namespace), ref.Name)
		mirror := b.snapshot.Mirrors[name]

		var problem string

		switch {
		case mirror == nil:
			problem = "not found"
		case mirror.Upstream != upstream:
			problem = "is a mirror of another upstream"
		case !mirror.admission.admits(upstream.Object.Namespace, b.in.Namespaces):
			problem = "does not allow the upstream's namespace"
		case !mirror.Status.Ready:
			problem = "is not ready"
		}

		if problem != "" {
			upstream.Status.Message = fmt.Sprintf("default mirror %s isn't usable (%s); pulls go to the upstream", name, problem)
			continue
		}

		upstream.DefaultMirror = mirror
	}
}

func (b *builder) buildIndexes() {
	for host, upstream := range b.hostsOfReadyUpstreams() {
		b.snapshot.hosts[host] = upstream
	}

	for _, policy := range sortedValues(b.snapshot.Policies) {
		for _, rule := range policy.Rules {
			if !rule.Usable() {
				continue
			}

			for _, name := range policy.Object.Spec.ServiceAccounts {
				account := ServiceAccount{Namespace: policy.Object.Namespace, Name: name}
				b.snapshot.rules[account] = append(b.snapshot.rules[account], rule)
			}
		}
	}

	keys := make([]secrets.Key, 0, len(b.referenced))
	for key, secret := range b.referenced {
		keys = append(keys, key)

		if secret.State == secrets.Pending {
			b.snapshot.pendingSecrets++
		}
	}

	slices.SortFunc(keys, func(a, c secrets.Key) int {
		return cmp.Or(cmp.Compare(a.Namespace, c.Namespace), cmp.Compare(a.Name, c.Name))
	})
	b.snapshot.referenced = keys
}

func (b *builder) hostsOfReadyUpstreams() map[string]*Upstream {
	hosts := map[string]*Upstream{}

	for _, upstream := range b.snapshot.Upstreams {
		if !upstream.Status.Ready {
			continue
		}

		for _, host := range upstream.Object.Spec.Hosts {
			hosts[host] = upstream
		}
	}

	return hosts
}

// admission decides which namespaces may refer to an object.
type admission struct {
	own      string
	names    []string
	selector labels.Selector
}

func newAdmission(own string, allowed v1alpha1.AllowedNamespaces) (admission, error) {
	out := admission{own: own, names: allowed.Names}

	if allowed.Selector != nil {
		selector, err := metav1.LabelSelectorAsSelector(allowed.Selector)
		if err != nil {
			return admission{}, fmt.Errorf("spec.allowedNamespaces.selector: %w", err)
		}

		out.selector = selector
	}

	return out, nil
}

// admits reports whether namespace may refer to the object: it is the
// object's own, is listed by name, or is in the cache with labels the
// selector matches.
func (a admission) admits(namespace string, namespaces map[string]labels.Set) bool {
	if a.own == "" {
		return false
	}

	if namespace == a.own || slices.Contains(a.names, namespace) {
		return true
	}

	if a.selector == nil {
		return false
	}

	set, cached := namespaces[namespace]

	return cached && a.selector.Matches(set)
}

// ceilingEntry is one parsed AccessGrant of a ceiling.
type ceilingEntry struct {
	repositories []glob.Glob
	actions      []v1alpha1.Action
}

func parseCeiling(grants []v1alpha1.AccessGrant) ([]ceilingEntry, error) {
	if len(grants) == 0 {
		return nil, fmt.Errorf("the ceiling is empty")
	}

	out := make([]ceilingEntry, 0, len(grants))

	for _, grant := range grants {
		var entry ceilingEntry

		for _, pattern := range grant.Repositories {
			parsed, err := glob.Parse(pattern)
			if err != nil {
				return nil, err
			}

			entry.repositories = append(entry.repositories, parsed)
		}

		actions, err := impliedActions(grant.Actions)
		if err != nil {
			return nil, err
		}

		entry.actions = actions
		out = append(out, entry)
	}

	return out, nil
}

// outsideCeiling returns "<pattern> <action>" for each of the rule's
// patterns and implied actions no single ceiling entry contains.
func (c *Credential) outsideCeiling(rule *Rule) []string {
	var outside []string

	for _, pattern := range rule.Repositories {
		for _, action := range rule.Actions {
			if !c.ceilingAllows(pattern, action) {
				outside = append(outside, fmt.Sprintf("%s %s", pattern, action))
			}
		}
	}

	return outside
}

func (c *Credential) ceilingAllows(pattern glob.Glob, action v1alpha1.Action) bool {
	for _, entry := range c.ceiling {
		if !containsAction(entry.actions, action) {
			continue
		}

		for _, outer := range entry.repositories {
			if glob.Contains(outer, pattern) {
				return true
			}
		}
	}

	return false
}

var actionOrder = []v1alpha1.Action{v1alpha1.ActionPull, v1alpha1.ActionPush, v1alpha1.ActionDelete}

// impliedActions returns the actions plus the pull that push and delete
// imply, in a fixed order.
func impliedActions(actions []v1alpha1.Action) ([]v1alpha1.Action, error) {
	if len(actions) == 0 {
		return nil, fmt.Errorf("no actions")
	}

	set := map[v1alpha1.Action]bool{}

	for _, action := range actions {
		if !slices.Contains(actionOrder, action) {
			return nil, fmt.Errorf("unknown action %q", action)
		}

		set[action] = true
		set[v1alpha1.ActionPull] = true
	}

	out := make([]v1alpha1.Action, 0, len(set))

	for _, action := range actionOrder {
		if set[action] {
			out = append(out, action)
		}
	}

	return out, nil
}

func containsAction(actions []v1alpha1.Action, action v1alpha1.Action) bool {
	return slices.Contains(actions, action)
}

// parseURL checks an upstream or mirror URL: https (http only when allowed),
// a host, no userinfo, query, fragment or percent-encoding, and, when a path
// is allowed, no empty, "." or ".." segments.
func parseURL(raw string, allowHTTP, allowPath bool) (*url.URL, error) {
	if strings.Contains(raw, "%") {
		return nil, fmt.Errorf("must not contain %%")
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("not a URL")
	}

	switch {
	case parsed.Scheme == "https":
	case parsed.Scheme == "http" && allowHTTP:
	default:
		return nil, fmt.Errorf("the scheme must be https")
	}

	if parsed.Host == "" || parsed.Opaque != "" {
		return nil, fmt.Errorf("no host")
	}

	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.ContainsAny(raw, "?#") {
		return nil, fmt.Errorf("must not have userinfo, a query or a fragment")
	}

	if !allowPath {
		if parsed.Path != "" {
			return nil, fmt.Errorf("must not have a path")
		}

		return parsed, nil
	}

	path := strings.TrimSuffix(parsed.Path, "/")
	if path != "" {
		for _, segment := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
			if segment == "" || segment == "." || segment == ".." {
				return nil, fmt.Errorf("the path must not have empty, . or .. segments")
			}
		}
	}

	return parsed, nil
}

// mirrorURLAllowed reports whether the mirror URL matches a prefix: the
// same scheme and host, and the prefix's path is a prefix of the mirror's
// path on a "/" boundary.
func mirrorURLAllowed(mirror *url.URL, prefixes []string) bool {
	mirrorPath := strings.TrimSuffix(mirror.Path, "/")

	for _, raw := range prefixes {
		prefix, err := url.Parse(raw)
		if err != nil {
			continue
		}

		if !strings.EqualFold(prefix.Scheme, mirror.Scheme) || !strings.EqualFold(prefix.Host, mirror.Host) {
			continue
		}

		prefixPath := strings.TrimSuffix(prefix.Path, "/")
		if prefixPath == "" || mirrorPath == prefixPath || strings.HasPrefix(mirrorPath, prefixPath+"/") {
			return true
		}
	}

	return false
}

// sortedValues returns a map's values ordered by name, so builds are
// deterministic.
func sortedValues[T any](values map[types.NamespacedName]T) []T {
	names := make([]types.NamespacedName, 0, len(values))
	for name := range values {
		names = append(names, name)
	}

	slices.SortFunc(names, func(a, c types.NamespacedName) int {
		return cmp.Or(cmp.Compare(a.Namespace, c.Namespace), cmp.Compare(a.Name, c.Name))
	})

	out := make([]T, 0, len(names))
	for _, name := range names {
		out = append(out, values[name])
	}

	return out
}
