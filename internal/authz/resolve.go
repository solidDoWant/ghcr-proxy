package authz

import (
	"fmt"
	"slices"
	"strings"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/authn"
	"github.com/solidDoWant/registrygate/internal/model"
	"github.com/solidDoWant/registrygate/internal/registry"
)

// Result is what resolution decided for a request.
type Result int

const (
	// ResultNotReady answers 503 "registrygate is not ready": there is no snapshot yet, or an object the request
	// needs waits on a pending Secret.
	ResultNotReady Result = iota
	// ResultNotFound answers a plain 404: no ready upstream serves the host. Nothing is sent.
	ResultNotFound
	// ResultChallenge answers 401 with the Basic challenge. Nothing is sent.
	ResultChallenge
	// ResultVersionCheck answers the version check with 200 from the proxy. Nothing is sent.
	ResultVersionCheck
	// ResultCredential forwards to Resolution.Upstream with a token from Resolution.Credential.
	ResultCredential
	// ResultMirror serves a pull from Resolution.Mirror; the routes a mirror doesn't serve go to the upstream
	// anonymously.
	ResultMirror
	// ResultAnonymous serves a pull from the upstream's default mirror when Resolution.Mirror is set, else from
	// the upstream with no credential.
	ResultAnonymous
	// ResultDenied answers 403 DENIED with Resolution.Message. Nothing is sent.
	ResultDenied
	// ResultAmbiguous answers 403 DENIED with Resolution.Message: the matching rules name more than one
	// credential or more than one mirror. Nothing is sent.
	ResultAmbiguous
)

// maxListedRules is how many rules an ambiguity message names.
const maxListedRules = 5

// Resolution is resolution's answer for one request.
type Resolution struct {
	Result Result
	// Request is the request to forward. It differs from the one resolved when a mount's source doesn't resolve
	// to the push's credential: the mount is then dropped.
	Request registry.Request
	// Upstream is the upstream serving the host; set for every result but ResultNotReady and ResultNotFound.
	Upstream *model.Upstream
	// Credential is the credential to forward with, set for ResultCredential.
	Credential *model.Credential
	// Mirror is the mirror to pull from: the rule's mirror for ResultMirror, the default mirror (or nil) for
	// ResultAnonymous.
	Mirror *model.Mirror
	// MirrorFailed is set for ResultMirror when the rule's mirror isn't ready: the pull is treated as a failed
	// mirror (fallback to the upstream when the mirror allows it, else 502), never moved to the default mirror.
	MirrorFailed bool
	// Rules names the candidate rules as namespace/policy/rule, sorted.
	Rules []string
	// Message is the 403 message for ResultDenied and ResultAmbiguous. It names only the caller's namespace and
	// ServiceAccount, the repository, the action and rule names.
	Message string
}

// Decide resolves a classified request for identity to the host the client reached, which is the TLS server name.
// The zero Identity is an anonymous caller. A request is forwarded with a credential only when an accepted rule of a
// policy in the caller's own namespace names its ServiceAccount, the host's upstream, the repository and the action,
// and no other matching rule names a different credential.
func Decide(snapshot *model.Snapshot, host string, identity authn.Identity, request registry.Request) Resolution {
	if snapshot == nil {
		return Resolution{Result: ResultNotReady, Request: request}
	}

	host = strings.ToLower(host)

	upstream := snapshot.Host(host)
	if upstream == nil {
		if pendingHost(snapshot, host) {
			return Resolution{Result: ResultNotReady, Request: request}
		}

		return Resolution{Result: ResultNotFound, Request: request}
	}

	resolution := Resolution{Request: request, Upstream: upstream}
	anonymous := identity.ServiceAccount == ""

	switch {
	case request.Route == registry.RouteBase && anonymous:
		resolution.Result = ResultChallenge
	case request.Route == registry.RouteBase:
		resolution.Result = ResultVersionCheck
	case anonymous:
		if upstream.Object.Spec.AnonymousPulls == v1alpha1.AnonymousPullsForward && request.Action == v1alpha1.ActionPull {
			resolution.Result = ResultAnonymous
			resolution.Mirror = upstream.DefaultMirror
		} else {
			resolution.Result = ResultChallenge
		}
	default:
		resolution = resolve(snapshot, upstream, identity, request)

		if resolution.Result == ResultCredential && request.Mount != nil {
			source := resolve(snapshot, upstream, identity, registry.Request{Name: request.Mount.From, Action: v1alpha1.ActionPull})
			if source.Result != ResultCredential || source.Credential != resolution.Credential {
				resolution.Request.Mount = nil
			}
		}
	}

	return resolution
}

// resolve resolves an authenticated caller's request on upstream from its candidate rules.
func resolve(snapshot *model.Snapshot, upstream *model.Upstream, identity authn.Identity, request registry.Request) Resolution {
	account := model.ServiceAccount{Namespace: identity.Namespace, Name: identity.ServiceAccount}

	var candidates []*model.Rule

	// The snapshot's rules are already the usable ones: a credential rule whose credential isn't ready is left out,
	// a mirror rule whose mirror isn't ready is kept as a failed mirror.
	for _, rule := range snapshot.Rules(account) {
		if rule.Upstream == upstream && rule.Allows(request.Name, request.Action) {
			candidates = append(candidates, rule)
		}
	}

	resolution := Resolution{Request: request, Upstream: upstream}

	if len(candidates) == 0 {
		if request.Action == v1alpha1.ActionPull {
			resolution.Result = ResultAnonymous
			resolution.Mirror = upstream.DefaultMirror

			return resolution
		}

		resolution.Result = ResultDenied
		resolution.Message = fmt.Sprintf("ServiceAccount %s/%s may not %s repository %s: no RegistryAccessPolicy rule allows it",
			identity.Namespace, identity.ServiceAccount, request.Action, request.Name)

		return resolution
	}

	// Credential rules win over mirror rules: private content needs the credential, mirrors are for public content.
	if slices.ContainsFunc(candidates, func(rule *model.Rule) bool { return rule.Credential != nil }) {
		candidates = slices.DeleteFunc(candidates, func(rule *model.Rule) bool { return rule.Credential == nil })
	}

	resolution.Rules = ruleNames(candidates)

	credentials := map[*model.Credential]bool{}
	mirrors := map[*model.Mirror]bool{}

	for _, rule := range candidates {
		if rule.Credential != nil {
			credentials[rule.Credential] = true
		} else {
			mirrors[rule.Mirror] = true
		}
	}

	if len(credentials) > 1 || len(mirrors) > 1 {
		listed := resolution.Rules
		if len(listed) > maxListedRules {
			listed = append(slices.Clip(listed[:maxListedRules]), "…")
		}

		resolution.Result = ResultAmbiguous
		resolution.Message = "ambiguous rules: " + strings.Join(listed, ", ")

		return resolution
	}

	if slices.ContainsFunc(candidates, func(rule *model.Rule) bool { return rule.Status.Pending }) {
		resolution.Result = ResultNotReady
		return resolution
	}

	chosen := candidates[0]
	if chosen.Credential != nil {
		resolution.Result = ResultCredential
		resolution.Credential = chosen.Credential

		return resolution
	}

	resolution.Result = ResultMirror
	resolution.Mirror = chosen.Mirror
	resolution.MirrorFailed = !chosen.Mirror.Status.Ready

	return resolution
}

// pendingHost reports whether an upstream that waits on a pending Secret claims the host.
func pendingHost(snapshot *model.Snapshot, host string) bool {
	for _, upstream := range snapshot.Upstreams {
		if upstream.Status.Pending && slices.Contains(upstream.Object.Spec.Hosts, host) {
			return true
		}
	}

	return false
}

func ruleNames(rules []*model.Rule) []string {
	names := make([]string, 0, len(rules))
	for _, rule := range rules {
		policy := rule.Policy.Object
		names = append(names, policy.Namespace+"/"+policy.Name+"/"+rule.Name)
	}

	slices.Sort(names)

	return slices.Compact(names)
}
