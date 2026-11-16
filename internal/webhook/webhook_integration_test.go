//go:build integration

package webhook_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/solidDoWant/ghcr-proxy/api/v1alpha1"
	"github.com/solidDoWant/ghcr-proxy/internal/config"
	"github.com/solidDoWant/ghcr-proxy/internal/webhook"
)

func TestAdmissionRejectsPoliciesOutsideTheGrant(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set (run from the dev shell)")
	}

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			ValidatingWebhooks: []*admissionregistrationv1.ValidatingWebhookConfiguration{{
				ObjectMeta: metav1.ObjectMeta{Name: "ghcr-proxy"},
				Webhooks: []admissionregistrationv1.ValidatingWebhook{{
					Name:                    "ghcraccesspolicies.ghcr-proxy.soliddowant.dev",
					AdmissionReviewVersions: []string{"v1"},
					SideEffects:             ptr.To(admissionregistrationv1.SideEffectClassNone),
					FailurePolicy:           ptr.To(admissionregistrationv1.Fail),
					MatchPolicy:             ptr.To(admissionregistrationv1.Equivalent),
					TimeoutSeconds:          ptr.To(int32(5)),
					ClientConfig:            admissionregistrationv1.WebhookClientConfig{Service: &admissionregistrationv1.ServiceReference{Path: ptr.To(webhook.Path)}},
					Rules: []admissionregistrationv1.RuleWithOperations{{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
						Rule: admissionregistrationv1.Rule{
							APIGroups: []string{v1alpha1.GroupVersion.Group}, APIVersions: []string{"v1alpha1"}, Resources: []string{"ghcraccesspolicies"},
						},
					}},
				}},
			}},
		},
	}

	restConfig, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })

	options := env.WebhookInstallOptions

	pat := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(pat, []byte("x"), 0o600))

	cfg, err := config.Parse([]byte("upstream:\n  patFile: " + pat + "\ngrants:\n" + grantsYAML))
	require.NoError(t, err)

	server, err := webhook.NewServer(config.Webhook{
		Listen:   fmt.Sprintf("%s:%d", options.LocalServingHost, options.LocalServingPort),
		CertFile: filepath.Join(options.LocalServingCertDir, "tls.crt"),
		KeyFile:  filepath.Join(options.LocalServingCertDir, "tls.key"),
	}, scheme, cfg.Grants)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- server.Start(ctx) }()

	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})

	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	require.NoError(t, err)
	require.NoError(t, c.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ci-foo"}}))

	// The first requests may arrive before the server listens.
	var inside *v1alpha1.GHCRAccessPolicy

	require.Eventually(t, func() bool {
		inside = policy("ci-foo", []string{"soliddowant/foo"}, "push")
		inside.Name = "inside"

		return c.Create(t.Context(), inside) == nil
	}, 30*time.Second, 100*time.Millisecond, "a policy inside the grant applies")

	outside := policy("ci-foo", []string{"soliddowant/foo", "soliddowant/secret"}, "push")
	outside.Name = "outside"

	err = c.Create(t.Context(), outside)
	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err), "got %v", err)
	assert.Contains(t, err.Error(), `"soliddowant/secret"`)
	assert.NotContains(t, err.Error(), `rules[0] "soliddowant/foo"`)

	inside.Spec.Rules[0].Repositories = []string{"soliddowant/secret"}
	err = c.Update(t.Context(), inside)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"soliddowant/secret"`)
}
