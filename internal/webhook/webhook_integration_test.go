//go:build integration

package webhook_test

import (
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
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/config"
	"github.com/solidDoWant/registrygate/internal/model"
	"github.com/solidDoWant/registrygate/internal/webhook"
)

func webhookRegistration(name, path, resource string) admissionregistrationv1.ValidatingWebhook {
	return admissionregistrationv1.ValidatingWebhook{
		Name:                    name + ".registrygate.soliddowant.dev",
		AdmissionReviewVersions: []string{"v1"},
		SideEffects:             ptr.To(admissionregistrationv1.SideEffectClassNone),
		FailurePolicy:           ptr.To(admissionregistrationv1.Fail),
		MatchPolicy:             ptr.To(admissionregistrationv1.Equivalent),
		TimeoutSeconds:          ptr.To(int32(5)),
		ClientConfig:            admissionregistrationv1.WebhookClientConfig{Service: &admissionregistrationv1.ServiceReference{Path: ptr.To(path)}},
		Rules: []admissionregistrationv1.RuleWithOperations{{
			Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
			Rule: admissionregistrationv1.Rule{
				APIGroups: []string{v1alpha1.GroupVersion.Group}, APIVersions: []string{"v1alpha1"}, Resources: []string{resource},
			},
		}},
	}
}

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
				ObjectMeta: metav1.ObjectMeta{Name: "registrygate"},
				Webhooks: []admissionregistrationv1.ValidatingWebhook{
					webhookRegistration("upstreamregistries", webhook.UpstreamPath, "upstreamregistries"),
					webhookRegistration("registryaccesspolicies", webhook.PolicyPath, "registryaccesspolicies"),
				},
			}},
		},
	}

	restConfig, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })

	options := env.WebhookInstallOptions

	informers, err := cache.New(restConfig, cache.Options{Scheme: scheme})
	require.NoError(t, err)

	snapshots := model.New(model.Options{Reader: informers, Informers: informers, AdminNamespaces: []string{"registrygate"}})

	server, err := webhook.NewServer(config.Webhook{
		Listen:   fmt.Sprintf("%s:%d", options.LocalServingHost, options.LocalServingPort),
		CertFile: filepath.Join(options.LocalServingCertDir, "tls.crt"),
		KeyFile:  filepath.Join(options.LocalServingCertDir, "tls.key"),
	}, scheme, []string{"registrygate"}, snapshots.Snapshot)
	require.NoError(t, err)

	ctx := t.Context()
	done := make(chan error, 3)

	go func() { done <- informers.Start(ctx) }()
	go func() { done <- snapshots.Run(ctx) }()
	go func() { done <- server.Start(ctx) }()

	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	require.NoError(t, err)

	for _, name := range []string{"registrygate", "ci-a"} {
		require.NoError(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}))
	}

	upstream := &v1alpha1.UpstreamRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: "ghcr", Namespace: "ci-a"},
		Spec: v1alpha1.UpstreamRegistrySpec{
			Hosts: []string{"ghcr.io"}, URL: "https://ghcr.example",
			TLS: v1alpha1.ServingTLS{SecretRef: v1alpha1.LocalRef{Name: "tls"}},
		},
	}

	// The first requests may arrive before the server listens.
	require.Eventually(t, func() bool {
		err := c.Create(ctx, upstream)
		return apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err)
	}, 30*time.Second, 100*time.Millisecond, "an upstream outside the admin namespaces is rejected")

	upstream.Namespace = "registrygate"
	require.NoError(t, c.Create(ctx, upstream))

	credential := &v1alpha1.UpstreamCredential{
		ObjectMeta: metav1.ObjectMeta{Name: "push", Namespace: "registrygate"},
		Spec: v1alpha1.UpstreamCredentialSpec{
			UpstreamRef: v1alpha1.ObjectRef{Name: "ghcr"},
			SecretRef:   v1alpha1.CredentialsSecretRef{Name: "creds"},
			Ceiling:     []v1alpha1.AccessGrant{{Repositories: []string{"org/**"}, Actions: []v1alpha1.Action{"push"}}},
		},
	}
	require.NoError(t, c.Create(ctx, credential))

	newPolicy := func(name string, refs *v1alpha1.ObjectRef, repositories ...string) *v1alpha1.RegistryAccessPolicy {
		return &v1alpha1.RegistryAccessPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ci-a"},
			Spec: v1alpha1.RegistryAccessPolicySpec{
				ServiceAccounts: []string{"builder"},
				Rules: []v1alpha1.AccessRule{{
					Name: "push", CredentialRef: refs, Repositories: repositories, Actions: []v1alpha1.Action{"push"},
				}},
			},
		}
	}

	// A policy whose credential does not exist yet applies.
	require.Eventually(t, func() bool {
		return c.Create(ctx, newPolicy("later", &v1alpha1.ObjectRef{Namespace: "registrygate", Name: "missing"}, "anything")) == nil
	}, 30*time.Second, 100*time.Millisecond, "a policy with a credential that does not exist applies")

	// The snapshot learns of the credential from the informer.
	inside := newPolicy("inside", &v1alpha1.ObjectRef{Namespace: "registrygate", Name: "push"}, "org/app")

	var outside error

	require.Eventually(t, func() bool {
		outside = c.Create(ctx, newPolicy("outside", &v1alpha1.ObjectRef{Namespace: "registrygate", Name: "push"}, "org/app", "secret/x"))
		return outside != nil
	}, 30*time.Second, 100*time.Millisecond, "a rule outside the ceiling is rejected")

	assert.Contains(t, outside.Error(), "secret/x")
	assert.Contains(t, outside.Error(), `"push"`)
	assert.NotContains(t, outside.Error(), "org/app push")

	require.NoError(t, c.Create(ctx, inside))

	inside.Spec.Rules[0].Repositories = []string{"secret/x"}
	require.Error(t, c.Update(ctx, inside))
}
