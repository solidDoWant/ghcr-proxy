package model_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clientgotesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/model"
	"github.com/solidDoWant/registrygate/internal/secrets"
)

// startModel puts the fixture's objects in a fake client and runs a model
// on it until the test ends.
func startModel(t *testing.T, f *fixture) (*model.Model, client.Client) {
	t.Helper()

	return startModelWithStore(t, f, f.secrets, nil)
}

// startModelWithStore is startModel with the given Secret store. When
// instance is set, it gets the model before the model starts, so the
// store's OnChange can reach it.
func startModelWithStore(t *testing.T, f *fixture, store model.SecretStore, instance **model.Model) (*model.Model, client.Client) {
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

	built := model.New(model.Options{
		Reader:          kube,
		Secrets:         store,
		AdminNamespaces: f.inputs.AdminNamespaces,
	})
	if instance != nil {
		*instance = built
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		assert.NoError(t, built.Run(ctx))
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return built, kube
}

func TestModelIsReadyOnlyWithoutPendingSecrets(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.secrets.set(adminNamespace, "push-creds", secrets.Secret{State: secrets.Pending})

	instance, _ := startModel(t, f)

	assert.False(t, instance.Ready(), "not ready before the first snapshot")
	require.Eventually(t, func() bool { return instance.Snapshot() != nil }, 10*time.Second, 5*time.Millisecond)
	assert.False(t, instance.Ready(), "a pending Secret keeps the replica unready")

	f.secrets.mutex.Lock()
	assert.Contains(t, f.secrets.referenced, secrets.Key{Namespace: adminNamespace, Name: "push-creds"},
		"the model tells the store which Secrets to watch")
	f.secrets.mutex.Unlock()

	f.secrets.set(adminNamespace, "push-creds", present(map[string]string{"username": "robot", "password": "secret"}))
	instance.MarkDirty()

	require.Eventually(t, instance.Ready, 10*time.Second, 5*time.Millisecond)
}

func TestModelRebuildsAfterChanges(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	instance, kube := startModel(t, f)
	account := model.ServiceAccount{Namespace: tenantNamespace, Name: serviceAccount}

	require.Eventually(t, instance.Ready, 10*time.Second, 5*time.Millisecond)
	require.Contains(t, ruleNames(instance.Snapshot().Rules(account)), "push")

	var credential v1alpha1.UpstreamCredential
	require.NoError(t, kube.Get(t.Context(), client.ObjectKey{Namespace: adminNamespace, Name: "push"}, &credential))
	credential.Spec.AllowedNamespaces.Names = nil
	require.NoError(t, kube.Update(t.Context(), &credential))
	instance.MarkDirty()

	require.Eventually(t, func() bool {
		return !contains(ruleNames(instance.Snapshot().Rules(account)), "push")
	}, 10*time.Second, 5*time.Millisecond)
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}

	return false
}

// TestModelReadersSeeOneConsistentSnapshot reads while rebuilds publish
// new snapshots. Every reference inside a snapshot must point at that same
// snapshot's objects, and the rule and the credential it uses must come
// from the same generation of inputs.
func TestModelReadersSeeOneConsistentSnapshot(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.credential().Labels = map[string]string{"repository": "org/app"}
	instance, kube := startModel(t, f)
	account := model.ServiceAccount{Namespace: tenantNamespace, Name: serviceAccount}

	require.Eventually(t, instance.Ready, 10*time.Second, 5*time.Millisecond)

	var (
		stop     atomic.Bool
		readers  sync.WaitGroup
		failures atomic.Int64
		message  atomic.Value
	)

	fail := func(format string, args ...any) {
		failures.Add(1)
		message.Store(fmt.Sprintf(format, args...))
	}

	for range 4 {
		readers.Go(func() {
			for !stop.Load() {
				snapshot := instance.Snapshot()
				upstream := snapshot.Upstreams[name(adminNamespace, "ghcr")]
				credential := snapshot.Credentials[name(adminNamespace, "push")]

				if upstream == nil || credential == nil {
					fail("objects missing from a snapshot")
					continue
				}

				if credential.Upstream != upstream {
					fail("credential's upstream is from another snapshot")
				}

				if snapshot.Host("ghcr.io") != upstream {
					fail("host index is from another snapshot")
				}

				for _, rule := range snapshot.Rules(account) {
					if rule.Credential != nil && rule.Credential != credential {
						fail("rule's credential is from another snapshot")
					}

					if rule.Policy != snapshot.Policies[name(tenantNamespace, "builds")] {
						fail("rule's policy is from another snapshot")
					}

					// The writer keeps the credential's repository label
					// and the rule's repository in step.
					if rule.Credential != nil && rule.Repositories[0].String() != credential.Object.Labels["repository"] {
						fail("rule and credential come from different inputs")
					}
				}
			}
		})
	}

	// The writer changes the rule's repository and the credential's label
	// together, before each rebuild.
	for iteration := range 20 {
		repository := fmt.Sprintf("org/app-%d", iteration)

		var credential v1alpha1.UpstreamCredential
		require.NoError(t, kube.Get(t.Context(), client.ObjectKey{Namespace: adminNamespace, Name: "push"}, &credential))
		credential.Labels = map[string]string{"repository": repository}

		var policy v1alpha1.RegistryAccessPolicy
		require.NoError(t, kube.Get(t.Context(), client.ObjectKey{Namespace: tenantNamespace, Name: "builds"}, &policy))
		policy.Spec.Rules[0].Repositories = []string{repository}

		// The model only rebuilds when marked dirty, so both updates land
		// in the same rebuild.
		require.NoError(t, kube.Update(t.Context(), &credential))
		require.NoError(t, kube.Update(t.Context(), &policy))
		instance.MarkDirty()

		require.Eventually(t, func() bool {
			rules := instance.Snapshot().Rules(account)
			return len(rules) > 0 && rules[0].Repositories[0].String() == repository
		}, 10*time.Second, time.Millisecond)
	}

	stop.Store(true)
	readers.Wait()

	assert.Zero(t, failures.Load(), message.Load())
}

// A replica that is ready stays ready when a tenant adds a mirror whose
// Secret watch hasn't listed yet: only that mirror waits, so one tenant
// can't make every namespace's requests fail.
func TestModelStaysReadyWhenANewSecretIsPending(t *testing.T) {
	// The fake clientset sends no initial-events bookmark.
	t.Setenv("KUBE_FEATURE_WatchListClient", "false")

	f := newFixture(t)
	certificate, privateKey := keyPair(t, "ghcr.io")

	clientset := kubefake.NewClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: "ghcr-tls"},
			Data:       map[string][]byte{"tls.crt": []byte(certificate), "tls.key": []byte(privateKey)},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: adminNamespace, Name: "push-creds"},
			Data:       map[string][]byte{"username": []byte("robot"), "password": []byte("secret")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: tenantNamespace, Name: "robot"},
			Data:       map[string][]byte{"username": []byte("r"), "password": []byte("p")},
		},
	)

	release := make(chan struct{})

	// The API server doesn't answer the tenant Secret's first list.
	clientset.PrependReactor("list", "secrets", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == tenantNamespace {
			<-release
		}

		return false, nil, nil
	})
	t.Cleanup(func() { close(release) })

	var instance *model.Model

	store := secrets.New(clientset, secrets.Options{
		AdminNamespaces: []string{adminNamespace},
		OnChange:        func() { instance.MarkDirty() },
	})

	_, kube := startModelWithStore(t, f, store, &instance)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	go func() { _ = store.Run(ctx) }()

	require.Eventually(t, instance.Ready, 10*time.Second, 5*time.Millisecond, "ready with every Secret loaded")

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
		mirror := instance.Snapshot().Mirrors[name(tenantNamespace, "harbor-b")]
		return mirror != nil && mirror.Status.Pending
	}, 10*time.Second, 5*time.Millisecond, "the new mirror waits for its Secret")

	snapshot := instance.Snapshot()
	assert.True(t, snapshot.PendingSecrets(), "the new mirror's Secret is still pending")
	assert.True(t, instance.Ready(), "the replica stays ready; only the new mirror waits for its Secret")
	assert.False(t, snapshot.Mirrors[name(tenantNamespace, "harbor")].Status.Pending,
		"objects that don't use the pending Secret aren't pending")
}
