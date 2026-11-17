package model_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/model"
	"github.com/solidDoWant/registrygate/internal/secrets"
)

// A replica that is ready stays ready when a tenant creates a mirror whose
// Secret watch hasn't listed yet: only that mirror waits.
func TestNewTenantSecretDoesNotMakeAReadyReplicaUnready(t *testing.T) {
	// The fake clientset sends no initial-events bookmark.
	t.Setenv("KUBE_FEATURE_WatchListClient", "false")

	f := newFixture(t)
	certificate, privateKey := keyPair(t, "ghcr.io")

	clientset := kubefake.NewClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: "ghcr-tls"},
			Data: map[string][]byte{"tls.crt": []byte(certificate), "tls.key": []byte(privateKey)}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: "push-creds"},
			Data: map[string][]byte{"username": []byte("robot"), "password": []byte("secret")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: tenantNamespace, Name: "robot"},
			Data: map[string][]byte{"username": []byte("r"), "password": []byte("p")}},
	)

	release := make(chan struct{})
	clientset.PrependReactor("list", "secrets", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == tenantNamespace {
			<-release // the API server is slow to answer the tenant Secret's first list
		}

		return false, nil, nil
	})
	t.Cleanup(func() { close(release) })

	var instance *model.Model

	store := secrets.New(clientset, secrets.Options{
		AdminNamespaces: []string{adminNamespace},
		OnChange:        func() { instance.MarkDirty() },
	})

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	f.secrets = nil
	f.inputs.Secrets = nil
	_, kube := startModelWithStore(t, f, store, &instance)

	go func() { _ = store.Run(ctx) }()

	require.Eventually(t, instance.Ready, 10*time.Second, 5*time.Millisecond, "ready with every Secret loaded")

	// A tenant in another namespace adds its own mirror with a credentials Secret.
	require.NoError(t, kube.Create(t.Context(), &v1alpha1.RegistryMirror{
		ObjectMeta: metav1.ObjectMeta{Namespace: tenantNamespace, Name: "harbor-b"},
		Spec: v1alpha1.RegistryMirrorSpec{
			UpstreamRef: v1alpha1.ObjectRef{Namespace: adminNamespace, Name: "ghcr"},
			URL:         "https://harbor.example/base/ci-b",
			PathPrefix:  "ci-b-ghcr",
			SecretRef:   &v1alpha1.CredentialsSecretRef{Name: "robot"},
		},
	}))
	instance.MarkDirty()

	require.Eventually(t, func() bool {
		snapshot := instance.Snapshot()
		mirror := snapshot.Mirrors[name(tenantNamespace, "harbor-b")]
		return mirror != nil && mirror.Status.Pending
	}, 10*time.Second, 5*time.Millisecond, "the new mirror waits for its Secret")

	assert.True(t, instance.Ready(), "the replica must stay ready; only the new mirror waits for its Secret")
}

func startModelWithStore(t *testing.T, f *fixture, store *secrets.Store, out **model.Model) (*model.Model, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	var objects []client.Object
	for namespace, labels := range f.inputs.Namespaces {
		objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: labels}})
	}

	for index := range f.inputs.Upstreams {
		objects = append(objects, &f.inputs.Upstreams[index])
	}

	for index := range f.inputs.Credentials {
		objects = append(objects, &f.inputs.Credentials[index])
	}

	for index := range f.inputs.Mirrors {
		objects = append(objects, &f.inputs.Mirrors[index])
	}

	for index := range f.inputs.Policies {
		objects = append(objects, &f.inputs.Policies[index])
	}

	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	instance := model.New(model.Options{Reader: kube, Secrets: store, AdminNamespaces: f.inputs.AdminNamespaces})
	*out = instance

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		assert.NoError(t, instance.Run(ctx))
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return instance, kube
}
