//go:build integration

package authn_test

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/solidDoWant/registrygate/internal/authn"
)

const (
	proxyAudience = "registrygate"
	// The pod is never scheduled in envtest; the image is never pulled.
	unusedPodImage = "ghcr.io/soliddowant/unused@sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

type mutableClock struct{ now time.Time }

func (c *mutableClock) Now() time.Time { return c.now }

// startAPIServer starts envtest's API server, which serves TokenRequest and
// TokenReview with real signed ServiceAccount tokens.
func startAPIServer(t *testing.T) kubernetes.Interface {
	t.Helper()

	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run inside the dev shell")
	}

	environment := &envtest.Environment{}
	config, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	client, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)

	return client
}

// podToken creates a ServiceAccount and a pod in a new namespace and returns
// a token for audience bound to that pod.
func podToken(t *testing.T, client kubernetes.Interface, namespace, audience string) string {
	t.Helper()
	ctx := t.Context()
	_, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = client.CoreV1().ServiceAccounts(namespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "builder"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	pod, err := client.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "build"},
		Spec: corev1.PodSpec{
			ServiceAccountName: "builder",
			Containers:         []corev1.Container{{Name: "build", Image: unusedPodImage}},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	request, err := client.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, "builder", &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: ptr.To[int64](3600),
			BoundObjectRef:    &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	return request.Status.Token
}

func newReviewer(t *testing.T, client kubernetes.Interface, clock *mutableClock) *authn.Reviewer {
	t.Helper()

	reviewer, err := authn.NewReviewer(client.AuthenticationV1().TokenReviews(), authn.Options{
		Audience:      proxyAudience,
		CacheTTL:      30 * time.Second,
		ReviewTimeout: 10 * time.Second,
		Now:           clock.Now,
	})
	require.NoError(t, err)

	return reviewer
}

func TestReviewAgainstTheAPIServer(t *testing.T) {
	client := startAPIServer(t)

	t.Run("token for the proxy audience gives the ServiceAccount identity", func(t *testing.T) {
		token := podToken(t, client, "ci-audience-ok", proxyAudience)
		reviewer := newReviewer(t, client, &mutableClock{now: time.Now()})

		identity, err := reviewer.Review(t.Context(), token)
		require.NoError(t, err)
		assert.Equal(t, authn.Identity{Namespace: "ci-audience-ok", ServiceAccount: "builder", Pod: "build"}, identity)
	})

	t.Run("token for another audience is rejected", func(t *testing.T) {
		token := podToken(t, client, "ci-audience-other", "https://kubernetes.default.svc")
		reviewer := newReviewer(t, client, &mutableClock{now: time.Now()})

		_, err := reviewer.Review(t.Context(), token)
		require.ErrorIs(t, err, authn.ErrRejected)
	})

	t.Run("garbage token is rejected", func(t *testing.T) {
		reviewer := newReviewer(t, client, &mutableClock{now: time.Now()})

		_, err := reviewer.Review(t.Context(), strings.Repeat("x", 40))
		require.ErrorIs(t, err, authn.ErrRejected)
	})

	t.Run("token bound to a deleted pod fails once the cached answer expires", func(t *testing.T) {
		token := podToken(t, client, "ci-deleted-pod", proxyAudience)
		clock := &mutableClock{now: time.Now()}
		reviewer := newReviewer(t, client, clock)

		_, err := reviewer.Review(t.Context(), token)
		require.NoError(t, err)
		require.NoError(t, client.CoreV1().Pods("ci-deleted-pod").Delete(t.Context(), "build", metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}))
		require.Eventually(t, func() bool {
			_, err := client.CoreV1().Pods("ci-deleted-pod").Get(t.Context(), "build", metav1.GetOptions{})
			return err != nil
		}, 30*time.Second, 100*time.Millisecond)

		_, err = reviewer.Review(t.Context(), token)
		require.NoError(t, err, "the cached answer holds until the cache TTL")

		// The API server keeps its own short cache of successful token
		// authentications, so the review fails only once that has lapsed too.
		// Each attempt moves the proxy's clock past its cache TTL.
		require.Eventually(t, func() bool {
			clock.now = clock.now.Add(30 * time.Second)
			_, err := reviewer.Review(t.Context(), token)

			return errors.Is(err, authn.ErrRejected)
		}, time.Minute, 500*time.Millisecond)
	})
}
