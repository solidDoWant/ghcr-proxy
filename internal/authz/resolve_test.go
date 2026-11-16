package authz_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/authn"
	"github.com/solidDoWant/registrygate/internal/authz"
	"github.com/solidDoWant/registrygate/internal/model"
	"github.com/solidDoWant/registrygate/internal/registry"
	"github.com/solidDoWant/registrygate/internal/secrets"
)

const (
	resolveAdmin  = "registrygate"
	resolveTenant = "ci-a"
	resolveSA     = "builder"
	mountDigest   = digest.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
)

// resolveSecrets is a Secret store double. A key in pending is not tracked yet; a key in neither map is not found.
type resolveSecrets struct {
	values  map[secrets.Key]secrets.Secret
	pending map[secrets.Key]bool
}

func (s *resolveSecrets) Get(key secrets.Key) (secrets.Secret, bool) {
	if s.pending[key] {
		return secrets.Secret{}, false
	}

	if secret, ok := s.values[key]; ok {
		return secret, true
	}

	return secrets.Secret{State: secrets.NotFound}, true
}

func (s *resolveSecrets) set(namespace, name string, data map[string]string) {
	out := secrets.Data{}
	for key, value := range data {
		out[key] = []byte(value)
	}

	s.values[secrets.Key{Namespace: namespace, Name: name}] = secrets.Secret{State: secrets.Present, Data: out, ResourceVersion: "1"}
}

var servingPair = sync.OnceValues(func() (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		DNSNames:     []string{"ghcr.io", "registry.example"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", err
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})) + "\x00" +
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})), nil
})

func upstreamObject(name, host string, anonymousPulls string, defaultMirror *v1alpha1.ObjectRef) v1alpha1.UpstreamRegistry {
	return v1alpha1.UpstreamRegistry{
		ObjectMeta: metav1.ObjectMeta{Namespace: resolveAdmin, Name: name, Generation: 1},
		Spec: v1alpha1.UpstreamRegistrySpec{
			Hosts:             []string{host},
			URL:               "https://" + name + ".upstream.example",
			TLS:               v1alpha1.ServingTLS{SecretRef: v1alpha1.LocalRef{Name: "serving"}},
			AnonymousPulls:    anonymousPulls,
			DefaultMirrorRef:  defaultMirror,
			AllowedMirrorURLs: []string{"https://harbor.example/"},
			AllowedNamespaces: v1alpha1.AllowedNamespaces{Names: []string{resolveTenant}},
		},
	}
}

func credentialObject(name, upstream, secret string) v1alpha1.UpstreamCredential {
	return v1alpha1.UpstreamCredential{
		ObjectMeta: metav1.ObjectMeta{Namespace: resolveAdmin, Name: name, Generation: 1},
		Spec: v1alpha1.UpstreamCredentialSpec{
			UpstreamRef: v1alpha1.ObjectRef{Name: upstream},
			SecretRef:   v1alpha1.CredentialsSecretRef{Name: secret},
			Ceiling: []v1alpha1.AccessGrant{{
				Repositories: []string{"org/**"},
				Actions:      []v1alpha1.Action{v1alpha1.ActionPush, v1alpha1.ActionDelete},
			}},
			AllowedNamespaces: v1alpha1.AllowedNamespaces{Names: []string{resolveTenant}},
		},
	}
}

func mirrorObject(namespace, name, url string, secret *v1alpha1.CredentialsSecretRef) v1alpha1.RegistryMirror {
	return v1alpha1.RegistryMirror{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Generation: 1},
		Spec: v1alpha1.RegistryMirrorSpec{
			UpstreamRef: v1alpha1.ObjectRef{Namespace: resolveAdmin, Name: "ghcr"},
			URL:         url,
			PathPrefix:  name,
			SecretRef:   secret,
		},
	}
}

func credentialRule(name, credential string, repositories []string, actions ...v1alpha1.Action) v1alpha1.AccessRule {
	return v1alpha1.AccessRule{
		Name:          name,
		CredentialRef: &v1alpha1.ObjectRef{Namespace: resolveAdmin, Name: credential},
		Repositories:  repositories,
		Actions:       actions,
	}
}

func mirrorRule(name, mirror string, repositories ...string) v1alpha1.AccessRule {
	return v1alpha1.AccessRule{
		Name:         name,
		MirrorRef:    &v1alpha1.ObjectRef{Name: mirror},
		Repositories: repositories,
		Actions:      []v1alpha1.Action{v1alpha1.ActionPull},
	}
}

func policyObject(name string, rules ...v1alpha1.AccessRule) v1alpha1.RegistryAccessPolicy {
	return v1alpha1.RegistryAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: resolveTenant, Name: name, Generation: 1},
		Spec:       v1alpha1.RegistryAccessPolicySpec{ServiceAccounts: []string{resolveSA}, Rules: rules},
	}
}

// resolveInputs is a working setup: upstream ghcr (host ghcr.io, anonymous pulls challenged, default mirror
// default-mirror) and upstream other (host registry.example, anonymous pulls forwarded, no default mirror); ready
// credentials cred-1 to cred-6 and other-cred, a credential pending-cred waiting on its Secret and a credential
// broken-cred with no Secret; ready tenant mirrors harbor and harbor-2, a mirror outside the allowed URLs
// unready-mirror and a mirror pending-mirror waiting on its Secret. It has no policies.
func resolveInputs(t *testing.T) model.Inputs {
	t.Helper()

	pair, err := servingPair()
	require.NoError(t, err)

	certificate, privateKey, found := strings.Cut(pair, "\x00")
	require.True(t, found)

	store := &resolveSecrets{values: map[secrets.Key]secrets.Secret{}, pending: map[secrets.Key]bool{}}
	store.set(resolveAdmin, "serving", map[string]string{"tls.crt": certificate, "tls.key": privateKey})
	store.set(resolveAdmin, "creds", map[string]string{"username": "robot", "password": "secret"})
	store.pending[secrets.Key{Namespace: resolveAdmin, Name: "pending-creds"}] = true
	store.pending[secrets.Key{Namespace: resolveTenant, Name: "pending-mirror-creds"}] = true

	credentials := []v1alpha1.UpstreamCredential{
		credentialObject("other-cred", "other", "creds"),
		credentialObject("pending-cred", "ghcr", "pending-creds"),
		credentialObject("broken-cred", "ghcr", "missing-creds"),
	}
	for index := 1; index <= 6; index++ {
		credentials = append(credentials, credentialObject(fmt.Sprintf("cred-%d", index), "ghcr", "creds"))
	}

	defaultMirror := mirrorObject(resolveAdmin, "default-mirror", "https://harbor.example/default", nil)

	return model.Inputs{
		AdminNamespaces: []string{resolveAdmin},
		Namespaces:      map[string]labels.Set{resolveAdmin: {}, resolveTenant: {}},
		Secrets:         store,
		Upstreams: []v1alpha1.UpstreamRegistry{
			upstreamObject("ghcr", "ghcr.io", "", &v1alpha1.ObjectRef{Name: "default-mirror"}),
			upstreamObject("other", "registry.example", v1alpha1.AnonymousPullsForward, nil),
		},
		Credentials: credentials,
		Mirrors: []v1alpha1.RegistryMirror{
			defaultMirror,
			mirrorObject(resolveTenant, "harbor", "https://harbor.example/ci-a", nil),
			mirrorObject(resolveTenant, "harbor-2", "https://harbor.example/ci-a-2", nil),
			mirrorObject(resolveTenant, "unready-mirror", "https://elsewhere.example/ci-a", nil),
			mirrorObject(resolveTenant, "pending-mirror", "https://harbor.example/ci-a-3",
				&v1alpha1.CredentialsSecretRef{Name: "pending-mirror-creds"}),
		},
	}
}

func pullRequest(name string) registry.Request {
	return registry.Request{Method: "GET", Route: registry.RouteManifest, Action: v1alpha1.ActionPull, Name: name, Reference: "latest"}
}

func pushRequest(name string) registry.Request {
	return registry.Request{Method: "PUT", Route: registry.RouteManifest, Action: v1alpha1.ActionPush, Name: name, Reference: "latest"}
}

func deleteRequest(name string) registry.Request {
	return registry.Request{Method: "DELETE", Route: registry.RouteManifest, Action: v1alpha1.ActionDelete, Name: name, Reference: "latest"}
}

func mountRequest(name, from string) registry.Request {
	return registry.Request{
		Method: "POST", Route: registry.RouteUploadStart, Action: v1alpha1.ActionPush, Name: name,
		Mount: &registry.Mount{Digest: mountDigest, From: from},
	}
}

func objectName(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}

func TestDecideResolvesRequests(t *testing.T) {
	t.Parallel()

	builder := authn.Identity{Namespace: resolveTenant, ServiceAccount: resolveSA}
	anonymous := authn.Identity{}

	sixCredentialRules := make([]v1alpha1.AccessRule, 0, 6)
	for index := 1; index <= 6; index++ {
		sixCredentialRules = append(sixCredentialRules,
			credentialRule(fmt.Sprintf("rule-%d", index), fmt.Sprintf("cred-%d", index), []string{"org/app"}, v1alpha1.ActionPush))
	}

	tests := []struct {
		name     string
		policies []v1alpha1.RegistryAccessPolicy
		host     string
		identity authn.Identity
		request  registry.Request

		result       authz.Result
		credential   string
		mirror       types.NamespacedName
		mirrorFailed bool
		rules        []string
		message      string
		mountKept    bool
	}{
		{
			name: "each of two credential rules on disjoint repositories resolves its own repository (first)",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds",
				credentialRule("a", "cred-1", []string{"org/a"}, v1alpha1.ActionPush),
				credentialRule("b", "cred-2", []string{"org/b"}, v1alpha1.ActionPush))},
			identity: builder, request: pushRequest("org/a"),
			result: authz.ResultCredential, credential: "cred-1", rules: []string{"ci-a/builds/a"},
		},
		{
			name: "each of two credential rules on disjoint repositories resolves its own repository (second)",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds",
				credentialRule("a", "cred-1", []string{"org/a"}, v1alpha1.ActionPush),
				credentialRule("b", "cred-2", []string{"org/b"}, v1alpha1.ActionPush))},
			identity: builder, request: pushRequest("org/b"),
			result: authz.ResultCredential, credential: "cred-2", rules: []string{"ci-a/builds/b"},
		},
		{
			name: "two matching rules naming different credentials are ambiguous",
			policies: []v1alpha1.RegistryAccessPolicy{
				policyObject("builds", credentialRule("a", "cred-1", []string{"org/**"}, v1alpha1.ActionPush)),
				policyObject("deploys", credentialRule("b", "cred-2", []string{"org/app"}, v1alpha1.ActionPush)),
			},
			identity: builder, request: pushRequest("org/app"),
			result: authz.ResultAmbiguous, rules: []string{"ci-a/builds/a", "ci-a/deploys/b"},
			message: "ambiguous rules: ci-a/builds/a, ci-a/deploys/b",
		},
		{
			name: "two matching rules naming different mirrors are ambiguous",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds",
				mirrorRule("z", "harbor", "**"), mirrorRule("y", "harbor-2", "org/**"))},
			identity: builder, request: pullRequest("org/app"),
			result: authz.ResultAmbiguous, rules: []string{"ci-a/builds/y", "ci-a/builds/z"},
			message: "ambiguous rules: ci-a/builds/y, ci-a/builds/z",
		},
		{
			name:     "an ambiguity message lists exactly five rules without an ellipsis",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds", sixCredentialRules[:5]...)},
			identity: builder, request: pushRequest("org/app"),
			result:  authz.ResultAmbiguous,
			rules:   []string{"ci-a/builds/rule-1", "ci-a/builds/rule-2", "ci-a/builds/rule-3", "ci-a/builds/rule-4", "ci-a/builds/rule-5"},
			message: "ambiguous rules: ci-a/builds/rule-1, ci-a/builds/rule-2, ci-a/builds/rule-3, ci-a/builds/rule-4, ci-a/builds/rule-5",
		},
		{
			name:     "an ambiguity message lists at most five of six rules",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds", sixCredentialRules...)},
			identity: builder, request: pushRequest("org/app"),
			result: authz.ResultAmbiguous,
			rules: []string{"ci-a/builds/rule-1", "ci-a/builds/rule-2", "ci-a/builds/rule-3", "ci-a/builds/rule-4",
				"ci-a/builds/rule-5", "ci-a/builds/rule-6"},
			message: "ambiguous rules: ci-a/builds/rule-1, ci-a/builds/rule-2, ci-a/builds/rule-3, ci-a/builds/rule-4, ci-a/builds/rule-5, …",
		},
		{
			name: "rules naming the same credential combine their actions (push)",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds",
				credentialRule("pushes", "cred-1", []string{"org/app"}, v1alpha1.ActionPush),
				credentialRule("deletes", "cred-1", []string{"org/**"}, v1alpha1.ActionDelete))},
			identity: builder, request: pushRequest("org/app"),
			result: authz.ResultCredential, credential: "cred-1", rules: []string{"ci-a/builds/pushes"},
		},
		{
			name: "rules naming the same credential combine their actions (delete)",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds",
				credentialRule("pushes", "cred-1", []string{"org/app"}, v1alpha1.ActionPush),
				credentialRule("deletes", "cred-1", []string{"org/**"}, v1alpha1.ActionDelete))},
			identity: builder, request: deleteRequest("org/app"),
			result: authz.ResultCredential, credential: "cred-1", rules: []string{"ci-a/builds/deletes"},
		},
		{
			name: "rules naming the same credential both matching a pull combine",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds",
				credentialRule("pushes", "cred-1", []string{"org/app"}, v1alpha1.ActionPush),
				credentialRule("deletes", "cred-1", []string{"org/**"}, v1alpha1.ActionDelete))},
			identity: builder, request: pullRequest("org/app"),
			result: authz.ResultCredential, credential: "cred-1", rules: []string{"ci-a/builds/deletes", "ci-a/builds/pushes"},
		},
		{
			name: "rules naming the same mirror combine",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds",
				mirrorRule("all", "harbor", "**"), mirrorRule("org", "harbor", "org/**"))},
			identity: builder, request: pullRequest("org/app"),
			result: authz.ResultMirror, mirror: objectName(resolveTenant, "harbor"), rules: []string{"ci-a/builds/all", "ci-a/builds/org"},
		},
		{
			name: "a credential rule wins over a mirror rule for a pull",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds",
				mirrorRule("mirror", "harbor", "**"),
				credentialRule("private", "cred-1", []string{"org/app"}, v1alpha1.ActionPull))},
			identity: builder, request: pullRequest("org/app"),
			result: authz.ResultCredential, credential: "cred-1", rules: []string{"ci-a/builds/private"},
		},
		{
			name: "a credential rule wins over two different mirror rules for a pull",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds",
				mirrorRule("mirror", "harbor", "**"), mirrorRule("mirror-2", "harbor-2", "**"),
				credentialRule("private", "cred-1", []string{"org/app"}, v1alpha1.ActionPush))},
			identity: builder, request: pullRequest("org/app"),
			result: authz.ResultCredential, credential: "cred-1", rules: []string{"ci-a/builds/private"},
		},
		{
			name:     "a mirror rule serves a pull",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds", mirrorRule("mirror", "harbor", "**"))},
			identity: builder, request: pullRequest("library/alpine"),
			result: authz.ResultMirror, mirror: objectName(resolveTenant, "harbor"), rules: []string{"ci-a/builds/mirror"},
		},
		{
			name:     "a mirror rule whose mirror isn't ready resolves to that mirror as failed",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds", mirrorRule("mirror", "unready-mirror", "**"))},
			identity: builder, request: pullRequest("library/alpine"),
			result: authz.ResultMirror, mirror: objectName(resolveTenant, "unready-mirror"), mirrorFailed: true,
			rules: []string{"ci-a/builds/mirror"},
		},
		{
			name:     "a mirror rule waiting on a pending Secret is not ready",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds", mirrorRule("mirror", "pending-mirror", "**"))},
			identity: builder, request: pullRequest("library/alpine"),
			result: authz.ResultNotReady, rules: []string{"ci-a/builds/mirror"},
		},
		{
			name: "a credential rule waiting on a pending Secret is not ready",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds",
				credentialRule("push", "pending-cred", []string{"org/app"}, v1alpha1.ActionPush))},
			identity: builder, request: pushRequest("org/app"),
			result: authz.ResultNotReady, rules: []string{"ci-a/builds/push"},
		},
		{
			name: "a credential rule whose credential isn't ready is not a candidate",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds",
				credentialRule("push", "broken-cred", []string{"org/app"}, v1alpha1.ActionPush))},
			identity: builder, request: pushRequest("org/app"),
			result:  authz.ResultDenied,
			message: "ServiceAccount ci-a/builder may not push repository org/app: no RegistryAccessPolicy rule allows it",
		},
		{
			name: "a credential rule on another upstream is not a candidate",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds",
				credentialRule("push", "other-cred", []string{"org/app"}, v1alpha1.ActionPush))},
			identity: builder, request: pushRequest("org/app"),
			result:  authz.ResultDenied,
			message: "ServiceAccount ci-a/builder may not push repository org/app: no RegistryAccessPolicy rule allows it",
		},
		{
			name: "a rule for another ServiceAccount is not a candidate",
			policies: []v1alpha1.RegistryAccessPolicy{func() v1alpha1.RegistryAccessPolicy {
				policy := policyObject("builds", credentialRule("push", "cred-1", []string{"org/app"}, v1alpha1.ActionPush))
				policy.Spec.ServiceAccounts = []string{"deployer"}

				return policy
			}()},
			identity: builder, request: pushRequest("org/app"),
			result:  authz.ResultDenied,
			message: "ServiceAccount ci-a/builder may not push repository org/app: no RegistryAccessPolicy rule allows it",
		},
		{
			name: "a credential rule without the action is not a candidate",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds",
				credentialRule("push", "cred-1", []string{"org/app"}, v1alpha1.ActionPush))},
			identity: builder, request: deleteRequest("org/app"),
			result:  authz.ResultDenied,
			message: "ServiceAccount ci-a/builder may not delete repository org/app: no RegistryAccessPolicy rule allows it",
		},
		{
			name: "a push rule is a candidate for pulls of its repositories",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds",
				credentialRule("push", "cred-1", []string{"org/app"}, v1alpha1.ActionPush))},
			identity: builder, request: pullRequest("org/app"),
			result: authz.ResultCredential, credential: "cred-1", rules: []string{"ci-a/builds/push"},
		},
		{
			name:     "an authenticated pull with no candidate goes to the default mirror",
			identity: builder, request: pullRequest("library/alpine"),
			result: authz.ResultAnonymous, mirror: objectName(resolveAdmin, "default-mirror"),
		},
		{
			name:     "an authenticated pull with no candidate and no default mirror goes upstream anonymously",
			host:     "registry.example",
			identity: builder, request: pullRequest("library/alpine"),
			result: authz.ResultAnonymous,
		},
		{
			name:     "a push with no candidate by a ServiceAccount is denied",
			identity: builder, request: pushRequest("org/app"),
			result:  authz.ResultDenied,
			message: "ServiceAccount ci-a/builder may not push repository org/app: no RegistryAccessPolicy rule allows it",
		},
		{
			name:     "a delete with no candidate by a ServiceAccount is denied",
			identity: builder, request: deleteRequest("org/app"),
			result:  authz.ResultDenied,
			message: "ServiceAccount ci-a/builder may not delete repository org/app: no RegistryAccessPolicy rule allows it",
		},
		{
			name:     "an anonymous pull is challenged when anonymous pulls are challenged",
			identity: anonymous, request: pullRequest("library/alpine"),
			result: authz.ResultChallenge,
		},
		{
			name:     "an anonymous pull matching a rule is challenged when anonymous pulls are challenged",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds", mirrorRule("mirror", "harbor", "**"))},
			identity: anonymous, request: pullRequest("library/alpine"),
			result: authz.ResultChallenge,
		},
		{
			name:     "an anonymous pull is forwarded anonymously when anonymous pulls are forwarded",
			host:     "registry.example",
			identity: anonymous, request: pullRequest("library/alpine"),
			result: authz.ResultAnonymous,
		},
		{
			name:     "an anonymous push is challenged when anonymous pulls are forwarded",
			host:     "registry.example",
			identity: anonymous, request: pushRequest("org/app"),
			result: authz.ResultChallenge,
		},
		{
			name:     "an anonymous delete is challenged",
			host:     "registry.example",
			identity: anonymous, request: deleteRequest("org/app"),
			result: authz.ResultChallenge,
		},
		{
			name: "a mount from a repository of the same credential is kept",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds",
				credentialRule("push", "cred-1", []string{"org/**"}, v1alpha1.ActionPush))},
			identity: builder, request: mountRequest("org/app", "org/base"),
			result: authz.ResultCredential, credential: "cred-1", rules: []string{"ci-a/builds/push"}, mountKept: true,
		},
		{
			name: "a mount from a repository of a different credential is dropped",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds",
				credentialRule("a", "cred-1", []string{"org/app"}, v1alpha1.ActionPush),
				credentialRule("b", "cred-2", []string{"org/base"}, v1alpha1.ActionPull))},
			identity: builder, request: mountRequest("org/app", "org/base"),
			result: authz.ResultCredential, credential: "cred-1", rules: []string{"ci-a/builds/a"},
		},
		{
			name: "a mount from a repository served by a mirror is dropped",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds",
				credentialRule("a", "cred-1", []string{"org/app"}, v1alpha1.ActionPush),
				mirrorRule("mirror", "harbor", "**"))},
			identity: builder, request: mountRequest("org/app", "library/alpine"),
			result: authz.ResultCredential, credential: "cred-1", rules: []string{"ci-a/builds/a"},
		},
		{
			name: "a mount from a repository with no rule is dropped",
			policies: []v1alpha1.RegistryAccessPolicy{policyObject("builds",
				credentialRule("a", "cred-1", []string{"org/app"}, v1alpha1.ActionPush))},
			identity: builder, request: mountRequest("org/app", "org/base"),
			result: authz.ResultCredential, credential: "cred-1", rules: []string{"ci-a/builds/a"},
		},
		{
			name:     "the version check by an anonymous caller is challenged",
			identity: anonymous, request: registry.Request{Method: "GET", Route: registry.RouteBase},
			result: authz.ResultChallenge,
		},
		{
			name:     "the version check by an anonymous caller is challenged when anonymous pulls are forwarded",
			host:     "registry.example",
			identity: anonymous, request: registry.Request{Method: "GET", Route: registry.RouteBase},
			result: authz.ResultChallenge,
		},
		{
			name:     "the version check by a ServiceAccount is answered by the proxy",
			identity: builder, request: registry.Request{Method: "GET", Route: registry.RouteBase},
			result: authz.ResultVersionCheck,
		},
		{
			name:     "a host no upstream serves is not found",
			host:     "unknown.example",
			identity: builder, request: pullRequest("library/alpine"),
			result: authz.ResultNotFound,
		},
		{
			name:     "the host is matched without regard to case",
			host:     "GHCR.io",
			identity: builder, request: registry.Request{Method: "GET", Route: registry.RouteBase},
			result: authz.ResultVersionCheck,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			inputs := resolveInputs(t)
			inputs.Policies = test.policies
			snapshot := model.Build(inputs)

			host := test.host
			if host == "" {
				host = "ghcr.io"
			}

			resolution := authz.Decide(snapshot, host, test.identity, test.request)

			require.Equal(t, test.result, resolution.Result)
			assert.Equal(t, test.rules, resolution.Rules)
			assert.Equal(t, test.message, resolution.Message)
			assert.Equal(t, test.mirrorFailed, resolution.MirrorFailed)

			if test.credential == "" {
				assert.Nil(t, resolution.Credential)
			} else if assert.NotNil(t, resolution.Credential) {
				assert.Equal(t, objectName(resolveAdmin, test.credential), resolution.Credential.Name())
			}

			if test.mirror == (types.NamespacedName{}) {
				assert.Nil(t, resolution.Mirror)
			} else if assert.NotNil(t, resolution.Mirror) {
				assert.Equal(t, test.mirror, resolution.Mirror.Name())
			}

			if test.result != authz.ResultNotFound {
				expectedHost := test.host
				if expectedHost == "" {
					expectedHost = "ghcr.io"
				}

				require.NotNil(t, resolution.Upstream)
				assert.Contains(t, resolution.Upstream.Object.Spec.Hosts, strings.ToLower(expectedHost))
			}

			expected := test.request
			if test.request.Mount != nil && !test.mountKept {
				expected.Mount = nil
			}

			assert.Equal(t, expected, resolution.Request)
		})
	}
}

func TestDecideWithoutSnapshotIsNotReady(t *testing.T) {
	t.Parallel()

	resolution := authz.Decide(nil, "ghcr.io", authn.Identity{Namespace: resolveTenant, ServiceAccount: resolveSA}, pullRequest("org/app"))
	require.Equal(t, authz.ResultNotReady, resolution.Result)
}

func TestDecideOnHostOfPendingUpstreamIsNotReady(t *testing.T) {
	t.Parallel()

	inputs := resolveInputs(t)
	store, ok := inputs.Secrets.(*resolveSecrets)
	require.True(t, ok)

	store.pending[secrets.Key{Namespace: resolveAdmin, Name: "serving"}] = true

	resolution := authz.Decide(model.Build(inputs), "ghcr.io", authn.Identity{Namespace: resolveTenant, ServiceAccount: resolveSA}, pullRequest("org/app"))
	require.Equal(t, authz.ResultNotReady, resolution.Result)
}
