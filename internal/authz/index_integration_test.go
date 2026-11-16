//go:build integration

package authz_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/authn"
	"github.com/solidDoWant/registrygate/internal/authz"
)

// policyChangeDeadline is how soon a policy change must reach decisions.
const policyChangeDeadline = 5 * time.Second

func TestDecideFollowsLivePolicyChangesWithoutRestart(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set (run from the dev shell)")
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	restConfig, err := env.Start()
	require.NoError(t, err)

	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	kube, err := client.New(restConfig, client.Options{Scheme: scheme})
	require.NoError(t, err)

	sum := sha256.Sum256([]byte(t.Name()))
	suffix := hex.EncodeToString(sum[:])[:8]
	namespace := "ci-" + suffix
	require.NoError(t, kube.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))

	informers, err := cache.New(restConfig, cache.Options{Scheme: scheme})
	require.NoError(t, err)

	index := authz.NewIndex(loadGrants(t, chartGrant))
	require.NoError(t, index.Watch(t.Context(), informers))

	go func() { _ = informers.Start(t.Context()) }()

	require.True(t, informers.WaitForCacheSync(t.Context()))
	require.Eventually(t, index.Synced, policyChangeDeadline, 50*time.Millisecond)

	authorizer := &authz.Authorizer{Index: index}
	caller := authn.Identity{Namespace: namespace, ServiceAccount: "builder"}
	repository := "soliddowant/" + suffix + "/app"
	push := classify(t, http.MethodPut, "/v2/"+repository+"/manifests/latest")

	require.Equal(t, authz.OutcomeDenied, authorizer.Decide(caller, push).Outcome)

	policy := namedPolicy(namespace, "access", []string{"builder"}, accessRule([]string{repository}, v1alpha1.ActionPull))
	require.NoError(t, kube.Create(t.Context(), policy))

	policy.Spec.Rules = append(policy.Spec.Rules, accessRule([]string{repository}, v1alpha1.ActionPush))
	require.NoError(t, kube.Update(t.Context(), policy))

	require.Eventually(t, func() bool {
		return authorizer.Decide(caller, push).Outcome == authz.OutcomeForward
	}, policyChangeDeadline, 50*time.Millisecond, "adding a push rule must allow the next push")

	policy.Spec.Rules = policy.Spec.Rules[:1]
	require.NoError(t, kube.Update(t.Context(), policy))

	require.Eventually(t, func() bool {
		return authorizer.Decide(caller, push).Outcome == authz.OutcomeDenied
	}, policyChangeDeadline, 50*time.Millisecond, "removing the push rule must refuse the next push")
}
