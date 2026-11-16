package webhook_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/ghcr-proxy/api/v1alpha1"
	"github.com/solidDoWant/ghcr-proxy/internal/config"
	"github.com/solidDoWant/ghcr-proxy/internal/webhook"
)

const grantsYAML = `  - namespaces: "ci-(?P<repo>[a-z0-9-]+)"
    repositories: ["soliddowant/${repo}", "soliddowant/${repo}/**"]
    actions: [pull, push]
`

func loadGrants(t *testing.T) config.Grants {
	t.Helper()

	pat := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(pat, []byte("x"), 0o600))

	cfg, err := config.Parse([]byte("upstream:\n  patFile: " + pat + "\ngrants:\n" + grantsYAML))
	require.NoError(t, err)

	return cfg.Grants
}

func policy(namespace string, repositories []string, actions ...v1alpha1.Action) *v1alpha1.GHCRAccessPolicy {
	return &v1alpha1.GHCRAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: namespace},
		Spec: v1alpha1.GHCRAccessPolicySpec{
			ServiceAccounts: []string{"builder"},
			Rules:           []v1alpha1.AccessRule{{Repositories: repositories, Actions: actions}},
		},
	}
}

func TestValidateCreate(t *testing.T) {
	validator := &webhook.Validator{Grants: loadGrants(t)}

	tests := []struct {
		name        string
		policy      *v1alpha1.GHCRAccessPolicy
		wantMessage []string
	}{
		{name: "inside the grant", policy: policy("ci-foo", []string{"soliddowant/foo", "soliddowant/foo/**"}, "push")},
		{
			name:        "pattern outside the grant",
			policy:      policy("ci-foo", []string{"soliddowant/foo", "soliddowant/other"}, "push"),
			wantMessage: []string{`"soliddowant/other"`, "OutsideGrant"},
		},
		{
			name:        "action outside the grant",
			policy:      policy("ci-foo", []string{"soliddowant/foo"}, "delete"),
			wantMessage: []string{`"soliddowant/foo"`, "delete"},
		},
		{
			name:        "no grant for the namespace",
			policy:      policy("other", []string{"soliddowant/foo"}, "pull"),
			wantMessage: []string{`"soliddowant/foo"`, "NoGrant"},
		},
		{
			name:        "invalid pattern",
			policy:      policy("ci-foo", []string{"Soliddowant/Foo"}, "pull"),
			wantMessage: []string{`"Soliddowant/Foo"`, "InvalidPattern"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := validator.ValidateCreate(t.Context(), test.policy)
			if test.wantMessage == nil {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)

			for _, want := range test.wantMessage {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestValidateUpdate(t *testing.T) {
	validator := &webhook.Validator{Grants: loadGrants(t)}
	outside := func() *v1alpha1.GHCRAccessPolicy { return policy("ci-foo", []string{"soliddowant/other"}, "push") }

	t.Run("unchanged spec is admitted even outside the grant", func(t *testing.T) {
		updated := outside()
		updated.Labels = map[string]string{"a": "b"}
		updated.Finalizers = []string{"example.com/f"}

		_, err := validator.ValidateUpdate(t.Context(), outside(), updated)
		require.NoError(t, err)
	})

	t.Run("spec change outside the grant is rejected", func(t *testing.T) {
		old := policy("ci-foo", []string{"soliddowant/foo"}, "push")

		_, err := validator.ValidateUpdate(t.Context(), old, outside())
		require.ErrorContains(t, err, "soliddowant/other")
	})

	t.Run("spec change inside the grant is admitted", func(t *testing.T) {
		_, err := validator.ValidateUpdate(t.Context(), outside(), policy("ci-foo", []string{"soliddowant/foo"}, "push"))
		require.NoError(t, err)
	})

	t.Run("object being deleted is admitted", func(t *testing.T) {
		deleting := policy("ci-foo", []string{"soliddowant/another"}, "push")
		now := metav1.Now()
		deleting.DeletionTimestamp = &now

		_, err := validator.ValidateUpdate(t.Context(), outside(), deleting)
		require.NoError(t, err)
	})
}

func TestValidateDeleteAdmits(t *testing.T) {
	validator := &webhook.Validator{Grants: loadGrants(t)}

	_, err := validator.ValidateDelete(t.Context(), outsidePolicy())
	require.NoError(t, err)
}

func outsidePolicy() *v1alpha1.GHCRAccessPolicy {
	return policy("other", []string{"x/y"}, "push")
}
