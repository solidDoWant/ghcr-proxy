//go:build integration

package controller_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/controller"
	"github.com/solidDoWant/registrygate/internal/model"
	"github.com/solidDoWant/registrygate/internal/secrets"
)

const (
	waitFor = 30 * time.Second
	tick    = 100 * time.Millisecond
)

var (
	runs       atomic.Int64
	restConfig *rest.Config
	scheme     = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("skipping: KUBEBUILDER_ASSETS is not set (run from the dev shell)")
		os.Exit(0)
	}

	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}

	if err := v1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	var err error

	restConfig, err = env.Start()
	if err != nil {
		panic(err)
	}

	code := m.Run()

	if err := env.Stop(); err != nil {
		fmt.Println("stopping envtest:", err)
	}

	os.Exit(code)
}

// uniqueSuffix makes per-test names, so tests sharing the API server don't see each other's objects.
func uniqueSuffix(t *testing.T) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%d", t.Name(), runs.Add(1)))
	return hex.EncodeToString(sum[:])[:8]
}

func newClient(t *testing.T) client.Client {
	t.Helper()

	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	require.NoError(t, err)

	return c
}

func create(t *testing.T, c client.Client, object client.Object) {
	t.Helper()
	require.NoError(t, c.Create(t.Context(), object))
}

func createNamespace(t *testing.T, c client.Client, name string) {
	t.Helper()
	create(t, c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
}

func keyPair(t *testing.T, host string) map[string][]byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	return map[string][]byte{
		"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		"tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}
}

// startStatus runs a manager with the status controller, and a model and Secret store feeding it, the way the
// proxy wires them. Everything stops on cleanup.
func startStatus(t *testing.T, userAgent, admin, leaseNamespace string) (ctrl.Manager, func()) {
	t.Helper()

	options := controller.ManagerOptions(scheme, leaseNamespace)
	options.Controller.SkipNameValidation = ptr.To(true)

	config := rest.CopyConfig(restConfig)
	config.UserAgent = userAgent

	mgr, err := ctrl.NewManager(config, options)
	require.NoError(t, err)

	status := &controller.Status{}

	var instance *model.Model

	kube, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)

	store := secrets.New(kube, secrets.Options{
		AdminNamespaces: []string{admin},
		OnChange:        func() { instance.MarkDirty() },
	})

	instance = model.New(model.Options{
		Reader:          mgr.GetCache(),
		Informers:       mgr.GetCache(),
		Secrets:         store,
		AdminNamespaces: []string{admin},
		OnRebuild:       status.Notify,
	})
	status.Snapshot = instance.Snapshot

	require.NoError(t, status.SetupWithManager(mgr))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 3)

	for _, run := range []func(context.Context) error{mgr.Start, store.Run, instance.Run} {
		go func() {
			defer func() { done <- struct{}{} }()

			assert.NoError(t, run(ctx))
		}()
	}

	var stopOnce sync.Once

	stop := func() {
		stopOnce.Do(func() {
			cancel()

			for range 3 {
				<-done
			}
		})
	}
	t.Cleanup(stop)

	return mgr, stop
}

func getReady(t *testing.T, c client.Client, object client.Object, conditions func() []metav1.Condition,
	status metav1.ConditionStatus, reason string,
) {
	t.Helper()

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		require.NoError(collect, c.Get(t.Context(), client.ObjectKeyFromObject(object), object))

		ready := meta.FindStatusCondition(conditions(), v1alpha1.ConditionReady)
		require.NotNil(collect, ready)
		assert.Equal(collect, status, ready.Status)
		assert.Equal(collect, reason, ready.Reason)
		assert.Equal(collect, object.GetGeneration(), ready.ObservedGeneration)
	}, waitFor, tick)
}

type world struct {
	c          client.Client
	admin      string
	tenant     string
	upstream   *v1alpha1.UpstreamRegistry
	credential *v1alpha1.UpstreamCredential
	mirror     *v1alpha1.RegistryMirror
	policy     *v1alpha1.RegistryAccessPolicy
	lease      string
	stop       func()
}

func newWorld(t *testing.T) *world {
	t.Helper()

	c := newClient(t)
	suffix := uniqueSuffix(t)
	w := &world{c: c, admin: "admin-" + suffix, tenant: "tenant-" + suffix}

	for _, namespace := range []string{w.admin, w.tenant, "lease-" + suffix} {
		createNamespace(t, c, namespace)
	}

	host := "ghcr-" + suffix + ".example"
	create(t, c, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: w.admin, Name: "tls"},
		Data:       keyPair(t, host),
	})
	create(t, c, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: w.admin, Name: "creds"},
		Data:       map[string][]byte{"username": []byte("robot"), "password": []byte("hunter2")},
	})

	w.upstream = &v1alpha1.UpstreamRegistry{
		ObjectMeta: metav1.ObjectMeta{Namespace: w.admin, Name: "ghcr"},
		Spec: v1alpha1.UpstreamRegistrySpec{
			Hosts: []string{host},
			URL:   "https://upstream.example",
			TLS:   v1alpha1.ServingTLS{SecretRef: v1alpha1.LocalRef{Name: "tls"}},
			AllowedNamespaces: v1alpha1.AllowedNamespaces{
				Names: []string{w.tenant},
			},
			AllowedMirrorURLs: []string{"https://harbor.example/base"},
		},
	}
	w.credential = &v1alpha1.UpstreamCredential{
		ObjectMeta: metav1.ObjectMeta{Namespace: w.admin, Name: "push"},
		Spec: v1alpha1.UpstreamCredentialSpec{
			UpstreamRef: v1alpha1.ObjectRef{Name: "ghcr"},
			SecretRef:   v1alpha1.CredentialsSecretRef{Name: "creds"},
			Ceiling: []v1alpha1.AccessGrant{{
				Repositories: []string{"org/**"},
				Actions:      []v1alpha1.Action{v1alpha1.ActionPush},
			}},
			AllowedNamespaces: v1alpha1.AllowedNamespaces{Names: []string{w.tenant}},
		},
	}
	w.mirror = &v1alpha1.RegistryMirror{
		ObjectMeta: metav1.ObjectMeta{Namespace: w.tenant, Name: "harbor"},
		Spec: v1alpha1.RegistryMirrorSpec{
			UpstreamRef: v1alpha1.ObjectRef{Namespace: w.admin, Name: "ghcr"},
			URL:         "https://harbor.example/base/ci",
			PathPrefix:  "ci-ghcr",
		},
	}
	w.policy = &v1alpha1.RegistryAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: w.tenant, Name: "builds"},
		Spec: v1alpha1.RegistryAccessPolicySpec{
			ServiceAccounts: []string{"builder"},
			Rules: []v1alpha1.AccessRule{
				{
					Name:          "push",
					CredentialRef: &v1alpha1.ObjectRef{Namespace: w.admin, Name: "push"},
					Repositories:  []string{"org/app"},
					Actions:       []v1alpha1.Action{v1alpha1.ActionPush},
				},
				{
					Name:          "too-much",
					CredentialRef: &v1alpha1.ObjectRef{Namespace: w.admin, Name: "push"},
					Repositories:  []string{"other/app"},
					Actions:       []v1alpha1.Action{v1alpha1.ActionPush},
				},
				{
					Name:         "pull",
					MirrorRef:    &v1alpha1.ObjectRef{Name: "harbor"},
					Repositories: []string{"**"},
					Actions:      []v1alpha1.Action{v1alpha1.ActionPull},
				},
			},
		},
	}

	for _, object := range []client.Object{w.upstream, w.credential, w.mirror, w.policy} {
		create(t, c, object)
	}

	w.lease = "lease-" + suffix
	_, w.stop = startStatus(t, "replica-leader", w.admin, w.lease)

	return w
}

func accepted(policy *v1alpha1.RegistryAccessPolicy, rule string) *metav1.Condition {
	for _, status := range policy.Status.Rules {
		if status.Name == rule {
			return meta.FindStatusCondition(status.Conditions, v1alpha1.ConditionAccepted)
		}
	}

	return nil
}

func TestStatusShowsEachReadinessOutcome(t *testing.T) {
	w := newWorld(t)

	getReady(t, w.c, w.upstream, func() []metav1.Condition { return w.upstream.Status.Conditions },
		metav1.ConditionTrue, v1alpha1.ReasonReady)
	assert.Equal(t, int64(1), w.upstream.Status.ObservedGeneration)

	getReady(t, w.c, w.credential, func() []metav1.Condition { return w.credential.Status.Conditions },
		metav1.ConditionTrue, v1alpha1.ReasonReady)
	getReady(t, w.c, w.mirror, func() []metav1.Condition { return w.mirror.Status.Conditions },
		metav1.ConditionTrue, v1alpha1.ReasonReady)

	getReady(t, w.c, w.policy, func() []metav1.Condition { return w.policy.Status.Conditions },
		metav1.ConditionFalse, v1alpha1.ReasonRulesNotAccepted)
	assert.Equal(t, int64(1), w.policy.Status.ObservedGeneration)
	assert.Contains(t, meta.FindStatusCondition(w.policy.Status.Conditions, v1alpha1.ConditionReady).Message, "too-much")

	require.Len(t, w.policy.Status.Rules, 3)
	require.NotNil(t, accepted(w.policy, "push"))
	assert.Equal(t, metav1.ConditionTrue, accepted(w.policy, "push").Status)
	assert.Equal(t, v1alpha1.ReasonAccepted, accepted(w.policy, "push").Reason)
	assert.Equal(t, metav1.ConditionFalse, accepted(w.policy, "too-much").Status)
	assert.Equal(t, v1alpha1.ReasonOutsideCeiling, accepted(w.policy, "too-much").Reason)
	assert.Equal(t, metav1.ConditionTrue, accepted(w.policy, "pull").Status)

	// A spec change moves the status to the new generation.
	w.mirror.Spec.URL = "https://elsewhere.example/ci"
	require.NoError(t, w.c.Update(t.Context(), w.mirror))
	getReady(t, w.c, w.mirror, func() []metav1.Condition { return w.mirror.Status.Conditions },
		metav1.ConditionFalse, v1alpha1.ReasonURLNotAllowed)
	assert.Equal(t, int64(2), w.mirror.Status.ObservedGeneration)
}

func TestStatusIsNotRewrittenWhenNothingChanged(t *testing.T) {
	w := newWorld(t)

	getReady(t, w.c, w.policy, func() []metav1.Condition { return w.policy.Status.Conditions },
		metav1.ConditionFalse, v1alpha1.ReasonRulesNotAccepted)
	getReady(t, w.c, w.upstream, func() []metav1.Condition { return w.upstream.Status.Conditions },
		metav1.ConditionTrue, v1alpha1.ReasonReady)

	policyVersion := w.policy.ResourceVersion
	upstreamVersion := w.upstream.ResourceVersion
	transition := meta.FindStatusCondition(w.upstream.Status.Conditions, v1alpha1.ConditionReady).LastTransitionTime

	// Wait until a rewrite would show: condition times have one-second resolution.
	require.Eventually(t, func() bool { return time.Since(transition.Time) > 1500*time.Millisecond }, waitFor, tick)

	// Unrelated changes make the model rebuild the snapshot several times.
	for index := range 3 {
		create(t, w.c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("noise-%s-%d", w.tenant, index),
		}})
	}

	policyKey, upstreamKey := client.ObjectKeyFromObject(w.policy), client.ObjectKeyFromObject(w.upstream)

	require.Never(t, func() bool {
		var (
			policy   v1alpha1.RegistryAccessPolicy
			upstream v1alpha1.UpstreamRegistry
		)

		if w.c.Get(t.Context(), policyKey, &policy) != nil || w.c.Get(t.Context(), upstreamKey, &upstream) != nil {
			return false
		}

		return policy.ResourceVersion != policyVersion || upstream.ResourceVersion != upstreamVersion
	}, 3*time.Second, tick)

	require.NoError(t, w.c.Get(t.Context(), client.ObjectKeyFromObject(w.upstream), w.upstream))
	assert.True(t, transition.Equal(&meta.FindStatusCondition(w.upstream.Status.Conditions, v1alpha1.ConditionReady).LastTransitionTime))
}

func TestStatusFollowsASecretChange(t *testing.T) {
	w := newWorld(t)

	getReady(t, w.c, w.credential, func() []metav1.Condition { return w.credential.Status.Conditions },
		metav1.ConditionTrue, v1alpha1.ReasonReady)

	var secret corev1.Secret

	secretKey := client.ObjectKey{Namespace: w.admin, Name: "creds"}
	require.NoError(t, w.c.Get(t.Context(), secretKey, &secret))

	secret.Data = map[string][]byte{"username": []byte("robot")}
	require.NoError(t, w.c.Update(t.Context(), &secret))

	getReady(t, w.c, w.credential, func() []metav1.Condition { return w.credential.Status.Conditions },
		metav1.ConditionFalse, v1alpha1.ReasonSecretInvalid)

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		require.NoError(collect, w.c.Get(t.Context(), client.ObjectKeyFromObject(w.policy), w.policy))

		rule := accepted(w.policy, "push")
		require.NotNil(collect, rule)
		assert.Equal(collect, v1alpha1.ReasonRefNotReady, rule.Reason)
	}, waitFor, tick)

	secret.Data = map[string][]byte{"username": []byte("robot"), "password": []byte("hunter3")}
	require.NoError(t, w.c.Update(t.Context(), &secret))

	getReady(t, w.c, w.credential, func() []metav1.Condition { return w.credential.Status.Conditions },
		metav1.ConditionTrue, v1alpha1.ReasonReady)
}

// statusWriters returns the field managers that have written the credential's status.
func statusWriters(credential *v1alpha1.UpstreamCredential) []string {
	var writers []string

	for _, entry := range credential.ManagedFields {
		if entry.Subresource == "status" {
			writers = append(writers, entry.Manager)
		}
	}

	return writers
}

func TestOnlyTheLeaderWritesStatus(t *testing.T) {
	w := newWorld(t)

	getReady(t, w.c, w.credential, func() []metav1.Condition { return w.credential.Status.Conditions },
		metav1.ConditionTrue, v1alpha1.ReasonReady)

	// A standby replica sees the same objects but must not write.
	startStatus(t, "replica-standby", w.admin, w.lease)

	var secret corev1.Secret

	require.NoError(t, w.c.Get(t.Context(), client.ObjectKey{Namespace: w.admin, Name: "creds"}, &secret))
	secret.Data = map[string][]byte{"username": []byte("robot")}
	require.NoError(t, w.c.Update(t.Context(), &secret))

	getReady(t, w.c, w.credential, func() []metav1.Condition { return w.credential.Status.Conditions },
		metav1.ConditionFalse, v1alpha1.ReasonSecretInvalid)
	assert.Equal(t, []string{"replica-leader"}, statusWriters(w.credential))

	// When the leader stops it releases the lease and the standby takes over.
	w.stop()

	secret.Data = map[string][]byte{"username": []byte("robot"), "password": []byte("hunter3")}
	require.NoError(t, w.c.Update(t.Context(), &secret))

	getReady(t, w.c, w.credential, func() []metav1.Condition { return w.credential.Status.Conditions },
		metav1.ConditionTrue, v1alpha1.ReasonReady)
	assert.Contains(t, statusWriters(w.credential), "replica-standby")
}
