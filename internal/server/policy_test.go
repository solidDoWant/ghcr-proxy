package server_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/authn"
	"github.com/solidDoWant/registrygate/internal/secrets"
	"github.com/solidDoWant/registrygate/test/fakeregistry"
)

// patEntries are the requests the upstream saw that carried the credential or a token made from it.
func patEntries(fake *fakeregistry.Fake) []fakeregistry.Entry {
	var out []fakeregistry.Entry

	for _, entry := range fake.Log() {
		if entry.Credential == fakeregistry.CredPATToken || entry.Credential == fakeregistry.CredPATBasic {
			out = append(out, entry)
		}
	}

	return out
}

func TestTokenWithTheProxyAudienceAuthenticates(t *testing.T) {
	reviewer, err := authn.NewReviewer(&tokenReviews{}, authn.Options{
		Audience: "registrygate", CacheTTL: time.Second, ReviewTimeout: 5 * time.Second,
	})
	require.NoError(t, err)

	proxy := newHarness(t, harnessOptions{reviewer: reviewer})

	assert.Equal(t, http.StatusOK, proxy.do(t, http.MethodGet, "/v2/", basic("token-for-proxy")).status)

	push := proxy.do(t, http.MethodPost, "/v2/soliddowant/foo/app/blobs/uploads/", basic("token-for-proxy"))
	assert.Equal(t, http.StatusAccepted, push.status, push.body)
	assert.NotEmpty(t, patEntries(proxy.fake), "the authenticated caller's policy applies")
}

func TestRequestWithoutCredentialIsAnonymous(t *testing.T) {
	proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
		objects.upstream.Spec.AnonymousPulls = v1alpha1.AnonymousPullsForward
	}})

	image, err := random.Image(256, 1)
	require.NoError(t, err)
	require.NoError(t, proxy.fake.Push("soliddowant/foo/source", image))
	proxy.fake.MakePublic("soliddowant/foo/source")
	proxy.fake.ResetLog()

	// The repository is one the builder's policy may pull with the credential; an anonymous caller gets none of it.
	got := proxy.do(t, http.MethodGet, "/v2/soliddowant/foo/source/manifests/latest")
	assert.Equal(t, http.StatusOK, got.status, got.body)
	assert.NotEmpty(t, proxy.fake.Log(), "the pull is forwarded")
	assert.Empty(t, patEntries(proxy.fake), "an anonymous pull never uses the credential")
}

func TestAnonymousPushIsChallengedAndSendsNothingUpstream(t *testing.T) {
	proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
		objects.upstream.Spec.AnonymousPulls = v1alpha1.AnonymousPullsForward
	}})

	for _, request := range []struct{ method, target string }{
		{http.MethodPost, "/v2/soliddowant/foo/app/blobs/uploads/"},
		{http.MethodPut, "/v2/soliddowant/foo/app/manifests/1"},
	} {
		got := proxy.do(t, request.method, request.target)

		assert.Equal(t, http.StatusUnauthorized, got.status, request.target)
		assert.Equal(t, `Basic realm="registrygate"`, got.header.Get("WWW-Authenticate"), request.target)
	}

	assert.Empty(t, proxy.fake.Log())
}

func TestRuleOutsideItsCeilingIsIgnoredAndOtherRulesStillApply(t *testing.T) {
	proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
		objects.policy.Spec.Rules = append(objects.policy.Spec.Rules, v1alpha1.AccessRule{
			Name: "too-much", CredentialRef: ref("shared"),
			Repositories: []string{"soliddowant/bar/app"}, Actions: []v1alpha1.Action{v1alpha1.ActionPush},
		})
	}})

	outside := proxy.do(t, http.MethodPost, "/v2/soliddowant/bar/app/blobs/uploads/", basic(builderToken))
	assert.Equal(t, http.StatusForbidden, outside.status)
	assert.Equal(t, "DENIED", outside.code)
	assert.Empty(t, proxy.fake.Log(), "the rule outside the ceiling is ignored")

	inside := proxy.do(t, http.MethodPost, "/v2/soliddowant/foo/app/blobs/uploads/", basic(builderToken))
	assert.Equal(t, http.StatusAccepted, inside.status, inside.body)
	assert.NotEmpty(t, patEntries(proxy.fake))
}

func TestWithdrawnAdmissionStopsTheNextRequestUsingIt(t *testing.T) {
	tests := []struct {
		name     string
		withdraw func(*objects)
	}{
		{name: "credential", withdraw: func(objects *objects) { objects.credential.Spec.AllowedNamespaces.Names = []string{adminNamespace} }},
		{name: "upstream", withdraw: func(objects *objects) { objects.upstream.Spec.AllowedNamespaces.Names = []string{adminNamespace} }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy := newHarness(t, harnessOptions{})

			target := "/v2/soliddowant/foo/app/blobs/uploads/"
			require.Equal(t, http.StatusAccepted, proxy.do(t, http.MethodPost, target, basic(builderToken)).status)

			test.withdraw(proxy.objects)
			proxy.rebuild(t)
			proxy.fake.ResetLog()

			got := proxy.do(t, http.MethodPost, target, basic(builderToken))
			assert.Equal(t, http.StatusForbidden, got.status)
			assert.Empty(t, proxy.fake.Log())
		})
	}
}

func TestEachRepositoryIsPushedWithItsOwnCredential(t *testing.T) {
	// The second credential's password is not one the upstream accepts, so a push that reaches the upstream with
	// the first credential's PAT shows which credential the proxy chose.
	proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
		second := objects.credential.DeepCopy()
		second.Name, second.UID = "second", "second-uid"
		second.Spec.SecretRef.Name = "second-credential"
		objects.moreCredentials = append(objects.moreCredentials, *second)
		objects.secrets[secrets.Key{Namespace: adminNamespace, Name: "second-credential"}] = secrets.Secret{
			State: secrets.Present, ResourceVersion: "1",
			Data: secrets.Data{"username": []byte("second"), "password": []byte("not-the-pat")},
		}
		objects.policy.Spec.Rules = append(objects.policy.Spec.Rules, v1alpha1.AccessRule{
			Name: "tools", CredentialRef: ref("second"),
			Repositories: []string{"soliddowant/foo/tools"}, Actions: []v1alpha1.Action{v1alpha1.ActionPush},
		})
	}})

	first := proxy.do(t, http.MethodPost, "/v2/soliddowant/foo/app/blobs/uploads/", basic(builderToken))
	assert.Equal(t, http.StatusAccepted, first.status, first.body)
	assert.NotEmpty(t, patEntries(proxy.fake), "the first repository is pushed with the first credential")

	proxy.fake.ResetLog()

	second := proxy.do(t, http.MethodPost, "/v2/soliddowant/foo/tools/blobs/uploads/", basic(builderToken))
	assert.NotEqual(t, http.StatusAccepted, second.status)
	assert.NotEmpty(t, proxy.fake.Log(), "the second repository is sent upstream")
	assert.Empty(t, patEntries(proxy.fake), "the second repository is pushed with the second credential, never the first")
}

func TestAmbiguousRulesAre403NamingEachRule(t *testing.T) {
	proxy := newHarness(t, harnessOptions{configure: func(objects *objects) {
		second := objects.credential.DeepCopy()
		second.Name, second.UID = "second", "second-uid"
		objects.moreCredentials = append(objects.moreCredentials, *second)
		objects.policy.Spec.Rules = append(objects.policy.Spec.Rules, v1alpha1.AccessRule{
			Name: "app-again", CredentialRef: ref("second"),
			Repositories: []string{"soliddowant/foo/app"}, Actions: []v1alpha1.Action{v1alpha1.ActionPush},
		})
	}})

	got := proxy.do(t, http.MethodPost, "/v2/soliddowant/foo/app/blobs/uploads/", basic(builderToken))

	assert.Equal(t, http.StatusForbidden, got.status)
	assert.Equal(t, "DENIED", got.code)
	assert.Contains(t, got.body, "ci-foo/build/app,")
	assert.Contains(t, got.body, "ci-foo/build/app-again")
	assert.Empty(t, proxy.fake.Log())
}

func TestRefusedCredentialIsNeverServedAsAnonymous(t *testing.T) {
	reviewer, err := authn.NewReviewer(&tokenReviews{}, authn.Options{
		Audience: "registrygate", CacheTTL: time.Second, ReviewTimeout: 5 * time.Second,
	})
	require.NoError(t, err)

	// With anonymous pulls forwarded, a refused credential that fell back to anonymous would reach the upstream.
	proxy := newHarness(t, harnessOptions{reviewer: reviewer, configure: func(objects *objects) {
		objects.upstream.Spec.AnonymousPulls = v1alpha1.AnonymousPullsForward
	}})

	image, err := random.Image(256, 1)
	require.NoError(t, err)
	require.NoError(t, proxy.fake.Push("other/public", image))
	proxy.fake.MakePublic("other/public")

	tests := []struct {
		name          string
		authorization string
	}{
		{name: "token without the proxy audience", authorization: basic("token-for-api-server")},
		{name: "credential that is not a token", authorization: basic("not-a-token")},
		{name: "bearer scheme", authorization: "Bearer token-for-proxy"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy.fake.ResetLog()

			got := proxy.do(t, http.MethodGet, "/v2/other/public/manifests/latest", test.authorization)

			assert.Equal(t, http.StatusUnauthorized, got.status)
			assert.Equal(t, `Basic realm="registrygate"`, got.header.Get("WWW-Authenticate"))
			assert.Empty(t, proxy.fake.Log())
		})
	}
}
