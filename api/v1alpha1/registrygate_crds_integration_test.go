//go:build integration

package v1alpha1_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
)

func startCRDEnv(t *testing.T) client.Client {
	t.Helper()

	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set (run from the dev shell)")
	}

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.Stop() })

	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	require.NoError(t, cl.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ci"}}))

	return cl
}

func validRule(name string) v1alpha1.AccessRule {
	return v1alpha1.AccessRule{
		Name:          name,
		CredentialRef: &v1alpha1.ObjectRef{Namespace: "registrygate", Name: "shared"},
		Repositories:  []string{"acme/**"},
		Actions:       []v1alpha1.Action{v1alpha1.ActionPull, v1alpha1.ActionPush},
	}
}

func policy(name string, rules ...v1alpha1.AccessRule) *v1alpha1.RegistryAccessPolicy {
	return &v1alpha1.RegistryAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ci", Name: name},
		Spec:       v1alpha1.RegistryAccessPolicySpec{ServiceAccounts: []string{"builder"}, Rules: rules},
	}
}

func upstream(name string, hosts ...string) *v1alpha1.UpstreamRegistry {
	return &v1alpha1.UpstreamRegistry{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec: v1alpha1.UpstreamRegistrySpec{
			Hosts: hosts,
			URL:   "https://ghcr.io",
			TLS:   v1alpha1.ServingTLS{SecretRef: v1alpha1.LocalRef{Name: "tls"}},
		},
	}
}

func TestRegistrygateCRDsAdmissionRules(t *testing.T) {
	cl := startCRDEnv(t)

	mirrorRule := func(actions ...v1alpha1.Action) v1alpha1.AccessRule {
		return v1alpha1.AccessRule{
			Name: "mirror", MirrorRef: &v1alpha1.ObjectRef{Name: "m"},
			Repositories: []string{"library/**"}, Actions: actions,
		}
	}
	both := validRule("both")
	both.MirrorRef = &v1alpha1.ObjectRef{Name: "m"}
	both.Actions = []v1alpha1.Action{v1alpha1.ActionPull}
	neither := validRule("neither")
	neither.CredentialRef = nil
	dup := []v1alpha1.AccessRule{validRule("same"), validRule("same")}

	tests := map[string]struct {
		object client.Object
		valid  bool
	}{
		"credential rule":                  {policy("ok-credential", validRule("build")), true},
		"mirror rule with pull only":       {policy("ok-mirror", mirrorRule(v1alpha1.ActionPull)), true},
		"rule with both refs":              {policy("both", both), false},
		"rule with neither ref":            {policy("neither", neither), false},
		"mirror rule with push":            {policy("mirror-push", mirrorRule(v1alpha1.ActionPull, v1alpha1.ActionPush)), false},
		"mirror rule with delete only":     {policy("mirror-delete", mirrorRule(v1alpha1.ActionDelete)), false},
		"two rules with the same name":     {policy("dup", dup...), false},
		"upstream with lower-case host":    {upstream("ok", "ghcr.example.com"), true},
		"upstream with upper-case host":    {upstream("upper", "Ghcr.example.com"), false},
		"upstream with wildcard host":      {upstream("wild", "*.example.com"), false},
		"upstream with a port in the host": {upstream("port", "ghcr.example.com:8443"), false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if ns := tc.object.GetNamespace(); ns == "default" {
				tc.object.SetName(strings.ReplaceAll(name, " ", "-"))
			}

			err := cl.Create(t.Context(), tc.object)
			if tc.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestRegistrygateCRDsAcceptFullObjects(t *testing.T) {
	cl := startCRDEnv(t)
	ctx := t.Context()

	up := upstream("ghcr", "ghcr.example.com")
	up.Spec.AnonymousPulls = v1alpha1.AnonymousPullsForward
	up.Spec.AllowedMirrorURLs = []string{"https://mirror.example.com/proxy"}
	up.Spec.TokenRealmHosts = []string{"auth.example.com:443"}
	up.Spec.Timeouts = v1alpha1.Timeouts{ResponseHeader: &metav1.Duration{Duration: 30e9}}
	up.Spec.AllowedNamespaces = v1alpha1.AllowedNamespaces{Names: []string{"ci"}, Selector: &metav1.LabelSelector{}}
	require.NoError(t, cl.Create(ctx, up))

	credential := &v1alpha1.UpstreamCredential{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "shared"},
		Spec: v1alpha1.UpstreamCredentialSpec{
			UpstreamRef: v1alpha1.ObjectRef{Name: "ghcr"},
			SecretRef:   v1alpha1.CredentialsSecretRef{Name: "pat"},
			Ceiling:     []v1alpha1.AccessGrant{{Repositories: []string{"acme/**"}, Actions: []v1alpha1.Action{v1alpha1.ActionPull}}},
		},
	}
	require.NoError(t, cl.Create(ctx, credential))

	mirror := &v1alpha1.RegistryMirror{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ci", Name: "m"},
		Spec: v1alpha1.RegistryMirrorSpec{
			UpstreamRef: v1alpha1.ObjectRef{Namespace: "default", Name: "ghcr"},
			URL:         "https://mirror.example.com/proxy",
			PathPrefix:  "docker-hub",
		},
	}
	require.NoError(t, cl.Create(ctx, mirror))

	full := policy("full", validRule("build"))
	require.NoError(t, cl.Create(ctx, full))
}

func TestRegistrygateCRDsRejectBadMirrorURLs(t *testing.T) {
	cl := startCRDEnv(t)

	tests := map[string]struct {
		url   string
		valid bool
	}{
		"plain path":       {"https://mirror.example.com/a/b", true},
		"no path":          {"https://mirror.example.com", true},
		"percent in path":  {"https://mirror.example.com/a%2fb", false},
		"dot segment":      {"https://mirror.example.com/a/./b", false},
		"dot-dot segment":  {"https://mirror.example.com/a/../b", false},
		"empty segment":    {"https://mirror.example.com/a//b", false},
		"query":            {"https://mirror.example.com/a?x=1", false},
		"userinfo":         {"https://user@mirror.example.com/a", false},
		"trailing dot-dot": {"https://mirror.example.com/a/..", false},
	}

	i := 0
	for name, tc := range tests {
		i++

		t.Run(name, func(t *testing.T) {
			mirror := &v1alpha1.RegistryMirror{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ci", Name: strings.ReplaceAll(name, " ", "-")},
				Spec: v1alpha1.RegistryMirrorSpec{
					UpstreamRef: v1alpha1.ObjectRef{Namespace: "default", Name: "ghcr"},
					URL:         tc.url,
					PathPrefix:  "x",
				},
			}

			err := cl.Create(t.Context(), mirror)
			if tc.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestRegistrygateCRDsFitTheCELCostBudget(t *testing.T) {
	cl := startCRDEnv(t)

	globs := make([]string, 32)
	for i := range globs {
		globs[i] = "acme/team-*/app-*"
	}

	rules := make([]v1alpha1.AccessRule, 64)
	for i := range rules {
		rule := validRule("rule-" + strings.Repeat("a", i%5) + string(rune('a'+i%26)) + string(rune('a'+i/26)))
		rule.Repositories = globs
		rules[i] = rule
	}

	require.NoError(t, cl.Create(t.Context(), policy("big", rules...)))
}

func TestRegistrygateKindsShareTheCategory(t *testing.T) {
	cl := startCRDEnv(t)
	_ = cl

	for _, file := range []string{"registryaccesspolicies", "registrymirrors", "upstreamcredentials", "upstreamregistries"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "registrygate.soliddowant.dev_"+file+".yaml"))
		require.NoError(t, err)
		assert.Contains(t, string(data), "- registrygate", file)
	}
}
