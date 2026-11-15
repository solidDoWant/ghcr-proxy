package authz

import (
	"fmt"

	"github.com/solidDoWant/ghcr-proxy/api/v1alpha1"
	"github.com/solidDoWant/ghcr-proxy/internal/authn"
	"github.com/solidDoWant/ghcr-proxy/internal/registry"
)

// Outcome is what the proxy does with a request.
type Outcome int

const (
	// OutcomeUnavailable answers 503: the index has not synced, so the proxy can't decide.
	OutcomeUnavailable Outcome = iota
	// OutcomeChallenge answers 401 with the Basic challenge, nothing upstream.
	OutcomeChallenge
	// OutcomeVersionCheck answers 200 with an empty JSON object, nothing upstream.
	OutcomeVersionCheck
	// OutcomeForward forwards Decision.Forward upstream.
	OutcomeForward
	// OutcomeDenied answers 403 with Decision.Message, nothing upstream.
	OutcomeDenied
)

// Decision is the authorizer's answer for one request.
type Decision struct {
	Outcome Outcome
	// Forward is the request to forward and its credential, set when Outcome is OutcomeForward. Its request may
	// differ from the one decided: a mount the caller may not read from is dropped.
	Forward registry.Forward
	// Policy is the name of the policy that allowed PAT-backed access, if any.
	Policy string
	// Message is the 403 message when Outcome is OutcomeDenied. It names only the caller's namespace and
	// ServiceAccount, the repository and the action.
	Message string
}

// Authorizer decides requests with the policies of an Index.
type Authorizer struct {
	Index *Index
}

// Decide decides a classified request for identity. The zero Identity is an anonymous caller. A request gets
// PAT-backed access only when a policy in the caller's own namespace lists its ServiceAccount and that policy's
// effective policy allows the action on the repository.
func (a *Authorizer) Decide(identity authn.Identity, request registry.Request) Decision {
	if identity.ServiceAccount == "" {
		return decideAnonymous(request)
	}

	if !a.Index.Synced() {
		return Decision{Outcome: OutcomeUnavailable}
	}

	if request.Route == registry.RouteBase {
		return Decision{Outcome: OutcomeVersionCheck}
	}

	found := a.Index.lookup(identity.Namespace, identity.ServiceAccount, request.Name, request.Action)

	if request.Action == v1alpha1.ActionPull {
		if found.policy == "" {
			return forward(request, registry.CredentialAnonymous, "")
		}

		return forward(request, registry.CredentialPAT, found.policy)
	}

	if found.policy == "" {
		return Decision{Outcome: OutcomeDenied, Message: deniedMessage(identity, request, found.requested)}
	}

	if request.Mount != nil {
		source := a.Index.lookup(identity.Namespace, identity.ServiceAccount, request.Mount.From, v1alpha1.ActionPull)
		if source.policy == "" {
			request.Mount = nil
		}
	}

	return forward(request, registry.CredentialPAT, found.policy)
}

func decideAnonymous(request registry.Request) Decision {
	if request.Action == v1alpha1.ActionPull {
		return Decision{
			Outcome: OutcomeForward,
			Forward: registry.Forward{Request: request, Credential: registry.CredentialAnonymous, AnonymousCaller: true},
		}
	}

	return Decision{Outcome: OutcomeChallenge}
}

func forward(request registry.Request, credential registry.Credential, policy string) Decision {
	return Decision{
		Outcome: OutcomeForward,
		Forward: registry.Forward{Request: request, Credential: credential},
		Policy:  policy,
	}
}

func deniedMessage(identity authn.Identity, request registry.Request, requested bool) string {
	reason := "no GHCRAccessPolicy in the namespace allows it"
	if requested {
		reason = "the namespace's grant does not cover it"
	}

	return fmt.Sprintf("ServiceAccount %s/%s may not %s repository %s: %s",
		identity.Namespace, identity.ServiceAccount, request.Action, request.Name, reason)
}
