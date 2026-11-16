package model_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/model"
	"github.com/solidDoWant/registrygate/internal/secrets"
)

const (
	adminNamespace  = "registrygate"
	tenantNamespace = "ci-a"
	otherNamespace  = "ci-b"
	serviceAccount  = "builder"
)

// fakeSecrets is a Secret store double keyed by namespace/name.
type fakeSecrets struct {
	mutex      sync.Mutex
	values     map[secrets.Key]secrets.Secret
	referenced []secrets.Key
}

func (f *fakeSecrets) Get(key secrets.Key) (secrets.Secret, bool) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	secret, ok := f.values[key]
	if !ok {
		return secrets.Secret{State: secrets.NotFound}, true
	}

	return secret, true
}

func (f *fakeSecrets) SetReferenced(keys []secrets.Key) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.referenced = keys
}

func (f *fakeSecrets) set(namespace, name string, secret secrets.Secret) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.values[secrets.Key{Namespace: namespace, Name: name}] = secret
}

func present(data map[string]string) secrets.Secret {
	out := secrets.Data{}
	for key, value := range data {
		out[key] = []byte(value)
	}

	return secrets.Secret{State: secrets.Present, Data: out, ResourceVersion: "1"}
}

var certificateCache sync.Map

// keyPair returns a self-signed PEM certificate and key for the DNS names.
func keyPair(t *testing.T, dnsNames ...string) (string, string) {
	t.Helper()

	cacheKey := fmt.Sprint(dnsNames)
	if cached, ok := certificateCache.Load(cacheKey); ok {
		pair := cached.([2]string)
		return pair[0], pair[1]
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	certificate := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	privateKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	certificateCache.Store(cacheKey, [2]string{certificate, privateKey})

	return certificate, privateKey
}

// fixture is a working setup: an upstream for ghcr.io in the admin
// namespace, a shared push credential, a tenant mirror and a tenant policy
// with one credential rule and one mirror rule.
type fixture struct {
	inputs  model.Inputs
	secrets *fakeSecrets
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	certificate, privateKey := keyPair(t, "ghcr.io")
	store := &fakeSecrets{values: map[secrets.Key]secrets.Secret{}}
	store.set(adminNamespace, "ghcr-tls", present(map[string]string{"tls.crt": certificate, "tls.key": privateKey}))
	store.set(adminNamespace, "push-creds", present(map[string]string{"username": "robot", "password": "secret"}))

	return &fixture{
		secrets: store,
		inputs: model.Inputs{
			AdminNamespaces: []string{adminNamespace},
			Namespaces: map[string]labels.Set{
				adminNamespace:  {},
				tenantNamespace: {"team": "a"},
				otherNamespace:  {"team": "b"},
			},
			Secrets: store,
			Upstreams: []v1alpha1.UpstreamRegistry{{
				ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: "ghcr", Generation: 1},
				Spec: v1alpha1.UpstreamRegistrySpec{
					Hosts:             []string{"ghcr.io"},
					URL:               "https://ghcr.example",
					TLS:               v1alpha1.ServingTLS{SecretRef: v1alpha1.LocalRef{Name: "ghcr-tls"}},
					AllowedMirrorURLs: []string{"https://harbor.example/base"},
					AllowedNamespaces: v1alpha1.AllowedNamespaces{Names: []string{tenantNamespace}},
				},
			}},
			Credentials: []v1alpha1.UpstreamCredential{{
				ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: "push", Generation: 1},
				Spec: v1alpha1.UpstreamCredentialSpec{
					UpstreamRef: v1alpha1.ObjectRef{Name: "ghcr"},
					SecretRef:   v1alpha1.CredentialsSecretRef{Name: "push-creds"},
					Ceiling: []v1alpha1.AccessGrant{{
						Repositories: []string{"org/**"},
						Actions:      []v1alpha1.Action{v1alpha1.ActionPush},
					}},
					AllowedNamespaces: v1alpha1.AllowedNamespaces{Names: []string{tenantNamespace}},
				},
			}},
			Mirrors: []v1alpha1.RegistryMirror{{
				ObjectMeta: metav1.ObjectMeta{Namespace: tenantNamespace, Name: "harbor", Generation: 1},
				Spec: v1alpha1.RegistryMirrorSpec{
					UpstreamRef: v1alpha1.ObjectRef{Namespace: adminNamespace, Name: "ghcr"},
					URL:         "https://harbor.example/base/ci-a",
					PathPrefix:  "ci-a-ghcr",
				},
			}},
			Policies: []v1alpha1.RegistryAccessPolicy{{
				ObjectMeta: metav1.ObjectMeta{Namespace: tenantNamespace, Name: "builds", Generation: 1},
				Spec: v1alpha1.RegistryAccessPolicySpec{
					ServiceAccounts: []string{serviceAccount},
					Rules: []v1alpha1.AccessRule{
						{
							Name:          "push",
							CredentialRef: &v1alpha1.ObjectRef{Namespace: adminNamespace, Name: "push"},
							Repositories:  []string{"org/app"},
							Actions:       []v1alpha1.Action{v1alpha1.ActionPush},
						},
						{
							Name:         "pull",
							MirrorRef:    &v1alpha1.ObjectRef{Name: "harbor"},
							Repositories: []string{"**"},
							Actions:      []v1alpha1.Action{v1alpha1.ActionPull},
						},
					},
				},
			}},
		},
	}
}

func (f *fixture) upstream() *v1alpha1.UpstreamRegistry { return &f.inputs.Upstreams[0] }

func (f *fixture) credential() *v1alpha1.UpstreamCredential { return &f.inputs.Credentials[0] }

func (f *fixture) mirror() *v1alpha1.RegistryMirror { return &f.inputs.Mirrors[0] }

func (f *fixture) rule(name string) *v1alpha1.AccessRule {
	for index := range f.inputs.Policies[0].Spec.Rules {
		if f.inputs.Policies[0].Spec.Rules[index].Name == name {
			return &f.inputs.Policies[0].Spec.Rules[index]
		}
	}

	panic("no rule " + name)
}

func name(namespace, object string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: object}
}

func ruleStatus(t *testing.T, snapshot *model.Snapshot, rule string) model.Status {
	t.Helper()

	policy := snapshot.Policies[name(tenantNamespace, "builds")]
	require.NotNil(t, policy)

	for _, candidate := range policy.Rules {
		if candidate.Name == rule {
			return candidate.Status
		}
	}

	require.Failf(t, "rule not found", "%s", rule)

	return model.Status{}
}

func TestBuildReadiness(t *testing.T) {
	t.Parallel()

	upstreamName := name(adminNamespace, "ghcr")
	credentialName := name(adminNamespace, "push")
	mirrorName := name(tenantNamespace, "harbor")

	upstreamStatus := func(snapshot *model.Snapshot) model.Status { return snapshot.Upstreams[upstreamName].Status }
	credentialStatus := func(snapshot *model.Snapshot) model.Status {
		return snapshot.Credentials[credentialName].Status
	}
	mirrorStatus := func(snapshot *model.Snapshot) model.Status { return snapshot.Mirrors[mirrorName].Status }
	pushRule := func(snapshot *model.Snapshot) model.Status { return ruleStatusNoT(snapshot, "push") }
	pullRule := func(snapshot *model.Snapshot) model.Status { return ruleStatusNoT(snapshot, "pull") }

	tests := []struct {
		name   string
		mutate func(t *testing.T, f *fixture)
		status func(*model.Snapshot) model.Status
		reason string
	}{
		{name: "working upstream is ready", status: upstreamStatus, reason: v1alpha1.ReasonReady},
		{name: "working mirror is ready", status: mirrorStatus, reason: v1alpha1.ReasonReady},
		{name: "working credential is ready", status: credentialStatus, reason: v1alpha1.ReasonReady},
		{name: "rule inside the ceiling is accepted", status: pushRule, reason: v1alpha1.ReasonAccepted},
		{name: "mirror rule is accepted", status: pullRule, reason: v1alpha1.ReasonAccepted},

		// UpstreamRegistry.
		{
			name:   "upstream with an http url is an invalid spec",
			mutate: func(_ *testing.T, f *fixture) { f.upstream().Spec.URL = "http://ghcr.example" },
			status: upstreamStatus, reason: v1alpha1.ReasonInvalidSpec,
		},
		{
			name:   "upstream url with a path is an invalid spec",
			mutate: func(_ *testing.T, f *fixture) { f.upstream().Spec.URL = "https://ghcr.example/v2" },
			status: upstreamStatus, reason: v1alpha1.ReasonInvalidSpec,
		},
		{
			name: "upstream with a bad selector is an invalid spec",
			mutate: func(_ *testing.T, f *fixture) {
				f.upstream().Spec.AllowedNamespaces.Selector = &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "team", Operator: "Bogus"}},
				}
			},
			status: upstreamStatus, reason: v1alpha1.ReasonInvalidSpec,
		},
		{
			name:   "upstream whose serving Secret is missing has it unavailable",
			mutate: func(_ *testing.T, f *fixture) { f.upstream().Spec.TLS.SecretRef.Name = "missing" },
			status: upstreamStatus, reason: v1alpha1.ReasonSecretUnavailable,
		},
		{
			name: "upstream whose serving Secret is forbidden has it unavailable",
			mutate: func(_ *testing.T, f *fixture) {
				f.secrets.set(adminNamespace, "ghcr-tls", secrets.Secret{State: secrets.Forbidden})
			},
			status: upstreamStatus, reason: v1alpha1.ReasonSecretUnavailable,
		},
		{
			name: "upstream whose serving Secret has no key pair has it invalid",
			mutate: func(_ *testing.T, f *fixture) {
				f.secrets.set(adminNamespace, "ghcr-tls", present(map[string]string{"tls.crt": "x", "tls.key": "y"}))
			},
			status: upstreamStatus, reason: v1alpha1.ReasonSecretInvalid,
		},
		{
			name: "upstream whose certificate doesn't cover a host is a mismatch",
			mutate: func(_ *testing.T, f *fixture) {
				f.upstream().Spec.Hosts = []string{"ghcr.io", "quay.io"}
			},
			status: upstreamStatus, reason: v1alpha1.ReasonCertificateMismatch,
		},
		{
			name: "upstream whose CA Secret doesn't parse has it invalid",
			mutate: func(_ *testing.T, f *fixture) {
				f.upstream().Spec.CASecretRef = &v1alpha1.LocalRef{Name: "ca"}
				f.secrets.set(adminNamespace, "ca", present(map[string]string{"ca.crt": "not a certificate"}))
			},
			status: upstreamStatus, reason: v1alpha1.ReasonSecretInvalid,
		},
		{
			name: "upstream with a valid CA Secret is ready",
			mutate: func(t *testing.T, f *fixture) {
				certificate, _ := keyPair(t, "ca.example")
				f.upstream().Spec.CASecretRef = &v1alpha1.LocalRef{Name: "ca"}
				f.secrets.set(adminNamespace, "ca", present(map[string]string{"ca.crt": certificate}))
			},
			status: upstreamStatus, reason: v1alpha1.ReasonReady,
		},
		{
			name: "upstream whose host another valid upstream claims conflicts",
			mutate: func(_ *testing.T, f *fixture) {
				other := f.upstream().DeepCopy()
				other.Name = "other"
				f.inputs.Upstreams = append(f.inputs.Upstreams, *other)
			},
			status: upstreamStatus, reason: v1alpha1.ReasonHostConflict,
		},
		{
			name: "upstream sharing a host with an invalid upstream is ready",
			mutate: func(_ *testing.T, f *fixture) {
				other := f.upstream().DeepCopy()
				other.Name = "other"
				other.Spec.URL = "ftp://ghcr.example"
				f.inputs.Upstreams = append(f.inputs.Upstreams, *other)
			},
			status: upstreamStatus, reason: v1alpha1.ReasonReady,
		},

		// RegistryMirror.
		{
			name:   "mirror whose upstream doesn't exist has its ref not found",
			mutate: func(_ *testing.T, f *fixture) { f.mirror().Spec.UpstreamRef.Name = "missing" },
			status: mirrorStatus, reason: v1alpha1.ReasonRefNotFound,
		},
		{
			name:   "mirror whose upstream doesn't admit its namespace is not allowed",
			mutate: func(_ *testing.T, f *fixture) { f.upstream().Spec.AllowedNamespaces.Names = nil },
			status: mirrorStatus, reason: v1alpha1.ReasonNotAllowed,
		},
		{
			name: "mirror admitted by its upstream's selector is ready",
			mutate: func(_ *testing.T, f *fixture) {
				f.upstream().Spec.AllowedNamespaces = v1alpha1.AllowedNamespaces{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}},
				}
			},
			status: mirrorStatus, reason: v1alpha1.ReasonReady,
		},
		{
			name: "mirror whose namespace isn't in the cache is not allowed by an empty selector",
			mutate: func(_ *testing.T, f *fixture) {
				f.upstream().Spec.AllowedNamespaces = v1alpha1.AllowedNamespaces{Selector: &metav1.LabelSelector{}}
				delete(f.inputs.Namespaces, tenantNamespace)
			},
			status: mirrorStatus, reason: v1alpha1.ReasonNotAllowed,
		},
		{
			name: "mirror admitted by an empty selector is ready",
			mutate: func(_ *testing.T, f *fixture) {
				f.upstream().Spec.AllowedNamespaces = v1alpha1.AllowedNamespaces{Selector: &metav1.LabelSelector{}}
			},
			status: mirrorStatus, reason: v1alpha1.ReasonReady,
		},
		{
			name:   "mirror whose upstream isn't ready has its ref not ready",
			mutate: func(_ *testing.T, f *fixture) { f.upstream().Spec.TLS.SecretRef.Name = "missing" },
			status: mirrorStatus, reason: v1alpha1.ReasonRefNotReady,
		},
		{
			name:   "mirror on another host than the allowed prefix is not allowed",
			mutate: func(_ *testing.T, f *fixture) { f.mirror().Spec.URL = "https://evil.example/base/ci-a" },
			status: mirrorStatus, reason: v1alpha1.ReasonURLNotAllowed,
		},
		{
			name:   "mirror whose path continues the prefix without a slash is not allowed",
			mutate: func(_ *testing.T, f *fixture) { f.mirror().Spec.URL = "https://harbor.example/basement" },
			status: mirrorStatus, reason: v1alpha1.ReasonURLNotAllowed,
		},
		{
			name: "mirror with another scheme than the allowed prefix is not allowed",
			mutate: func(_ *testing.T, f *fixture) {
				f.inputs.AllowHTTP = true
				f.mirror().Spec.URL = "http://harbor.example/base"
			},
			status: mirrorStatus, reason: v1alpha1.ReasonURLNotAllowed,
		},
		{
			name:   "mirror at exactly the allowed prefix is ready",
			mutate: func(_ *testing.T, f *fixture) { f.mirror().Spec.URL = "https://harbor.example/base/" },
			status: mirrorStatus, reason: v1alpha1.ReasonReady,
		},
		{
			name:   "mirror url with a dot-dot segment is an invalid spec",
			mutate: func(_ *testing.T, f *fixture) { f.mirror().Spec.URL = "https://harbor.example/base/../x" },
			status: mirrorStatus, reason: v1alpha1.ReasonInvalidSpec,
		},
		{
			name:   "mirror url with percent-encoding is an invalid spec",
			mutate: func(_ *testing.T, f *fixture) { f.mirror().Spec.URL = "https://harbor.example/base/%2e" },
			status: mirrorStatus, reason: v1alpha1.ReasonInvalidSpec,
		},
		{
			name: "mirror whose credentials Secret lacks the password has it invalid",
			mutate: func(_ *testing.T, f *fixture) {
				f.mirror().Spec.SecretRef = &v1alpha1.CredentialsSecretRef{Name: "robot"}
				f.secrets.set(tenantNamespace, "robot", present(map[string]string{"username": "robot"}))
			},
			status: mirrorStatus, reason: v1alpha1.ReasonSecretInvalid,
		},
		{
			name: "mirror whose credentials Secret is missing has it unavailable",
			mutate: func(_ *testing.T, f *fixture) {
				f.mirror().Spec.SecretRef = &v1alpha1.CredentialsSecretRef{Name: "robot"}
			},
			status: mirrorStatus, reason: v1alpha1.ReasonSecretUnavailable,
		},

		// UpstreamCredential.
		{
			name: "credential whose Secret is missing has it unavailable",
			mutate: func(_ *testing.T, f *fixture) {
				f.credential().Spec.SecretRef.Name = "missing"
			},
			status: credentialStatus, reason: v1alpha1.ReasonSecretUnavailable,
		},
		{
			name: "credential whose Secret has an empty password has it invalid",
			mutate: func(_ *testing.T, f *fixture) {
				f.secrets.set(adminNamespace, "push-creds", present(map[string]string{"username": "robot", "password": ""}))
			},
			status: credentialStatus, reason: v1alpha1.ReasonSecretInvalid,
		},
		{
			name: "credential whose Secret lacks the username has it invalid",
			mutate: func(_ *testing.T, f *fixture) {
				f.secrets.set(adminNamespace, "push-creds", present(map[string]string{"password": "secret"}))
			},
			status: credentialStatus, reason: v1alpha1.ReasonSecretInvalid,
		},
		{
			name: "credential with other key names reads those keys",
			mutate: func(_ *testing.T, f *fixture) {
				f.credential().Spec.SecretRef.UsernameKey = "user"
				f.credential().Spec.SecretRef.PasswordKey = "token"
				f.secrets.set(adminNamespace, "push-creds", present(map[string]string{"user": "robot", "token": "secret"}))
			},
			status: credentialStatus, reason: v1alpha1.ReasonReady,
		},
		{
			name: "credential with other key names doesn't fall back to the default keys",
			mutate: func(_ *testing.T, f *fixture) {
				f.credential().Spec.SecretRef.PasswordKey = "token"
			},
			status: credentialStatus, reason: v1alpha1.ReasonSecretInvalid,
		},
		{
			name:   "credential whose upstream doesn't exist has its ref not found",
			mutate: func(_ *testing.T, f *fixture) { f.credential().Spec.UpstreamRef.Name = "missing" },
			status: credentialStatus, reason: v1alpha1.ReasonRefNotFound,
		},
		{
			name: "credential in a namespace its upstream doesn't admit is not allowed",
			mutate: func(_ *testing.T, f *fixture) {
				f.credential().Namespace = otherNamespace
				f.credential().Spec.UpstreamRef.Namespace = adminNamespace
				f.secrets.set(otherNamespace, "push-creds", present(map[string]string{"username": "robot", "password": "secret"}))
			},
			status: func(snapshot *model.Snapshot) model.Status {
				return snapshot.Credentials[name(otherNamespace, "push")].Status
			},
			reason: v1alpha1.ReasonNotAllowed,
		},
		{
			name: "credential whose upstream isn't ready has its ref not ready",
			mutate: func(_ *testing.T, f *fixture) {
				f.secrets.set(adminNamespace, "ghcr-tls", secrets.Secret{State: secrets.NotFound})
			},
			status: credentialStatus, reason: v1alpha1.ReasonRefNotReady,
		},
		{
			name: "credential with an invalid ceiling glob is an invalid spec",
			mutate: func(_ *testing.T, f *fixture) {
				f.credential().Spec.Ceiling[0].Repositories = []string{"Org/**"}
			},
			status: credentialStatus, reason: v1alpha1.ReasonInvalidSpec,
		},

		// RegistryAccessPolicy rules.
		{
			name: "rule whose credential doesn't exist has its ref not found",
			mutate: func(_ *testing.T, f *fixture) {
				f.rule("push").CredentialRef.Name = "missing"
			},
			status: pushRule, reason: v1alpha1.ReasonRefNotFound,
		},
		{
			name: "rule whose mirror doesn't exist has its ref not found",
			mutate: func(_ *testing.T, f *fixture) {
				f.rule("pull").MirrorRef.Name = "missing"
			},
			status: pullRule, reason: v1alpha1.ReasonRefNotFound,
		},
		{
			name: "rule whose credential doesn't admit the policy's namespace is not allowed",
			mutate: func(_ *testing.T, f *fixture) {
				f.credential().Spec.AllowedNamespaces.Names = []string{otherNamespace}
			},
			status: pushRule, reason: v1alpha1.ReasonNotAllowed,
		},
		{
			name: "rule whose mirror doesn't admit the policy's namespace is not allowed",
			mutate: func(_ *testing.T, f *fixture) {
				f.inputs.Policies[0].Namespace = otherNamespace
				f.rule("pull").MirrorRef.Namespace = tenantNamespace
				f.upstream().Spec.AllowedNamespaces.Names = []string{tenantNamespace, otherNamespace}
			},
			status: func(snapshot *model.Snapshot) model.Status {
				return ruleOf(snapshot, name(otherNamespace, "builds"), "pull").Status
			},
			reason: v1alpha1.ReasonNotAllowed,
		},
		{
			name: "rule whose upstream doesn't admit the policy's namespace is not allowed",
			mutate: func(_ *testing.T, f *fixture) {
				// The credential lives in the admin namespace, so the
				// upstream admits it; it doesn't admit the policy's.
				f.credential().Spec.AllowedNamespaces.Names = []string{tenantNamespace, otherNamespace}
				f.inputs.Policies[0].Namespace = otherNamespace
				f.upstream().Spec.AllowedNamespaces.Names = []string{tenantNamespace}
			},
			status: func(snapshot *model.Snapshot) model.Status {
				return ruleOf(snapshot, name(otherNamespace, "builds"), "push").Status
			},
			reason: v1alpha1.ReasonNotAllowed,
		},
		{
			name: "rule whose credential isn't ready has its ref not ready",
			mutate: func(_ *testing.T, f *fixture) {
				f.credential().Spec.SecretRef.Name = "missing"
			},
			status: pushRule, reason: v1alpha1.ReasonRefNotReady,
		},
		{
			name: "rule whose mirror isn't ready has its ref not ready",
			mutate: func(_ *testing.T, f *fixture) {
				f.mirror().Spec.URL = "https://evil.example/base"
			},
			status: pullRule, reason: v1alpha1.ReasonRefNotReady,
		},
		{
			name:   "rule on repositories outside the ceiling is outside it",
			mutate: func(_ *testing.T, f *fixture) { f.rule("push").Repositories = []string{"other/app"} },
			status: pushRule, reason: v1alpha1.ReasonOutsideCeiling,
		},
		{
			name:   "rule wider than the ceiling is outside it",
			mutate: func(_ *testing.T, f *fixture) { f.rule("push").Repositories = []string{"**"} },
			status: pushRule, reason: v1alpha1.ReasonOutsideCeiling,
		},
		{
			name:   "delete rule under a push ceiling is outside it",
			mutate: func(_ *testing.T, f *fixture) { f.rule("push").Actions = []v1alpha1.Action{v1alpha1.ActionDelete} },
			status: pushRule, reason: v1alpha1.ReasonOutsideCeiling,
		},
		{
			name: "push rule under a pull ceiling is outside it",
			mutate: func(_ *testing.T, f *fixture) {
				f.credential().Spec.Ceiling[0].Actions = []v1alpha1.Action{v1alpha1.ActionPull}
			},
			status: pushRule, reason: v1alpha1.ReasonOutsideCeiling,
		},
		{
			name: "push rule needs pull in the ceiling for the same repositories",
			mutate: func(_ *testing.T, f *fixture) {
				f.credential().Spec.Ceiling = []v1alpha1.AccessGrant{
					{Repositories: []string{"org/**"}, Actions: []v1alpha1.Action{v1alpha1.ActionPush}},
				}
				f.rule("push").Actions = []v1alpha1.Action{v1alpha1.ActionPull, v1alpha1.ActionPush}
			},
			status: pushRule, reason: v1alpha1.ReasonAccepted,
		},
		{
			name: "pull rule under a push ceiling is inside it",
			mutate: func(_ *testing.T, f *fixture) {
				f.rule("push").Actions = []v1alpha1.Action{v1alpha1.ActionPull}
			},
			status: pushRule, reason: v1alpha1.ReasonAccepted,
		},
		{
			name: "rule with an invalid glob is an invalid spec",
			mutate: func(_ *testing.T, f *fixture) {
				f.rule("push").Repositories = []string{"org//app"}
			},
			status: pushRule, reason: v1alpha1.ReasonInvalidSpec,
		},
		{
			name: "policy with a rule not accepted lists it",
			mutate: func(_ *testing.T, f *fixture) {
				f.rule("push").Repositories = []string{"other/app"}
			},
			status: func(snapshot *model.Snapshot) model.Status {
				return snapshot.Policies[name(tenantNamespace, "builds")].Status
			},
			reason: v1alpha1.ReasonRulesNotAccepted,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			if test.mutate != nil {
				test.mutate(t, f)
			}

			status := test.status(model.Build(f.inputs))
			assert.False(t, status.Pending)
			assert.Equal(t, test.reason, status.Reason, status.Message)
			assert.Equal(t, test.reason == v1alpha1.ReasonReady || test.reason == v1alpha1.ReasonAccepted, status.Ready)
			assert.Equal(t, int64(1), status.ObservedGeneration)
		})
	}
}

func ruleOf(snapshot *model.Snapshot, policy types.NamespacedName, rule string) *model.Rule {
	for _, candidate := range snapshot.Policies[policy].Rules {
		if candidate.Name == rule {
			return candidate
		}
	}

	return nil
}

func ruleStatusNoT(snapshot *model.Snapshot, rule string) model.Status {
	return ruleOf(snapshot, name(tenantNamespace, "builds"), rule).Status
}

func TestBuildHostConflictMarksBothAndServesNeither(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	other := f.upstream().DeepCopy()
	other.Name = "other"
	f.inputs.Upstreams = append(f.inputs.Upstreams, *other)

	snapshot := model.Build(f.inputs)

	for _, object := range []string{"ghcr", "other"} {
		status := snapshot.Upstreams[name(adminNamespace, object)].Status
		assert.Equal(t, v1alpha1.ReasonHostConflict, status.Reason, object)
	}

	assert.Contains(t, snapshot.Upstreams[name(adminNamespace, "ghcr")].Status.Message, adminNamespace+"/other")
	assert.Nil(t, snapshot.Host("ghcr.io"))
}

func TestBuildServesReadyUpstreamHostsWithTheirCertificate(t *testing.T) {
	t.Parallel()

	snapshot := model.Build(newFixture(t).inputs)

	upstream := snapshot.Host("ghcr.io")
	require.NotNil(t, upstream)
	require.NotNil(t, upstream.Certificate)
	require.NoError(t, upstream.Certificate.Leaf.VerifyHostname("ghcr.io"))
	assert.Nil(t, snapshot.Host("quay.io"))
}

func TestBuildIgnoresUpstreamsOutsideAdminNamespaces(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.upstream().Namespace = tenantNamespace
	f.secrets.set(tenantNamespace, "ghcr-tls", f.secrets.values[secrets.Key{Namespace: adminNamespace, Name: "ghcr-tls"}])
	f.mirror().Spec.UpstreamRef.Namespace = tenantNamespace

	snapshot := model.Build(f.inputs)

	assert.Empty(t, snapshot.Upstreams)
	assert.Nil(t, snapshot.Host("ghcr.io"))
	assert.Equal(t, v1alpha1.ReasonRefNotFound, snapshot.Mirrors[name(tenantNamespace, "harbor")].Status.Reason)
	assert.NotContains(t, snapshot.Referenced(), secrets.Key{Namespace: tenantNamespace, Name: "ghcr-tls"})
}

func TestBuildReadsCredentialsFromTheSecret(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		usernameKey string
		passwordKey string
		data        map[string]string
	}{
		{name: "default keys", data: map[string]string{"username": "robot$ci", "password": "secret"}},
		{
			name: "other key names", usernameKey: "user", passwordKey: "token",
			data: map[string]string{"user": "robot$ci", "token": "secret", "username": "wrong", "password": "wrong"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			f.credential().Spec.SecretRef.UsernameKey = test.usernameKey
			f.credential().Spec.SecretRef.PasswordKey = test.passwordKey
			f.secrets.set(adminNamespace, "push-creds", present(test.data))

			credential := model.Build(f.inputs).Credentials[name(adminNamespace, "push")]
			require.True(t, credential.Status.Ready, credential.Status.Message)
			assert.Equal(t, model.Credentials{Username: "robot$ci", Password: "secret"}, credential.Credentials)
		})
	}
}

func TestCredentialsAreRedactedWhenFormatted(t *testing.T) {
	t.Parallel()

	credentials := model.Credentials{Username: "robot", Password: "hunter2"}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		assert.NotContains(t, fmt.Sprintf(format, credentials), "hunter2", format)
	}
}

func TestBuildIndexesRulesByServiceAccount(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.mirror().Spec.URL = "https://evil.example/base"   // mirror not ready
	f.rule("push").Repositories = []string{"other/app"} // outside the ceiling

	snapshot := model.Build(f.inputs)

	rules := snapshot.Rules(model.ServiceAccount{Namespace: tenantNamespace, Name: serviceAccount})
	require.Len(t, rules, 1, "the rule outside the ceiling isn't a candidate")
	assert.Equal(t, "pull", rules[0].Name)
	assert.Equal(t, v1alpha1.ReasonRefNotReady, rules[0].Status.Reason, "a mirror rule whose mirror isn't ready stays, marked not ready")
	assert.Same(t, snapshot.Mirrors[name(tenantNamespace, "harbor")], rules[0].Mirror)

	assert.Empty(t, snapshot.Rules(model.ServiceAccount{Namespace: otherNamespace, Name: serviceAccount}))
	assert.Empty(t, snapshot.Rules(model.ServiceAccount{Namespace: tenantNamespace, Name: "other"}))
}

func TestBuildRulesImplyPullForPushAndDelete(t *testing.T) {
	t.Parallel()

	snapshot := model.Build(newFixture(t).inputs)

	rule := ruleOf(snapshot, name(tenantNamespace, "builds"), "push")
	require.True(t, rule.Status.Ready, rule.Status.Message)
	assert.True(t, rule.Allows("org/app", v1alpha1.ActionPull))
	assert.True(t, rule.Allows("org/app", v1alpha1.ActionPush))
	assert.False(t, rule.Allows("org/app", v1alpha1.ActionDelete))
	assert.False(t, rule.Allows("org/other", v1alpha1.ActionPull))
	assert.Same(t, snapshot.Credentials[name(adminNamespace, "push")], rule.Credential)
	assert.Same(t, snapshot.Upstreams[name(adminNamespace, "ghcr")], rule.Upstream)
}

func TestBuildStopsOfferingWithdrawnReferences(t *testing.T) {
	t.Parallel()

	tenant := model.ServiceAccount{Namespace: tenantNamespace, Name: serviceAccount}
	other := model.ServiceAccount{Namespace: otherNamespace, Name: serviceAccount}

	// policyInOtherNamespace moves the policy to the other namespace, with
	// the upstream and the tenant's mirror admitting it.
	policyInOtherNamespace := func(f *fixture) {
		f.inputs.Policies[0].Namespace = otherNamespace
		f.rule("pull").MirrorRef.Namespace = tenantNamespace
		f.upstream().Spec.AllowedNamespaces.Names = []string{tenantNamespace, otherNamespace}
		f.mirror().Spec.AllowedNamespaces.Names = []string{otherNamespace}
	}

	tests := []struct {
		name     string
		setup    func(f *fixture)
		withdraw func(f *fixture)
		account  model.ServiceAccount
		rule     string
	}{
		{
			name:     "credential admission withdrawn",
			withdraw: func(f *fixture) { f.credential().Spec.AllowedNamespaces.Names = nil },
			account:  tenant,
			rule:     "push",
		},
		{
			name:     "upstream admission withdrawn",
			withdraw: func(f *fixture) { f.upstream().Spec.AllowedNamespaces.Names = nil },
			account:  tenant,
			rule:     "push",
		},
		{
			name: "credential Secret removed",
			withdraw: func(f *fixture) {
				f.secrets.set(adminNamespace, "push-creds", secrets.Secret{State: secrets.NotFound})
			},
			account: tenant,
			rule:    "push",
		},
		{
			name:     "mirror admission withdrawn",
			setup:    policyInOtherNamespace,
			withdraw: func(f *fixture) { f.mirror().Spec.AllowedNamespaces.Names = nil },
			account:  other,
			rule:     "pull",
		},
		{
			name:     "upstream admission of the mirror's namespace withdrawn",
			withdraw: func(f *fixture) { f.upstream().Spec.AllowedNamespaces.Names = nil },
			account:  tenant,
			rule:     "pull",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			if test.setup != nil {
				test.setup(f)
			}

			before := model.Build(f.inputs)
			require.Contains(t, ruleNames(before.Rules(test.account)), test.rule)

			test.withdraw(f)

			after := model.Build(f.inputs)
			assert.NotContains(t, ruleNames(after.Rules(test.account)), test.rule)
			// The earlier snapshot is immutable: it still offers the rule.
			assert.Contains(t, ruleNames(before.Rules(test.account)), test.rule)
		})
	}
}

func ruleNames(rules []*model.Rule) []string {
	out := make([]string, 0, len(rules))
	for _, rule := range rules {
		out = append(out, rule.Name)
	}

	return out
}

func TestBuildDefaultMirror(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(f *fixture)
		usable  bool
		message string
	}{
		{name: "ready mirror of the upstream admitting its namespace", mutate: func(*fixture) {}, usable: true},
		{
			name: "mirror that doesn't admit the upstream's namespace",
			mutate: func(f *fixture) {
				f.upstream().Spec.DefaultMirrorRef.Namespace = otherNamespace
				f.moveMirror(otherNamespace, nil)
			},
			message: "does not allow",
		},
		{
			name:    "mirror that isn't ready",
			mutate:  func(f *fixture) { f.mirror().Spec.URL = "https://evil.example/base" },
			message: "is not ready",
		},
		{
			name:    "missing mirror",
			mutate:  func(f *fixture) { f.upstream().Spec.DefaultMirrorRef.Name = "missing" },
			message: "not found",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			f.upstream().Spec.DefaultMirrorRef = &v1alpha1.ObjectRef{Namespace: tenantNamespace, Name: "harbor"}
			f.mirror().Spec.AllowedNamespaces.Names = []string{adminNamespace}
			test.mutate(f)

			upstream := model.Build(f.inputs).Upstreams[name(adminNamespace, "ghcr")]
			require.True(t, upstream.Status.Ready, "the upstream stays ready")

			if test.usable {
				assert.NotNil(t, upstream.DefaultMirror)
				return
			}

			assert.Nil(t, upstream.DefaultMirror)
			assert.Contains(t, upstream.Status.Message, test.message)
		})
	}
}

// moveMirror puts the mirror in namespace with the given admission.
func (f *fixture) moveMirror(namespace string, names []string) {
	f.mirror().Namespace = namespace
	f.mirror().Spec.AllowedNamespaces.Names = names
	f.upstream().Spec.AllowedNamespaces.Names = append(f.upstream().Spec.AllowedNamespaces.Names, namespace)
}

func TestBuildPendingSecrets(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.secrets.set(adminNamespace, "push-creds", secrets.Secret{State: secrets.Pending})

	snapshot := model.Build(f.inputs)

	assert.True(t, snapshot.PendingSecrets())
	assert.True(t, snapshot.Credentials[name(adminNamespace, "push")].Status.Pending)
	assert.True(t, ruleStatus(t, snapshot, "push").Pending, "a rule using a pending credential is pending")
	assert.True(t, snapshot.Policies[name(tenantNamespace, "builds")].Status.Pending)
	assert.True(t, snapshot.Upstreams[name(adminNamespace, "ghcr")].Status.Ready, "objects that don't use the Secret keep their readiness")
	assert.Contains(t, snapshot.Referenced(), secrets.Key{Namespace: adminNamespace, Name: "push-creds"})

	f.secrets.set(adminNamespace, "push-creds", present(map[string]string{"username": "robot", "password": "secret"}))
	assert.False(t, model.Build(f.inputs).PendingSecrets())
}
