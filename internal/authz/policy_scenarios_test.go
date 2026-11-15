package authz_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/ghcr-proxy/api/v1alpha1"
	"github.com/solidDoWant/ghcr-proxy/internal/authn"
	"github.com/solidDoWant/ghcr-proxy/internal/authz"
	"github.com/solidDoWant/ghcr-proxy/internal/registry"
)

func TestDecideRefusesPushesOutsideTheGrantWhileInsideOnesWork(t *testing.T) {
	index, informer := watchedIndex(t, loadGrants(t, chartGrant))
	informer.Add(namedPolicy("ci-foo", "access", []string{"builder"},
		accessRule([]string{"soliddowant/bar", "soliddowant/foo/x"}, v1alpha1.ActionPush)))

	authorizer := &authz.Authorizer{Index: index}

	outside := authorizer.Decide(builder, classify(t, http.MethodPut, "/v2/soliddowant/bar/manifests/latest"))
	assert.Equal(t, authz.OutcomeDenied, outside.Outcome)

	inside := authorizer.Decide(builder, classify(t, http.MethodPut, "/v2/soliddowant/foo/x/manifests/latest"))
	require.Equal(t, authz.OutcomeForward, inside.Outcome)
	assert.Equal(t, registry.CredentialPAT, inside.Forward.Credential)
}

func TestDecideGivesPoliciesInANamespaceWithNoGrantNoAccess(t *testing.T) {
	index, informer := watchedIndex(t, loadGrants(t, chartGrant))
	informer.Add(namedPolicy("team-x", "access", []string{"builder"},
		accessRule([]string{"soliddowant/foo"}, v1alpha1.ActionPush)))

	authorizer := &authz.Authorizer{Index: index}
	caller := authn.Identity{Namespace: "team-x", ServiceAccount: "builder"}

	push := authorizer.Decide(caller, classify(t, http.MethodPut, "/v2/soliddowant/foo/manifests/latest"))
	assert.Equal(t, authz.OutcomeDenied, push.Outcome)

	pull := authorizer.Decide(caller, classify(t, http.MethodGet, "/v2/soliddowant/foo/manifests/latest"))
	require.Equal(t, authz.OutcomeForward, pull.Outcome)
	assert.Equal(t, registry.CredentialAnonymous, pull.Forward.Credential)
}

func TestDecideMatchesGlobsAndChecksContainmentInTheGrant(t *testing.T) {
	const wideGrant = `  - namespaces: "ci-foo"
    repositories: ["soliddowant/**"]
    actions: [pull, push, delete]
`

	const narrowGrant = `  - namespaces: "ci-foo"
    repositories: ["soliddowant/foo/*"]
    actions: [pull, push, delete]
`

	tests := []struct {
		name    string
		grant   string
		pattern string
		allowed map[string]bool
	}{
		{
			name: "single star matches one segment only", grant: wideGrant, pattern: "soliddowant/foo/*",
			allowed: map[string]bool{"soliddowant/foo/a": true, "soliddowant/foo/a/b": false, "soliddowant/foo": false},
		},
		{
			name: "double star matches the repository and everything below", grant: wideGrant, pattern: "soliddowant/foo/**",
			allowed: map[string]bool{"soliddowant/foo/a": true, "soliddowant/foo/a/b": true, "soliddowant/foo": true},
		},
		{
			name: "double star is not inside a single star grant", grant: narrowGrant, pattern: "soliddowant/foo/**",
			allowed: map[string]bool{"soliddowant/foo/a": false, "soliddowant/foo/a/b": false, "soliddowant/foo": false},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			index, informer := watchedIndex(t, loadGrants(t, test.grant))
			informer.Add(namedPolicy("ci-foo", "access", []string{"builder"}, accessRule([]string{test.pattern}, v1alpha1.ActionPush)))

			authorizer := &authz.Authorizer{Index: index}

			for repository, allowed := range test.allowed {
				decision := authorizer.Decide(builder, classify(t, http.MethodPut, "/v2/"+repository+"/manifests/latest"))
				assert.Equal(t, allowed, decision.Outcome == authz.OutcomeForward, repository)
			}
		})
	}
}
