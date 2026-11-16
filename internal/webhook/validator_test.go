package webhook_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/model"
	"github.com/solidDoWant/registrygate/internal/webhook"
)

const adminNamespace = "registrygate"

// snapshotWithCredential returns a snapshot holding one credential whose ceiling is push on org/**.
func snapshotWithCredential() *model.Snapshot {
	return model.Build(model.Inputs{
		AdminNamespaces: []string{adminNamespace},
		Credentials: []v1alpha1.UpstreamCredential{{
			ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: "push", Generation: 1},
			Spec: v1alpha1.UpstreamCredentialSpec{
				UpstreamRef: v1alpha1.ObjectRef{Name: "ghcr"},
				Ceiling: []v1alpha1.AccessGrant{{
					Repositories: []string{"org/**"},
					Actions:      []v1alpha1.Action{v1alpha1.ActionPush},
				}},
			},
		}},
	})
}

func credentialRule(name, credential string, repositories []string, actions ...v1alpha1.Action) v1alpha1.AccessRule {
	return v1alpha1.AccessRule{
		Name:          name,
		CredentialRef: &v1alpha1.ObjectRef{Namespace: adminNamespace, Name: credential},
		Repositories:  repositories,
		Actions:       actions,
	}
}

func policy(rules ...v1alpha1.AccessRule) *v1alpha1.RegistryAccessPolicy {
	return &v1alpha1.RegistryAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ci-a"},
		Spec:       v1alpha1.RegistryAccessPolicySpec{ServiceAccounts: []string{"builder"}, Rules: rules},
	}
}

func newPolicyValidator(snapshot *model.Snapshot) *webhook.PolicyValidator {
	return &webhook.PolicyValidator{Snapshot: func() *model.Snapshot { return snapshot }}
}

func TestValidateCreate(t *testing.T) {
	validator := newPolicyValidator(snapshotWithCredential())

	tests := []struct {
		name        string
		policy      *v1alpha1.RegistryAccessPolicy
		wantMessage []string
	}{
		{name: "inside the ceiling", policy: policy(credentialRule("push", "push", []string{"org/app", "org/app/**"}, "push"))},
		{
			name:        "pattern outside the ceiling names the rule and the pattern",
			policy:      policy(credentialRule("wide", "push", []string{"org/app", "other/**"}, "push")),
			wantMessage: []string{`"wide"`, "other/** push", "push"},
		},
		{
			name:        "action outside the ceiling",
			policy:      policy(credentialRule("del", "push", []string{"org/app"}, "delete")),
			wantMessage: []string{`"del"`, "org/app delete"},
		},
		{
			name:        "only the rules outside are named",
			policy:      policy(credentialRule("fine", "push", []string{"org/a"}, "push"), credentialRule("bad", "push", []string{"x"}, "push")),
			wantMessage: []string{`"bad"`},
		},
		{name: "credential that does not exist yet", policy: policy(credentialRule("later", "missing", []string{"anything"}, "delete"))},
		{
			name: "mirror rule has no ceiling",
			policy: policy(v1alpha1.AccessRule{
				Name: "pull", MirrorRef: &v1alpha1.ObjectRef{Name: "harbor"}, Repositories: []string{"**"}, Actions: []v1alpha1.Action{"pull"},
			}),
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

	t.Run("the rule that is inside is not named", func(t *testing.T) {
		_, err := validator.ValidateCreate(t.Context(),
			policy(credentialRule("fine", "push", []string{"org/a"}, "push"), credentialRule("bad", "push", []string{"x"}, "push")))
		require.Error(t, err)
		assert.NotContains(t, err.Error(), `"fine"`)
	})

	t.Run("not ready fails closed", func(t *testing.T) {
		_, err := newPolicyValidator(nil).ValidateCreate(t.Context(), policy())
		require.Error(t, err)
	})
}

func TestValidateUpdate(t *testing.T) {
	validator := newPolicyValidator(snapshotWithCredential())
	outside := func() *v1alpha1.RegistryAccessPolicy {
		return policy(credentialRule("wide", "push", []string{"other"}, "push"))
	}

	t.Run("unchanged spec is admitted even outside the ceiling", func(t *testing.T) {
		updated := outside()
		updated.Labels = map[string]string{"a": "b"}
		updated.Finalizers = []string{"example.com/f"}

		_, err := validator.ValidateUpdate(t.Context(), outside(), updated)
		require.NoError(t, err)
	})

	t.Run("spec change outside the ceiling is rejected", func(t *testing.T) {
		old := policy(credentialRule("wide", "push", []string{"org/a"}, "push"))

		_, err := validator.ValidateUpdate(t.Context(), old, outside())
		require.ErrorContains(t, err, "other")
	})

	t.Run("spec change inside the ceiling is admitted", func(t *testing.T) {
		_, err := validator.ValidateUpdate(t.Context(), outside(), policy(credentialRule("wide", "push", []string{"org/a"}, "push")))
		require.NoError(t, err)
	})

	t.Run("object being deleted is admitted", func(t *testing.T) {
		deleting := policy(credentialRule("wide", "push", []string{"another"}, "push"))
		now := metav1.Now()
		deleting.DeletionTimestamp = &now

		_, err := validator.ValidateUpdate(t.Context(), outside(), deleting)
		require.NoError(t, err)
	})
}

func TestValidateDeleteAdmits(t *testing.T) {
	_, err := newPolicyValidator(snapshotWithCredential()).ValidateDelete(t.Context(), policy())
	require.NoError(t, err)

	_, err = (&webhook.UpstreamValidator{}).ValidateDelete(t.Context(), &v1alpha1.UpstreamRegistry{})
	require.NoError(t, err)
}

func upstream(namespace, url string) *v1alpha1.UpstreamRegistry {
	return &v1alpha1.UpstreamRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: "u", Namespace: namespace},
		Spec:       v1alpha1.UpstreamRegistrySpec{URL: url},
	}
}

func TestUpstreamValidatorAdmitsOnlyAdminNamespaces(t *testing.T) {
	validator := &webhook.UpstreamValidator{AdminNamespaces: []string{adminNamespace, "other-admin"}}

	_, err := validator.ValidateCreate(t.Context(), upstream(adminNamespace, "https://a"))
	require.NoError(t, err)

	_, err = validator.ValidateCreate(t.Context(), upstream("other-admin", "https://a"))
	require.NoError(t, err)

	_, err = validator.ValidateCreate(t.Context(), upstream("ci-a", "https://a"))
	require.ErrorContains(t, err, `"ci-a"`)

	t.Run("spec change outside is rejected", func(t *testing.T) {
		_, err := validator.ValidateUpdate(t.Context(), upstream("ci-a", "https://a"), upstream("ci-a", "https://b"))
		require.Error(t, err)
	})

	t.Run("unchanged spec is admitted", func(t *testing.T) {
		updated := upstream("ci-a", "https://a")
		updated.Labels = map[string]string{"a": "b"}

		_, err := validator.ValidateUpdate(t.Context(), upstream("ci-a", "https://a"), updated)
		require.NoError(t, err)
	})

	t.Run("deleting object is admitted", func(t *testing.T) {
		deleting := upstream("ci-a", "https://b")
		now := metav1.Now()
		deleting.DeletionTimestamp = &now

		_, err := validator.ValidateUpdate(t.Context(), upstream("ci-a", "https://a"), deleting)
		require.NoError(t, err)
	})

	t.Run("no admin namespaces admits nothing", func(t *testing.T) {
		_, err := (&webhook.UpstreamValidator{}).ValidateCreate(t.Context(), upstream(adminNamespace, "https://a"))
		require.Error(t, err)
	})
}
