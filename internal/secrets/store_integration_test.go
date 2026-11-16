//go:build integration

package secrets_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/solidDoWant/registrygate/internal/secrets"
)

const proxyUser = "system:serviceaccount:registrygate:registrygate"

// cluster is an envtest API server with RBAC on. admin may do anything;
// proxy is the store's identity and may read only what a Role grants it.
type cluster struct {
	admin   kubernetes.Interface
	proxy   kubernetes.Interface
	watches *openWatches
}

// openWatches counts the proxy's watch requests that are still open, by
// field selector, so a test can see a watch end on the wire.
type openWatches struct {
	mutex sync.Mutex
	open  map[string]int
}

func (o *openWatches) count(fieldSelector string) int {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	return o.open[fieldSelector]
}

func (o *openWatches) add(fieldSelector string, delta int) {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	o.open[fieldSelector] += delta
}

type countingTransport struct {
	next    http.RoundTripper
	watches *openWatches
}

func (c countingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	query := request.URL.Query()
	if query.Get("watch") != "true" && query.Get("watch") != "1" {
		return c.next.RoundTrip(request)
	}

	response, err := c.next.RoundTrip(request)
	if err != nil {
		return response, err
	}

	fieldSelector := query.Get("fieldSelector")
	c.watches.add(fieldSelector, 1)

	response.Body = &countedBody{ReadCloser: response.Body, done: func() { c.watches.add(fieldSelector, -1) }}

	return response, nil
}

type countedBody struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (b *countedBody) Close() error {
	b.once.Do(b.done)
	return b.ReadCloser.Close()
}

func startCluster(t *testing.T) cluster {
	t.Helper()

	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run inside the dev shell")
	}

	environment := &envtest.Environment{}
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	config, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	admin, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)
	user, err := environment.ControlPlane.AddUser(envtest.User{Name: proxyUser}, nil)
	require.NoError(t, err)

	watches := &openWatches{open: map[string]int{}}
	proxyConfig := user.Config()
	proxyConfig.Wrap(func(next http.RoundTripper) http.RoundTripper {
		return countingTransport{next: next, watches: watches}
	})
	proxy, err := kubernetes.NewForConfig(proxyConfig)
	require.NoError(t, err)

	return cluster{admin: admin, proxy: proxy, watches: watches}
}

func (c cluster) createNamespace(t *testing.T, name string) {
	t.Helper()
	_, err := c.admin.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func (c cluster) createSecret(t *testing.T, namespace, name, password string) {
	t.Helper()
	_, err := c.admin.CoreV1().Secrets(namespace).Create(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Data:       map[string][]byte{"password": []byte(password)},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func (c cluster) updateSecret(t *testing.T, namespace, name, password string) string {
	t.Helper()
	secret, err := c.admin.CoreV1().Secrets(namespace).Get(t.Context(), name, metav1.GetOptions{})
	require.NoError(t, err)

	secret.Data = map[string][]byte{"password": []byte(password)}
	updated, err := c.admin.CoreV1().Secrets(namespace).Update(t.Context(), secret, metav1.UpdateOptions{})
	require.NoError(t, err)

	return updated.ResourceVersion
}

// grantSecret lets the proxy read only the named Secrets in namespace, the
// way a tenant grants it.
func (c cluster) grantSecret(t *testing.T, namespace string, names ...string) {
	t.Helper()
	ctx := t.Context()
	_, err := c.admin.RbacV1().Roles(namespace).Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "registrygate-secrets"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"secrets"},
			ResourceNames: names,
			Verbs:         []string{"get", "list", "watch"},
		}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = c.admin.RbacV1().RoleBindings(namespace).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "registrygate-secrets"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "registrygate-secrets"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: proxyUser}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

// grantNamespace lets the proxy read every Secret in namespace, as the
// chart's Role in an admin namespace does.
func (c cluster) grantNamespace(t *testing.T, namespace string) {
	t.Helper()
	c.grantSecret(t, namespace)
}

func runStore(t *testing.T, client kubernetes.Interface, options secrets.Options) *secrets.Store {
	t.Helper()

	store := secrets.New(client, options)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)

		assert.NoError(t, store.Run(ctx))
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return store
}

func requireSecret(t *testing.T, store *secrets.Store, key secrets.Key, check func(secrets.Secret) bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		secret, tracked := store.Get(key)
		return tracked && check(secret)
	}, 30*time.Second, 50*time.Millisecond)
}

func hasPassword(password string) func(secrets.Secret) bool {
	return func(secret secrets.Secret) bool {
		return secret.State == secrets.Present && string(secret.Data["password"]) == password
	}
}

func inState(state secrets.State) func(secrets.Secret) bool {
	return func(secret secrets.Secret) bool { return secret.State == state }
}

func TestSecretStoreFollowsTenantSecrets(t *testing.T) {
	c := startCluster(t)
	c.createNamespace(t, "registrygate")

	t.Run("a Secret readable through a resourceNames Role reports each change", func(t *testing.T) {
		namespace := "tenant-role"
		c.createNamespace(t, namespace)
		c.createSecret(t, namespace, "robot", "first")
		c.createSecret(t, namespace, "other", "unrelated")
		c.grantSecret(t, namespace, "robot")

		changes := make(chan struct{}, 1)
		store := runStore(t, c.proxy, secrets.Options{
			AdminNamespaces: []string{"registrygate"},
			OnChange: func() {
				select {
				case changes <- struct{}{}:
				default:
				}
			},
		})
		key := secrets.Key{Namespace: namespace, Name: "robot"}
		store.SetReferenced([]secrets.Key{key})
		requireSecret(t, store, key, hasPassword("first"))

		<-changes

		resourceVersion := c.updateSecret(t, namespace, "robot", "second")
		requireSecret(t, store, key, func(secret secrets.Secret) bool {
			return hasPassword("second")(secret) && secret.ResourceVersion == resourceVersion
		})

		select {
		case <-changes:
		case <-time.After(30 * time.Second):
			t.Fatal("the store did not report the change")
		}

		require.NoError(t, c.admin.CoreV1().Secrets(namespace).Delete(t.Context(), "robot", metav1.DeleteOptions{}))
		requireSecret(t, store, key, inState(secrets.NotFound))
	})

	t.Run("a Secret without a Role is forbidden until a Role is created", func(t *testing.T) {
		namespace := "tenant-late-role"
		c.createNamespace(t, namespace)
		c.createSecret(t, namespace, "robot", "hidden")

		store := runStore(t, c.proxy, secrets.Options{AdminNamespaces: []string{"registrygate"}})
		key := secrets.Key{Namespace: namespace, Name: "robot"}
		store.SetReferenced([]secrets.Key{key})
		requireSecret(t, store, key, inState(secrets.Forbidden))

		c.grantSecret(t, namespace, "robot")
		requireSecret(t, store, key, hasPassword("hidden"))
	})

	t.Run("a referenced Secret that does not exist is not found", func(t *testing.T) {
		namespace := "tenant-missing"
		c.createNamespace(t, namespace)
		c.grantSecret(t, namespace, "robot")

		store := runStore(t, c.proxy, secrets.Options{AdminNamespaces: []string{"registrygate"}})
		key := secrets.Key{Namespace: namespace, Name: "robot"}
		store.SetReferenced([]secrets.Key{key})
		requireSecret(t, store, key, inState(secrets.NotFound))

		c.createSecret(t, namespace, "robot", "created")
		requireSecret(t, store, key, hasPassword("created"))
	})

	t.Run("dropping the last reference stops the watch", func(t *testing.T) {
		namespace := "tenant-dropped"
		c.createNamespace(t, namespace)
		c.createSecret(t, namespace, "kept", "kept")
		c.createSecret(t, namespace, "dropped", "dropped")
		c.grantSecret(t, namespace, "kept", "dropped")

		store := runStore(t, c.proxy, secrets.Options{AdminNamespaces: []string{"registrygate"}})
		kept := secrets.Key{Namespace: namespace, Name: "kept"}
		dropped := secrets.Key{Namespace: namespace, Name: "dropped"}
		store.SetReferenced([]secrets.Key{kept, dropped})
		requireSecret(t, store, dropped, hasPassword("dropped"))
		requireSecret(t, store, kept, hasPassword("kept"))
		assert.Equal(t, []secrets.Key{dropped, kept}, store.Watched())
		require.Eventually(t, func() bool { return c.watches.count("metadata.name=dropped") == 1 }, 30*time.Second, 50*time.Millisecond)

		store.SetReferenced([]secrets.Key{kept})
		assert.Equal(t, []secrets.Key{kept}, store.Watched())
		_, tracked := store.Get(dropped)
		assert.False(t, tracked)
		require.Eventually(t, func() bool { return c.watches.count("metadata.name=dropped") == 0 }, 30*time.Second, 50*time.Millisecond,
			"the dropped Secret's watch request is still open")
		assert.Equal(t, 1, c.watches.count("metadata.name=kept"))
		requireSecret(t, store, kept, hasPassword("kept"))

		store.SetReferenced(nil)
		assert.Empty(t, store.Watched())
		require.Eventually(t, func() bool { return c.watches.count("metadata.name=kept") == 0 }, 30*time.Second, 50*time.Millisecond)
	})

	t.Run("admin namespace Secrets come from the namespace informer", func(t *testing.T) {
		namespace := "registrygate-admin"
		c.createNamespace(t, namespace)
		c.createSecret(t, namespace, "serving", "first")
		c.grantNamespace(t, namespace)

		store := runStore(t, c.proxy, secrets.Options{AdminNamespaces: []string{namespace}})
		key := secrets.Key{Namespace: namespace, Name: "serving"}
		store.SetReferenced([]secrets.Key{key})
		assert.Empty(t, store.Watched(), "admin namespaces need no watch per Secret")
		requireSecret(t, store, key, hasPassword("first"))

		resourceVersion := c.updateSecret(t, namespace, "serving", "second")
		requireSecret(t, store, key, func(secret secrets.Secret) bool {
			return hasPassword("second")(secret) && secret.ResourceVersion == resourceVersion
		})
		requireSecret(t, store, secrets.Key{Namespace: namespace, Name: "absent"}, inState(secrets.NotFound))
	})

	t.Run("an admin namespace the proxy may not read is forbidden", func(t *testing.T) {
		namespace := "registrygate-unreadable"
		c.createNamespace(t, namespace)

		store := runStore(t, c.proxy, secrets.Options{AdminNamespaces: []string{namespace}})
		key := secrets.Key{Namespace: namespace, Name: "serving"}
		requireSecret(t, store, key, inState(secrets.Forbidden))
	})
}
