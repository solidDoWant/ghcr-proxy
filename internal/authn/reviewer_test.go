package authn

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/registrygate/internal/metrics"
)

const testAudience = "registrygate"

// fakeTokenReviews stands in for the API server's TokenReview endpoint.
type fakeTokenReviews struct {
	calls  atomic.Int32
	answer func(ctx context.Context, review *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error)
}

func (f *fakeTokenReviews) Create(ctx context.Context, review *authenticationv1.TokenReview, _ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
	f.calls.Add(1)
	return f.answer(ctx, review)
}

func authenticated(username string, audiences []string, extra map[string]authenticationv1.ExtraValue) func(context.Context, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
	return func(_ context.Context, review *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
		answer := review.DeepCopy()
		answer.Status = authenticationv1.TokenReviewStatus{
			Authenticated: true,
			Audiences:     audiences,
			User:          authenticationv1.UserInfo{Username: username, Extra: extra},
		}

		return answer, nil
	}
}

type fakeClock struct {
	mutex sync.Mutex
	now   time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	return c.now
}

func (c *fakeClock) Advance(duration time.Duration) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.now = c.now.Add(duration)
}

func newTestReviewer(t *testing.T, client TokenReviewCreator, clock *fakeClock) *Reviewer {
	t.Helper()

	reviewer, err := NewReviewer(client, Options{
		Audience:      testAudience,
		CacheTTL:      30 * time.Second,
		ReviewTimeout: 5 * time.Second,
		Now:           clock.Now,
	})
	require.NoError(t, err)

	return reviewer
}

// signedToken returns a JWT expiring at expiresAt. Only its exp claim matters:
// the fake API server does not check signatures.
func signedToken(t *testing.T, expiresAt time.Time) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		ExpiresAt: jwt.NewNumericDate(expiresAt),
		Subject:   t.Name(),
	}).SignedString([]byte("test signing key"))
	require.NoError(t, err)

	return token
}

func TestReviewReturnsServiceAccountIdentity(t *testing.T) {
	var sentSpec authenticationv1.TokenReviewSpec

	answer := authenticated("system:serviceaccount:ci-app:builder", []string{testAudience},
		map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-name": {"build-1"}})
	client := &fakeTokenReviews{answer: func(ctx context.Context, review *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
		sentSpec = review.Spec
		return answer(ctx, review)
	}}
	reviewer := newTestReviewer(t, client, newFakeClock())

	identity, err := reviewer.Review(t.Context(), "the-token")
	require.NoError(t, err)
	assert.Equal(t, Identity{Namespace: "ci-app", ServiceAccount: "builder", Pod: "build-1"}, identity)
	assert.Equal(t, "the-token", sentSpec.Token)
	assert.Equal(t, []string{testAudience}, sentSpec.Audiences)
}

func TestReviewRejectsTokensThatAreNotServiceAccountTokensForTheAudience(t *testing.T) {
	tests := []struct {
		name   string
		answer func(context.Context, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error)
	}{
		{
			name: "not authenticated",
			answer: func(_ context.Context, review *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
				answer := review.DeepCopy()
				answer.Status = authenticationv1.TokenReviewStatus{Authenticated: false, Error: "invalid bearer token"}

				return answer, nil
			},
		},
		{
			name:   "audience missing from the result",
			answer: authenticated("system:serviceaccount:ci-app:builder", []string{"https://kubernetes.default.svc"}, nil),
		},
		{
			name:   "no audiences in the result",
			answer: authenticated("system:serviceaccount:ci-app:builder", nil, nil),
		},
		{
			name:   "user is not a ServiceAccount",
			answer: authenticated("alice", []string{testAudience}, nil),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeTokenReviews{answer: test.answer}
			reviewer := newTestReviewer(t, client, newFakeClock())

			identity, err := reviewer.Review(t.Context(), "the-token")
			require.ErrorIs(t, err, ErrRejected)
			assert.NotErrorIs(t, err, ErrUnavailable)
			assert.Equal(t, Identity{}, identity)
		})
	}
}

func TestReviewIsUnavailableWhenTheAPIServerFails(t *testing.T) {
	tests := []struct {
		name          string
		reviewTimeout time.Duration
		answer        func(context.Context, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error)
	}{
		{
			name:          "review error",
			reviewTimeout: 5 * time.Second,
			answer: func(context.Context, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
				return nil, errors.New("connection refused")
			},
		},
		{
			name:          "review timeout",
			reviewTimeout: 10 * time.Millisecond,
			answer: func(ctx context.Context, _ *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeTokenReviews{answer: test.answer}
			reviewer, err := NewReviewer(client, Options{
				Audience:      testAudience,
				CacheTTL:      30 * time.Second,
				ReviewTimeout: test.reviewTimeout,
				Now:           newFakeClock().Now,
			})
			require.NoError(t, err)

			_, err = reviewer.Review(t.Context(), "the-token")
			require.ErrorIs(t, err, ErrUnavailable)
			assert.NotErrorIs(t, err, ErrRejected)

			// Failures are never cached: the next request asks again.
			_, err = reviewer.Review(t.Context(), "the-token")
			require.ErrorIs(t, err, ErrUnavailable)
			assert.Equal(t, int32(2), client.calls.Load())
		})
	}
}

func TestConcurrentReviewsOfOneTokenMakeOneReview(t *testing.T) {
	const callers = 100

	release := make(chan struct{})
	answer := authenticated("system:serviceaccount:ci-app:builder", []string{testAudience}, nil)
	client := &fakeTokenReviews{answer: func(ctx context.Context, review *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
		<-release
		return answer(ctx, review)
	}}
	reviewer := newTestReviewer(t, client, newFakeClock())

	var started, finished sync.WaitGroup
	started.Add(callers)
	finished.Add(callers)
	identities := make([]Identity, callers)

	errs := make([]error, callers)
	for caller := range callers {
		go func() {
			defer finished.Done()

			started.Done()

			identities[caller], errs[caller] = reviewer.Review(t.Context(), "the-token")
		}()
	}

	started.Wait()
	close(release)
	finished.Wait()

	for caller := range callers {
		require.NoError(t, errs[caller])
		assert.Equal(t, Identity{Namespace: "ci-app", ServiceAccount: "builder"}, identities[caller])
	}

	assert.Equal(t, int32(1), client.calls.Load())
}

func TestCallerGivingUpDoesNotFailOtherCallersOfTheSameReview(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	answer := authenticated("system:serviceaccount:ci-app:builder", []string{testAudience}, nil)
	client := &fakeTokenReviews{answer: func(ctx context.Context, review *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
		close(entered)
		<-release

		if err := ctx.Err(); err != nil {
			return nil, err
		}

		return answer(ctx, review)
	}}
	reviewer := newTestReviewer(t, client, newFakeClock())

	impatient, cancel := context.WithCancel(t.Context())
	impatientResult := make(chan error, 1)

	go func() {
		_, err := reviewer.Review(impatient, "the-token")
		impatientResult <- err
	}()

	<-entered

	patientResult := make(chan error, 1)

	go func() {
		_, err := reviewer.Review(t.Context(), "the-token")
		patientResult <- err
	}()

	cancel()
	require.ErrorIs(t, <-impatientResult, ErrUnavailable)
	close(release)
	require.NoError(t, <-patientResult)
	assert.Equal(t, int32(1), client.calls.Load())
}

func TestDefiniteAnswersAreCachedForTheTTL(t *testing.T) {
	tests := []struct {
		name      string
		answer    func(context.Context, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error)
		assertErr require.ErrorAssertionFunc
	}{
		{
			name:      "authenticated",
			answer:    authenticated("system:serviceaccount:ci-app:builder", []string{testAudience}, nil),
			assertErr: require.NoError,
		},
		{
			name:   "rejected",
			answer: authenticated("system:serviceaccount:ci-app:builder", []string{"other"}, nil),
			assertErr: func(t require.TestingT, err error, _ ...any) {
				require.ErrorIs(t, err, ErrRejected)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeTokenReviews{answer: test.answer}
			clock := newFakeClock()
			reviewer := newTestReviewer(t, client, clock)

			_, err := reviewer.Review(t.Context(), "the-token")
			test.assertErr(t, err)
			clock.Advance(30*time.Second - time.Nanosecond)

			_, err = reviewer.Review(t.Context(), "the-token")
			test.assertErr(t, err)
			assert.Equal(t, int32(1), client.calls.Load(), "a cached answer within the TTL")

			clock.Advance(time.Nanosecond)

			_, err = reviewer.Review(t.Context(), "the-token")
			test.assertErr(t, err)
			assert.Equal(t, int32(2), client.calls.Load(), "a new review once the TTL has passed")
		})
	}
}

func TestCacheEntryNeverOutlivesTheTokenExpiry(t *testing.T) {
	clock := newFakeClock()
	client := &fakeTokenReviews{answer: authenticated("system:serviceaccount:ci-app:builder", []string{testAudience}, nil)}
	reviewer := newTestReviewer(t, client, clock)
	token := signedToken(t, clock.Now().Add(5*time.Second))

	_, err := reviewer.Review(t.Context(), token)
	require.NoError(t, err)
	clock.Advance(4 * time.Second)

	_, err = reviewer.Review(t.Context(), token)
	require.NoError(t, err)
	assert.Equal(t, int32(1), client.calls.Load(), "a cached answer before the token expires")

	clock.Advance(time.Second)

	_, err = reviewer.Review(t.Context(), token)
	require.NoError(t, err)
	assert.Equal(t, int32(2), client.calls.Load(), "a new review once the token has expired")
}

func TestAnswerForAnExpiredTokenIsNotCached(t *testing.T) {
	clock := newFakeClock()
	client := &fakeTokenReviews{answer: authenticated("system:serviceaccount:ci-app:builder", []string{testAudience}, nil)}
	reviewer := newTestReviewer(t, client, clock)
	token := signedToken(t, clock.Now().Add(-time.Second))

	for range 2 {
		_, err := reviewer.Review(t.Context(), token)
		require.NoError(t, err)
	}

	assert.Equal(t, int32(2), client.calls.Load())
}

func TestCacheNeverStoresTheToken(t *testing.T) {
	clock := newFakeClock()
	client := &fakeTokenReviews{answer: authenticated("system:serviceaccount:ci-app:builder", []string{testAudience}, nil)}
	reviewer := newTestReviewer(t, client, clock)
	token := signedToken(t, clock.Now().Add(time.Hour))

	_, err := reviewer.Review(t.Context(), token)
	require.NoError(t, err)

	require.Equal(t, 1, reviewer.cache.Len())
	assert.Equal(t, [][sha256.Size]byte{sha256.Sum256([]byte(token))}, reviewer.cache.Keys())

	for _, value := range reviewer.cache.Values() {
		dump := fmt.Sprintf("%#v", value)
		assert.NotContains(t, dump, token)

		for _, part := range strings.Split(token, ".") {
			assert.NotContains(t, dump, part)
		}
	}
}

func TestReviewsCountResultAndCacheUse(t *testing.T) {
	proxyMetrics := metrics.New()
	clock := newFakeClock()
	client := &fakeTokenReviews{answer: authenticated("system:serviceaccount:ci-app:builder", []string{testAudience}, nil)}

	reviewer, err := NewReviewer(client, Options{
		Audience: testAudience, CacheTTL: 30 * time.Second, ReviewTimeout: 5 * time.Second, Now: clock.Now, Metrics: proxyMetrics,
	})
	require.NoError(t, err)

	_, err = reviewer.Review(t.Context(), "good-token")
	require.NoError(t, err)
	_, err = reviewer.Review(t.Context(), "good-token")
	require.NoError(t, err)

	client.answer = authenticated("system:serviceaccount:ci-app:builder", []string{"other"}, nil)
	_, err = reviewer.Review(t.Context(), "bad-token")
	require.ErrorIs(t, err, ErrRejected)

	client.answer = func(context.Context, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
		return nil, errors.New("api server down")
	}
	_, err = reviewer.Review(t.Context(), "unlucky-token")
	require.ErrorIs(t, err, ErrUnavailable)

	require.NoError(t, testutil.GatherAndCompare(proxyMetrics.Gatherer(), strings.NewReader(`
# HELP registrygate_token_reviews_total ServiceAccount token reviews, by result and cache use.
# TYPE registrygate_token_reviews_total counter
registrygate_token_reviews_total{cache="hit",result="authenticated"} 1
registrygate_token_reviews_total{cache="miss",result="authenticated"} 1
registrygate_token_reviews_total{cache="miss",result="error"} 1
registrygate_token_reviews_total{cache="miss",result="rejected"} 1
`), "registrygate_token_reviews_total"))
}
