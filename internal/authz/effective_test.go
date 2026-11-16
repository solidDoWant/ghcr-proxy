package authz_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/authz"
	"github.com/solidDoWant/registrygate/internal/config"
)

const (
	pull   = v1alpha1.ActionPull
	push   = v1alpha1.ActionPush
	delete = v1alpha1.ActionDelete
)

func policy(rules ...v1alpha1.GHCRAccessRule) *v1alpha1.GHCRAccessPolicy {
	return &v1alpha1.GHCRAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ci-foo", Name: "p"},
		Spec:       v1alpha1.GHCRAccessPolicySpec{ServiceAccounts: []string{"builder"}, Rules: rules},
	}
}

func rule(actions []v1alpha1.Action, repositories ...string) v1alpha1.GHCRAccessRule {
	return v1alpha1.GHCRAccessRule{Repositories: repositories, Actions: actions}
}

// fooGrant is the default grant expanded for the namespace ci-foo.
var fooGrant = config.ExpandedGrant{
	Patterns: []string{"soliddowant/foo", "soliddowant/foo/**"},
	Actions:  []config.Action{config.ActionPull, config.ActionPush, config.ActionDelete},
}

type check struct {
	name   string
	action v1alpha1.Action
	allows bool
}

func TestEffective(t *testing.T) {
	tests := []struct {
		name         string
		policy       *v1alpha1.GHCRAccessPolicy
		grants       []config.ExpandedGrant
		wantReason   string
		wantMessage  string
		wantRejected []v1alpha1.RejectedRule
		checks       []check
	}{
		{
			name:        "a policy fully inside its grant is accepted and keeps everything",
			policy:      policy(rule([]v1alpha1.Action{push}, "soliddowant/foo", "soliddowant/foo/*")),
			grants:      []config.ExpandedGrant{fooGrant},
			wantReason:  authz.ReasonAccepted,
			wantMessage: "all 2 repository patterns are inside the namespace's grant",
			checks: []check{
				{name: "soliddowant/foo", action: push, allows: true},
				{name: "soliddowant/foo", action: pull, allows: true},
				{name: "soliddowant/foo/x", action: push, allows: true},
				{name: "soliddowant/foo/x", action: delete, allows: false},
				{name: "soliddowant/foo/x/y", action: push, allows: false},
			},
		},
		{
			name: "a pattern outside the grant is rejected and dropped, the rest still applies",
			policy: policy(
				rule([]v1alpha1.Action{push}, "soliddowant/bar"),
				rule([]v1alpha1.Action{push}, "soliddowant/foo/**"),
			),
			grants:      []config.ExpandedGrant{fooGrant},
			wantReason:  authz.ReasonOutsideGrant,
			wantMessage: "1 of 2 repository patterns lose actions outside the namespace's grant; see status.rejected",
			wantRejected: []v1alpha1.RejectedRule{
				{Rule: 0, Repository: "soliddowant/bar", Actions: []v1alpha1.Action{pull, push}, Reason: v1alpha1.RejectedOutsideGrant},
			},
			checks: []check{
				{name: "soliddowant/bar", action: push, allows: false},
				{name: "soliddowant/bar", action: pull, allows: false},
				{name: "soliddowant/foo/x", action: push, allows: true},
			},
		},
		{
			name:   "a pattern wider than the grant's patterns is rejected",
			policy: policy(rule([]v1alpha1.Action{pull}, "soliddowant/**")),
			grants: []config.ExpandedGrant{fooGrant},
			wantRejected: []v1alpha1.RejectedRule{
				{Rule: 0, Repository: "soliddowant/**", Actions: []v1alpha1.Action{pull}, Reason: v1alpha1.RejectedOutsideGrant},
			},
			wantReason:  authz.ReasonOutsideGrant,
			wantMessage: "1 of 1 repository patterns lose actions outside the namespace's grant; see status.rejected",
			checks:      []check{{name: "soliddowant/foo", action: pull, allows: false}},
		},
		{
			name:   "actions the grant doesn't allow are dropped and the implied pull is kept",
			policy: policy(rule([]v1alpha1.Action{push, delete}, "soliddowant/foo")),
			grants: []config.ExpandedGrant{{
				Patterns: []string{"soliddowant/foo"},
				Actions:  []config.Action{config.ActionPull, config.ActionPush},
			}},
			wantReason:  authz.ReasonOutsideGrant,
			wantMessage: "1 of 1 repository patterns lose actions outside the namespace's grant; see status.rejected",
			wantRejected: []v1alpha1.RejectedRule{
				{Rule: 0, Repository: "soliddowant/foo", Actions: []v1alpha1.Action{delete}, Reason: v1alpha1.RejectedOutsideGrant},
			},
			checks: []check{
				{name: "soliddowant/foo", action: pull, allows: true},
				{name: "soliddowant/foo", action: push, allows: true},
				{name: "soliddowant/foo", action: delete, allows: false},
			},
		},
		{
			name:   "a grant of push alone also grants the implied pull",
			policy: policy(rule([]v1alpha1.Action{pull}, "soliddowant/foo")),
			grants: []config.ExpandedGrant{{
				Patterns: []string{"soliddowant/foo"},
				Actions:  []config.Action{config.ActionPush},
			}},
			wantReason:  authz.ReasonAccepted,
			wantMessage: "all 1 repository patterns are inside the namespace's grant",
			checks:      []check{{name: "soliddowant/foo", action: pull, allows: true}},
		},
		{
			name:   "each of several grants covers its own patterns and actions",
			policy: policy(rule([]v1alpha1.Action{push}, "soliddowant/foo", "shared/cache")),
			grants: []config.ExpandedGrant{fooGrant, {
				Patterns: []string{"shared/*"},
				Actions:  []config.Action{config.ActionPull},
			}},
			wantReason:  authz.ReasonOutsideGrant,
			wantMessage: "1 of 2 repository patterns lose actions outside the namespace's grant; see status.rejected",
			wantRejected: []v1alpha1.RejectedRule{
				{Rule: 0, Repository: "shared/cache", Actions: []v1alpha1.Action{push}, Reason: v1alpha1.RejectedOutsideGrant},
			},
			checks: []check{
				{name: "soliddowant/foo", action: push, allows: true},
				{name: "shared/cache", action: pull, allows: true},
				{name: "shared/cache", action: push, allows: false},
			},
		},
		{
			name:        "with no grant every pattern is rejected and nothing is allowed",
			policy:      policy(rule([]v1alpha1.Action{push}, "soliddowant/foo"), rule([]v1alpha1.Action{pull}, "Bad//x")),
			wantReason:  authz.ReasonNoGrant,
			wantMessage: "no grant applies to this namespace; all 2 repository patterns are ignored",
			wantRejected: []v1alpha1.RejectedRule{
				{Rule: 0, Repository: "soliddowant/foo", Actions: []v1alpha1.Action{pull, push}, Reason: v1alpha1.RejectedNoGrant},
				{Rule: 1, Repository: "Bad//x", Actions: []v1alpha1.Action{pull}, Reason: v1alpha1.RejectedNoGrant},
			},
			checks: []check{
				{name: "soliddowant/foo", action: pull, allows: false},
				{name: "soliddowant/foo", action: push, allows: false},
			},
		},
		{
			name: "an invalid pattern keeps nothing and takes precedence over outside the grant",
			policy: policy(
				rule([]v1alpha1.Action{pull}, "soliddowant/foo/[ab]"),
				rule([]v1alpha1.Action{pull}, "soliddowant/bar", "soliddowant/foo"),
			),
			grants:      []config.ExpandedGrant{fooGrant},
			wantReason:  authz.ReasonInvalidPattern,
			wantMessage: "1 of 3 repository patterns are not valid patterns; see status.rejected",
			wantRejected: []v1alpha1.RejectedRule{
				{Rule: 0, Repository: "soliddowant/foo/[ab]", Actions: []v1alpha1.Action{pull}, Reason: v1alpha1.RejectedInvalidPattern},
				{Rule: 1, Repository: "soliddowant/bar", Actions: []v1alpha1.Action{pull}, Reason: v1alpha1.RejectedOutsideGrant},
			},
			checks: []check{
				{name: "soliddowant/foo/a", action: pull, allows: false},
				{name: "soliddowant/foo", action: pull, allows: true},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := authz.Effective(test.policy, test.grants)

			assert.Equal(t, test.wantReason, got.Reason)
			assert.Equal(t, test.wantReason == authz.ReasonAccepted, got.Ready())
			assert.Equal(t, test.wantMessage, got.Message)
			assert.Equal(t, test.wantRejected, got.Rejected)

			for _, check := range test.checks {
				assert.Equal(t, check.allows, got.Allows(check.name, check.action), "%s on %s", check.action, check.name)
			}

			if test.wantReason == authz.ReasonNoGrant {
				require.Empty(t, got.Allowed)
			}
		})
	}
}
