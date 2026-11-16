package authz_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllertest"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/authn"
	"github.com/solidDoWant/registrygate/internal/authz"
	"github.com/solidDoWant/registrygate/internal/config"
	"github.com/solidDoWant/registrygate/internal/registry"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// chartGrant gives each ci-<repo> namespace soliddowant/<repo> and everything below it.
	chartGrant = `  - namespaces: "ci-(?P<repo>[a-z0-9-]+)"
    repositories: ["soliddowant/${repo}", "soliddowant/${repo}/**"]
    actions: [pull, push, delete]
`
)

func loadGrants(t *testing.T, grants string) config.Grants {
	t.Helper()

	pat := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(pat, []byte("x"), 0o600))

	cfg, err := config.Parse([]byte("upstream:\n  patFile: " + pat + "\ngrants:\n" + grants))
	require.NoError(t, err)

	return cfg.Grants
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	return scheme
}

// watchedIndex returns an index fed by a synced fake informer, and that informer.
func watchedIndex(t *testing.T, grants config.Grants) (*authz.Index, *controllertest.FakeInformer) {
	t.Helper()

	informers := &informertest.FakeInformers{Scheme: newScheme(t)}
	informer, err := informers.FakeInformerFor(t.Context(), &v1alpha1.GHCRAccessPolicy{})
	require.NoError(t, err)

	index := authz.NewIndex(grants)
	require.NoError(t, index.Watch(t.Context(), informers))

	return index, informer
}

func namedPolicy(namespace, name string, serviceAccounts []string, rules ...v1alpha1.GHCRAccessRule) *v1alpha1.GHCRAccessPolicy {
	return &v1alpha1.GHCRAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       v1alpha1.GHCRAccessPolicySpec{ServiceAccounts: serviceAccounts, Rules: rules},
	}
}

func accessRule(repositories []string, actions ...v1alpha1.Action) v1alpha1.GHCRAccessRule {
	return v1alpha1.GHCRAccessRule{Repositories: repositories, Actions: actions}
}

func classify(t *testing.T, method, target string) registry.Request {
	t.Helper()

	request, err := registry.Classify(httptest.NewRequest(method, target, nil))
	require.NoError(t, err)

	return request
}

var (
	builder   = authn.Identity{Namespace: "ci-foo", ServiceAccount: "builder"}
	anonymous = authn.Identity{}
)

func TestDecideFollowsTheDecisionTable(t *testing.T) {
	grants := loadGrants(t, chartGrant)
	index, informer := watchedIndex(t, grants)
	informer.Add(namedPolicy("ci-foo", "push-foo", []string{"builder"},
		accessRule([]string{"soliddowant/foo/app"}, v1alpha1.ActionPush),
		accessRule([]string{"soliddowant/foo/source"}, v1alpha1.ActionPull),
		accessRule([]string{"soliddowant/foo/old"}, v1alpha1.ActionDelete),
	))

	authorizer := &authz.Authorizer{Index: index}

	mountURL := "/v2/soliddowant/foo/app/blobs/uploads/?mount=" + digestA + "&from="

	tests := []struct {
		name       string
		identity   authn.Identity
		method     string
		target     string
		outcome    authz.Outcome
		credential registry.Credential
		anonCaller bool
		mountKept  bool
		policy     string
	}{
		{name: "anonymous version check is challenged", identity: anonymous, method: http.MethodGet, target: "/v2/", outcome: authz.OutcomeChallenge},
		{name: "ServiceAccount version check is answered locally", identity: builder, method: http.MethodGet, target: "/v2/", outcome: authz.OutcomeVersionCheck},
		{
			name: "allowed pull is PAT-backed", identity: builder, method: http.MethodGet, target: "/v2/soliddowant/foo/source/manifests/latest",
			outcome: authz.OutcomeForward, credential: registry.CredentialPAT, policy: "push-foo",
		},
		{
			name: "pull outside the policy is anonymous", identity: builder, method: http.MethodGet, target: "/v2/soliddowant/foo/other/manifests/latest",
			outcome: authz.OutcomeForward, credential: registry.CredentialAnonymous,
		},
		{
			name: "anonymous pull is anonymous", identity: anonymous, method: http.MethodGet, target: "/v2/soliddowant/foo/source/manifests/latest",
			outcome: authz.OutcomeForward, credential: registry.CredentialAnonymous, anonCaller: true,
		},
		{
			name: "allowed push is PAT-backed", identity: builder, method: http.MethodPut, target: "/v2/soliddowant/foo/app/manifests/latest",
			outcome: authz.OutcomeForward, credential: registry.CredentialPAT, policy: "push-foo",
		},
		{name: "push outside the policy is denied", identity: builder, method: http.MethodPut, target: "/v2/soliddowant/foo/source/manifests/latest", outcome: authz.OutcomeDenied},
		{name: "anonymous push is challenged", identity: anonymous, method: http.MethodPut, target: "/v2/soliddowant/foo/app/manifests/latest", outcome: authz.OutcomeChallenge},
		{name: "anonymous delete is challenged", identity: anonymous, method: http.MethodDelete, target: "/v2/soliddowant/foo/old/manifests/latest", outcome: authz.OutcomeChallenge},
		{
			name: "mount from a readable repository is kept", identity: builder, method: http.MethodPost, target: mountURL + "soliddowant/foo/source",
			outcome: authz.OutcomeForward, credential: registry.CredentialPAT, mountKept: true, policy: "push-foo",
		},
		{
			name: "mount from an unreadable repository is dropped", identity: builder, method: http.MethodPost, target: mountURL + "soliddowant/foo/other",
			outcome: authz.OutcomeForward, credential: registry.CredentialPAT, policy: "push-foo",
		},
		{
			name: "mount without push is denied", identity: builder, method: http.MethodPost,
			target: "/v2/soliddowant/foo/source/blobs/uploads/?mount=" + digestA + "&from=soliddowant/foo/app", outcome: authz.OutcomeDenied,
		},
		{
			name: "allowed delete is PAT-backed", identity: builder, method: http.MethodDelete, target: "/v2/soliddowant/foo/old/manifests/latest",
			outcome: authz.OutcomeForward, credential: registry.CredentialPAT, policy: "push-foo",
		},
		{name: "delete outside the policy is denied", identity: builder, method: http.MethodDelete, target: "/v2/soliddowant/foo/app/manifests/latest", outcome: authz.OutcomeDenied},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := classify(t, test.method, test.target)
			decision := authorizer.Decide(test.identity, request)

			require.Equal(t, test.outcome, decision.Outcome)
			assert.Equal(t, test.policy, decision.Policy)

			if test.outcome != authz.OutcomeForward {
				assert.Equal(t, registry.Forward{}, decision.Forward)
				return
			}

			assert.Equal(t, test.credential, decision.Forward.Credential)
			assert.Equal(t, test.anonCaller, decision.Forward.AnonymousCaller)
			assert.Equal(t, request.Name, decision.Forward.Request.Name)

			if request.Mount != nil {
				assert.Equal(t, test.mountKept, decision.Forward.Request.Mount != nil)
			}
		})
	}
}

func TestDecideDeniedMessageSaysWhetherAPolicyOrTheGrantRefused(t *testing.T) {
	index, informer := watchedIndex(t, loadGrants(t, chartGrant))
	informer.Add(namedPolicy("ci-foo", "outside", []string{"builder"}, accessRule([]string{"soliddowant/bar"}, v1alpha1.ActionPush)))

	authorizer := &authz.Authorizer{Index: index}

	outsideGrant := authorizer.Decide(builder, classify(t, http.MethodPut, "/v2/soliddowant/bar/manifests/latest"))
	require.Equal(t, authz.OutcomeDenied, outsideGrant.Outcome)
	assert.Equal(t, "ServiceAccount ci-foo/builder may not push repository soliddowant/bar: the namespace's grant does not cover it",
		outsideGrant.Message)

	noPolicy := authorizer.Decide(builder, classify(t, http.MethodDelete, "/v2/soliddowant/foo/x/manifests/latest"))
	require.Equal(t, authz.OutcomeDenied, noPolicy.Outcome)
	assert.Equal(t, "ServiceAccount ci-foo/builder may not delete repository soliddowant/foo/x: no GHCRAccessPolicy in the namespace allows it",
		noPolicy.Message)
}

func TestDecideGivesAServiceAccountNoPolicyListsOnlyAnonymousPulls(t *testing.T) {
	index, informer := watchedIndex(t, loadGrants(t, chartGrant))
	informer.Add(namedPolicy("ci-foo", "builder", []string{"builder"}, accessRule([]string{"soliddowant/foo/**"}, v1alpha1.ActionPush)))

	authorizer := &authz.Authorizer{Index: index}
	other := authn.Identity{Namespace: "ci-foo", ServiceAccount: "other"}

	pull := authorizer.Decide(other, classify(t, http.MethodGet, "/v2/soliddowant/foo/app/manifests/latest"))
	require.Equal(t, authz.OutcomeForward, pull.Outcome)
	assert.Equal(t, registry.CredentialAnonymous, pull.Forward.Credential)

	push := authorizer.Decide(other, classify(t, http.MethodPut, "/v2/soliddowant/foo/app/manifests/latest"))
	assert.Equal(t, authz.OutcomeDenied, push.Outcome)
}

func TestDecideIgnoresPoliciesInOtherNamespaces(t *testing.T) {
	grants := loadGrants(t, `  - namespaces: "ci-.*"
    repositories: ["soliddowant/**"]
    actions: [pull, push, delete]
`)
	index, informer := watchedIndex(t, grants)
	informer.Add(namedPolicy("ci-a", "builder", []string{"builder"}, accessRule([]string{"soliddowant/r"}, v1alpha1.ActionPush)))

	authorizer := &authz.Authorizer{Index: index}
	inA := authn.Identity{Namespace: "ci-a", ServiceAccount: "builder"}
	inB := authn.Identity{Namespace: "ci-b", ServiceAccount: "builder"}
	push := classify(t, http.MethodPut, "/v2/soliddowant/r/manifests/latest")
	pull := classify(t, http.MethodGet, "/v2/soliddowant/r/manifests/latest")

	require.Equal(t, authz.OutcomeForward, authorizer.Decide(inA, push).Outcome)

	assert.Equal(t, authz.OutcomeDenied, authorizer.Decide(inB, push).Outcome)

	pullDecision := authorizer.Decide(inB, pull)
	require.Equal(t, authz.OutcomeForward, pullDecision.Outcome)
	assert.Equal(t, registry.CredentialAnonymous, pullDecision.Forward.Credential)
}

func TestDecideKeepsActionsSeparate(t *testing.T) {
	index, informer := watchedIndex(t, loadGrants(t, chartGrant))
	informer.Add(namedPolicy("ci-foo", "access", []string{"builder"},
		accessRule([]string{"soliddowant/foo/read"}, v1alpha1.ActionPull),
		accessRule([]string{"soliddowant/foo/write"}, v1alpha1.ActionPush),
	))

	authorizer := &authz.Authorizer{Index: index}

	tests := []struct {
		name       string
		method     string
		target     string
		outcome    authz.Outcome
		credential registry.Credential
	}{
		{name: "pull-only allows pull", method: http.MethodGet, target: "/v2/soliddowant/foo/read/manifests/latest", outcome: authz.OutcomeForward, credential: registry.CredentialPAT},
		{name: "pull-only refuses push", method: http.MethodPut, target: "/v2/soliddowant/foo/read/manifests/latest", outcome: authz.OutcomeDenied},
		{name: "pull-only refuses delete", method: http.MethodDelete, target: "/v2/soliddowant/foo/read/manifests/latest", outcome: authz.OutcomeDenied},
		{name: "push allows pull", method: http.MethodGet, target: "/v2/soliddowant/foo/write/manifests/latest", outcome: authz.OutcomeForward, credential: registry.CredentialPAT},
		{name: "push does not allow delete", method: http.MethodDelete, target: "/v2/soliddowant/foo/write/manifests/latest", outcome: authz.OutcomeDenied},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := authorizer.Decide(builder, classify(t, test.method, test.target))
			require.Equal(t, test.outcome, decision.Outcome)
			assert.Equal(t, test.credential, decision.Forward.Credential)
		})
	}
}

func TestDecideFollowsPolicyChangesFromTheInformer(t *testing.T) {
	index, informer := watchedIndex(t, loadGrants(t, chartGrant))
	authorizer := &authz.Authorizer{Index: index}
	push := classify(t, http.MethodPut, "/v2/soliddowant/foo/app/manifests/latest")

	pullOnly := namedPolicy("ci-foo", "access", []string{"builder"}, accessRule([]string{"soliddowant/foo/app"}, v1alpha1.ActionPull))
	informer.Add(pullOnly)
	require.Equal(t, authz.OutcomeDenied, authorizer.Decide(builder, push).Outcome)

	withPush := namedPolicy("ci-foo", "access", []string{"builder"}, accessRule([]string{"soliddowant/foo/app"}, v1alpha1.ActionPush))
	informer.Update(pullOnly, withPush)
	require.Equal(t, authz.OutcomeForward, authorizer.Decide(builder, push).Outcome)

	informer.Delete(withPush)
	assert.Equal(t, authz.OutcomeDenied, authorizer.Decide(builder, push).Outcome)
}

func TestDecideIsUnavailableUntilTheInformerSyncs(t *testing.T) {
	grants := loadGrants(t, chartGrant)
	pull := classify(t, http.MethodGet, "/v2/soliddowant/foo/app/manifests/latest")

	t.Run("never watched", func(t *testing.T) {
		authorizer := &authz.Authorizer{Index: authz.NewIndex(grants)}
		assert.Equal(t, authz.OutcomeUnavailable, authorizer.Decide(builder, pull).Outcome)
		assert.Equal(t, authz.OutcomeUnavailable, authorizer.Decide(builder, classify(t, http.MethodGet, "/v2/")).Outcome)
	})

	t.Run("informer not synced", func(t *testing.T) {
		informers := &informertest.FakeInformers{Scheme: newScheme(t)}
		informer := controllertest.NewFakeInformer()
		gvk := v1alpha1.GroupVersion.WithKind("GHCRAccessPolicy")
		informers.InformersByGVK = map[schema.GroupVersionKind]toolscache.SharedIndexInformer{gvk: informer}

		index := authz.NewIndex(grants)
		require.NoError(t, index.Watch(t.Context(), informers))

		authorizer := &authz.Authorizer{Index: index}
		assert.Equal(t, authz.OutcomeUnavailable, authorizer.Decide(builder, pull).Outcome)

		informer.Synced()
		assert.Equal(t, authz.OutcomeForward, authorizer.Decide(builder, pull).Outcome)
	})

	t.Run("anonymous pulls do not need the index", func(t *testing.T) {
		authorizer := &authz.Authorizer{Index: authz.NewIndex(grants)}
		assert.Equal(t, authz.OutcomeForward, authorizer.Decide(anonymous, pull).Outcome)
	})
}

func TestCountsSplitPoliciesByWhetherTheyAreInsideTheirGrants(t *testing.T) {
	index, informer := watchedIndex(t, loadGrants(t, chartGrant))

	ready, notReady := index.Counts()
	assert.Zero(t, ready+notReady)

	informer.Add(namedPolicy("ci-foo", "inside", []string{"builder"}, accessRule([]string{"soliddowant/foo/app"}, v1alpha1.ActionPush)))
	informer.Add(namedPolicy("ci-foo", "outside", []string{"builder"}, accessRule([]string{"soliddowant/bar/app"}, v1alpha1.ActionPush)))
	informer.Add(namedPolicy("other", "no-grant", []string{"builder"}, accessRule([]string{"soliddowant/foo/app"}, v1alpha1.ActionPush)))

	ready, notReady = index.Counts()
	assert.Equal(t, 1, ready)
	assert.Equal(t, 2, notReady)

	informer.Delete(namedPolicy("ci-foo", "outside", nil))

	ready, notReady = index.Counts()
	assert.Equal(t, 1, ready)
	assert.Equal(t, 1, notReady)
}
