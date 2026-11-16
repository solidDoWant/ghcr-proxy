package v1alpha1_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
)

func TestAddToSchemeRegistersTheAccessPolicyKinds(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	tests := []struct {
		name   string
		object runtime.Object
		kind   string
	}{
		{name: "policy", object: &v1alpha1.GHCRAccessPolicy{}, kind: "GHCRAccessPolicy"},
		{name: "policy list", object: &v1alpha1.GHCRAccessPolicyList{}, kind: "GHCRAccessPolicyList"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kinds, _, err := scheme.ObjectKinds(test.object)
			require.NoError(t, err)
			require.Len(t, kinds, 1)
			assert.Equal(t, "registrygate.soliddowant.dev", kinds[0].Group)
			assert.Equal(t, "v1alpha1", kinds[0].Version)
			assert.Equal(t, test.kind, kinds[0].Kind)
		})
	}
}
