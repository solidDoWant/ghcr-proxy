//go:build integration

package controller_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/config"
	"github.com/solidDoWant/registrygate/internal/controller"
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

// defaultGrants is the chart's default grant: each ci-<repo> namespace gets soliddowant/<repo> and everything below.
const defaultGrants = `  - namespaces: "ci-(?P<repo>[a-z0-9-]+)"
    repositories: ["soliddowant/${repo}", "soliddowant/${repo}/**"]
    actions: [pull, push, delete]
`

func loadGrants(t *testing.T, grants string) config.Grants {
	t.Helper()

	pat := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(pat, []byte("x"), 0o600))

	cfg, err := config.Parse([]byte("upstream:\n  patFile: " + pat + "\ngrants:\n" + grants))
	require.NoError(t, err)

	return cfg.Grants
}

// uniqueSuffix makes per-test names, so tests sharing the API server don't see each other's objects.
// A counter keeps them unique when a test runs more than once in one process (-count).
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

func createNamespace(t *testing.T, c client.Client, name string) {
	t.Helper()
	require.NoError(t, c.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}))
}

// startManager runs a manager with the proxy's options and the status controller. The returned function stops it
// and waits until it has stopped; it is also called on cleanup.
func startManager(t *testing.T, userAgent, leaseNamespace string, grants config.Grants) (ctrl.Manager, func()) {
	t.Helper()

	cfg := rest.CopyConfig(restConfig)
	cfg.UserAgent = userAgent

	options := controller.ManagerOptions(scheme, leaseNamespace)
	// Controller names are unique per process; the tests run several managers in one.
	options.Controller.SkipNameValidation = ptr.To(true)

	mgr, err := ctrl.NewManager(cfg, options)
	require.NoError(t, err)

	reconciler := &controller.GHCRAccessPolicyReconciler{Client: mgr.GetClient(), Grants: grants}
	require.NoError(t, reconciler.SetupWithManager(mgr))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)

		assert.NoError(t, mgr.Start(ctx))
	}()

	stop := func() {
		cancel()
		<-done
	}
	t.Cleanup(stop)

	return mgr, stop
}

func createPolicy(t *testing.T, c client.Client, namespace, name string, repositories ...string) *v1alpha1.GHCRAccessPolicy {
	t.Helper()

	policy := &v1alpha1.GHCRAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: v1alpha1.GHCRAccessPolicySpec{
			ServiceAccounts: []string{"builder"},
			Rules: []v1alpha1.GHCRAccessRule{{
				Repositories: repositories,
				Actions:      []v1alpha1.Action{v1alpha1.ActionPush},
			}},
		},
	}
	require.NoError(t, c.Create(t.Context(), policy))

	return policy
}

// readyWith waits until the policy's Ready condition has the status and reason for its current generation, and
// returns the policy as read then.
func readyWith(t *testing.T, c client.Client, key client.ObjectKey, status metav1.ConditionStatus, reason string) *v1alpha1.GHCRAccessPolicy {
	t.Helper()

	var policy v1alpha1.GHCRAccessPolicy

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		require.NoError(collect, c.Get(t.Context(), key, &policy))

		ready := meta.FindStatusCondition(policy.Status.Conditions, controller.ConditionReady)
		require.NotNil(collect, ready)
		assert.Equal(collect, status, ready.Status)
		assert.Equal(collect, reason, ready.Reason)
		assert.Equal(collect, policy.Generation, ready.ObservedGeneration)
		assert.Equal(collect, policy.Generation, policy.Status.ObservedGeneration)
	}, waitFor, tick)

	return &policy
}

func TestStatusController(t *testing.T) {
	c := newClient(t)
	suffix := uniqueSuffix(t)
	leaseNamespace := "lease-" + suffix
	createNamespace(t, c, leaseNamespace)
	// The default grant gives ci-<repo> the repository soliddowant/<repo> and everything below it.
	repo := "foo-" + suffix
	fooNamespace := "ci-" + repo
	createNamespace(t, c, fooNamespace)

	startManager(t, "status-controller", leaseNamespace, loadGrants(t, defaultGrants))

	t.Run("a pattern outside the grant is OutsideGrant and listed in rejected", func(t *testing.T) {
		policy := createPolicy(t, c, fooNamespace, "outside", "soliddowant/bar", "soliddowant/"+repo+"/**")

		got := readyWith(t, c, client.ObjectKeyFromObject(policy), metav1.ConditionFalse, "OutsideGrant")
		assert.Equal(t, []v1alpha1.RejectedRule{{
			Rule:       0,
			Repository: "soliddowant/bar",
			Actions:    []v1alpha1.Action{v1alpha1.ActionPull, v1alpha1.ActionPush},
			Reason:     v1alpha1.RejectedOutsideGrant,
		}}, got.Status.Rejected)
		assert.Equal(t, "1 of 2 repository patterns lose actions outside the namespace's grant; see status.rejected",
			meta.FindStatusCondition(got.Status.Conditions, controller.ConditionReady).Message)
	})

	t.Run("a policy in a namespace no grant matches is NoGrant", func(t *testing.T) {
		namespace := "team-" + suffix
		createNamespace(t, c, namespace)
		policy := createPolicy(t, c, namespace, "nogrant", "soliddowant/foo")

		got := readyWith(t, c, client.ObjectKeyFromObject(policy), metav1.ConditionFalse, "NoGrant")
		assert.Equal(t, []v1alpha1.RejectedRule{{
			Rule:       0,
			Repository: "soliddowant/foo",
			Actions:    []v1alpha1.Action{v1alpha1.ActionPull, v1alpha1.ActionPush},
			Reason:     v1alpha1.RejectedNoGrant,
		}}, got.Status.Rejected)
	})

	t.Run("a policy inside its grant is Accepted for its current generation", func(t *testing.T) {
		policy := createPolicy(t, c, fooNamespace, "inside", "soliddowant/"+repo, "soliddowant/"+repo+"/*")

		got := readyWith(t, c, client.ObjectKeyFromObject(policy), metav1.ConditionTrue, "Accepted")
		assert.Empty(t, got.Status.Rejected)
		assert.Equal(t, int64(1), got.Status.ObservedGeneration)

		// A spec change moves the status to the new generation and clears the old answer.
		got.Spec.Rules[0].Repositories = append(got.Spec.Rules[0].Repositories, "soliddowant/other")
		require.NoError(t, c.Update(t.Context(), got))

		got = readyWith(t, c, client.ObjectKeyFromObject(policy), metav1.ConditionFalse, "OutsideGrant")
		assert.Equal(t, int64(2), got.Status.ObservedGeneration)

		got.Spec.Rules[0].Repositories = got.Spec.Rules[0].Repositories[:2]
		require.NoError(t, c.Update(t.Context(), got))

		got = readyWith(t, c, client.ObjectKeyFromObject(policy), metav1.ConditionTrue, "Accepted")
		assert.Equal(t, int64(3), got.Status.ObservedGeneration)
		assert.Empty(t, got.Status.Rejected)
	})
}

// statusWriters returns the field managers that have written the policy's status.
func statusWriters(policy *v1alpha1.GHCRAccessPolicy) []string {
	var writers []string

	for _, entry := range policy.ManagedFields {
		if entry.Subresource == "status" {
			writers = append(writers, entry.Manager)
		}
	}

	return writers
}

func TestOnlyTheLeaderWritesStatus(t *testing.T) {
	c := newClient(t)
	suffix := uniqueSuffix(t)
	leaseNamespace := "lease-" + suffix
	namespace := "ci-" + suffix

	createNamespace(t, c, leaseNamespace)
	createNamespace(t, c, namespace)

	// The two replicas run different grants, so whichever writes leaves a different answer: the leader accepts the
	// policy, the other replica would say NoGrant.
	leader, stopLeader := startManager(t, "replica-leader", leaseNamespace, loadGrants(t, defaultGrants))

	select {
	case <-leader.Elected():
	case <-time.After(waitFor):
		require.FailNow(t, "the first replica was not elected")
	}

	startManager(t, "replica-standby", leaseNamespace, loadGrants(t, "  []\n"))

	policy := createPolicy(t, c, namespace, "leader", "soliddowant/"+suffix)
	key := client.ObjectKeyFromObject(policy)

	readyWith(t, c, key, metav1.ConditionTrue, "Accepted")

	require.Never(t, func() bool {
		var current v1alpha1.GHCRAccessPolicy
		require.NoError(t, c.Get(t.Context(), key, &current))

		ready := meta.FindStatusCondition(current.Status.Conditions, controller.ConditionReady)

		return ready == nil || ready.Reason != "Accepted" || len(statusWriters(&current)) != 1
	}, 3*time.Second, tick)

	got := readyWith(t, c, key, metav1.ConditionTrue, "Accepted")
	assert.Equal(t, []string{"replica-leader"}, statusWriters(got))

	// When the leader stops it releases the lease, and the standby takes over and writes status from its own grants.
	stopLeader()

	readyWith(t, c, key, metav1.ConditionFalse, "NoGrant")
}
