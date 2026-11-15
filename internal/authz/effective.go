// Package authz decides what a caller may do: the effective policy of a GHCRAccessPolicy under the namespace's
// grants. The status controller and the authorizer both use Effective, so status and enforcement can't disagree.
package authz

import (
	"fmt"

	"github.com/solidDoWant/ghcr-proxy/api/v1alpha1"
	"github.com/solidDoWant/ghcr-proxy/internal/config"
	"github.com/solidDoWant/ghcr-proxy/internal/glob"
)

// Ready condition reasons, highest precedence first.
const (
	ReasonNoGrant        = "NoGrant"
	ReasonInvalidPattern = "InvalidPattern"
	ReasonOutsideGrant   = "OutsideGrant"
	ReasonAccepted       = "Accepted"
)

// actionOrder is the order actions are listed in.
var actionOrder = []v1alpha1.Action{v1alpha1.ActionPull, v1alpha1.ActionPush, v1alpha1.ActionDelete}

// Allowed is one repository pattern of a policy with the actions the grants let it keep.
type Allowed struct {
	Pattern glob.Glob
	Actions []v1alpha1.Action
}

// Allows reports whether this entry allows action on the repository name.
func (a Allowed) Allows(name string, action v1alpha1.Action) bool {
	for _, kept := range a.Actions {
		if kept == action {
			return a.Pattern.Match(name)
		}
	}

	return false
}

// EffectivePolicy is what a policy allows once its namespace's grants have capped it, and what was dropped.
type EffectivePolicy struct {
	// Allowed holds every (pattern, action) pair the policy keeps. Actions include the pull that push and delete
	// imply.
	Allowed []Allowed
	// Rejected holds one entry per (rule, pattern) that lost at least one action.
	Rejected []v1alpha1.RejectedRule
	// Reason is the Ready condition reason: ReasonNoGrant, ReasonInvalidPattern, ReasonOutsideGrant or
	// ReasonAccepted.
	Reason string
	// Message is the Ready condition message: fixed text and counts only.
	Message string
}

// Ready reports whether the whole policy is inside its grants.
func (e EffectivePolicy) Ready() bool { return e.Reason == ReasonAccepted }

// Allows reports whether the effective policy allows action on the repository name.
func (e EffectivePolicy) Allows(name string, action v1alpha1.Action) bool {
	for _, allowed := range e.Allowed {
		if allowed.Allows(name, action) {
			return true
		}
	}

	return false
}

type parsedGrant struct {
	patterns []glob.Glob
	actions  map[v1alpha1.Action]bool
}

// Effective caps policy by grants, the grants that apply to the policy's namespace (config.Grants.For). A
// (pattern, action) pair is kept only if some grant has a pattern that contains the policy's pattern and allows the
// action. A policy pattern outside the glob grammar keeps nothing. With no grants, nothing is kept.
func Effective(policy *v1alpha1.GHCRAccessPolicy, grants []config.ExpandedGrant) EffectivePolicy {
	parsed := parseGrants(grants)

	var (
		out                          EffectivePolicy
		total, invalid, outsideGrant int
	)

	for ruleIndex, rule := range policy.Spec.Rules {
		wanted := implied(rule.Actions)

		for _, pattern := range rule.Repositories {
			total++

			rejected := v1alpha1.RejectedRule{Rule: int32(ruleIndex), Repository: pattern} //nolint:gosec // spec.rules has at most 32 items.

			policyGlob, err := glob.Parse(pattern)

			switch {
			case len(parsed) == 0:
				rejected.Reason = v1alpha1.RejectedNoGrant
				rejected.Actions = wanted
			case err != nil:
				rejected.Reason = v1alpha1.RejectedInvalidPattern
				rejected.Actions = wanted
				invalid++
			default:
				var kept []v1alpha1.Action

				for _, action := range wanted {
					if grantsAllow(parsed, policyGlob, action) {
						kept = append(kept, action)
					} else {
						rejected.Actions = append(rejected.Actions, action)
					}
				}

				if len(kept) > 0 {
					out.Allowed = append(out.Allowed, Allowed{Pattern: policyGlob, Actions: kept})
				}

				if len(rejected.Actions) > 0 {
					rejected.Reason = v1alpha1.RejectedOutsideGrant
					outsideGrant++
				}
			}

			if len(rejected.Actions) > 0 {
				out.Rejected = append(out.Rejected, rejected)
			}
		}
	}

	switch {
	case len(parsed) == 0:
		out.Reason = ReasonNoGrant
		out.Message = fmt.Sprintf("no grant applies to this namespace; all %d repository patterns are ignored", total)
	case invalid > 0:
		out.Reason = ReasonInvalidPattern
		out.Message = fmt.Sprintf("%d of %d repository patterns are not valid patterns; see status.rejected", invalid, total)
	case outsideGrant > 0:
		out.Reason = ReasonOutsideGrant
		out.Message = fmt.Sprintf("%d of %d repository patterns lose actions outside the namespace's grant; see status.rejected",
			outsideGrant, total)
	default:
		out.Reason = ReasonAccepted
		out.Message = fmt.Sprintf("all %d repository patterns are inside the namespace's grant", total)
	}

	return out
}

func parseGrants(grants []config.ExpandedGrant) []parsedGrant {
	out := make([]parsedGrant, 0, len(grants))

	for _, grant := range grants {
		parsed := parsedGrant{actions: map[v1alpha1.Action]bool{}}

		for _, pattern := range grant.Patterns {
			// The config loader only expands patterns that satisfy the grammar; one that doesn't widens nothing.
			if grantGlob, err := glob.Parse(pattern); err == nil {
				parsed.patterns = append(parsed.patterns, grantGlob)
			}
		}

		var actions []v1alpha1.Action
		for _, action := range grant.Actions {
			actions = append(actions, v1alpha1.Action(action))
		}

		for _, action := range implied(actions) {
			parsed.actions[action] = true
		}

		out = append(out, parsed)
	}

	return out
}

func grantsAllow(grants []parsedGrant, pattern glob.Glob, action v1alpha1.Action) bool {
	for _, grant := range grants {
		if !grant.actions[action] {
			continue
		}

		for _, grantGlob := range grant.patterns {
			if glob.Contains(grantGlob, pattern) {
				return true
			}
		}
	}

	return false
}

// implied returns actions with the pull that push and delete imply, without duplicates, in a fixed order.
func implied(actions []v1alpha1.Action) []v1alpha1.Action {
	set := map[v1alpha1.Action]bool{}

	for _, action := range actions {
		switch action {
		case v1alpha1.ActionPull, v1alpha1.ActionPush, v1alpha1.ActionDelete:
			set[action] = true
			set[v1alpha1.ActionPull] = true
		}
	}

	out := make([]v1alpha1.Action, 0, len(set))

	for _, action := range actionOrder {
		if set[action] {
			out = append(out, action)
		}
	}

	return out
}
