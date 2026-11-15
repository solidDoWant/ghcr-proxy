// Package authn turns a ServiceAccount token into the caller's identity by
// asking the Kubernetes API server (TokenReview), and caches the answers.
package authn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/golang-jwt/jwt/v5"
	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/sync/singleflight"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/serviceaccount"
	authenticationv1client "k8s.io/client-go/kubernetes/typed/authentication/v1"
	"k8s.io/client-go/rest"
)

// MaxCacheEntries bounds the review cache. Unique garbage tokens each cost one
// review and can evict entries; the API server's own rate limits and the
// review timeout bound the damage.
const MaxCacheEntries = 10000

// Rate limits of the client used for token reviews.
const (
	clientQPS   = 50
	clientBurst = 100
)

var (
	// ErrRejected means the API server gave a definite no: the token is not
	// authenticated, lacks the audience, or does not belong to a ServiceAccount.
	ErrRejected = errors.New("token rejected")
	// ErrUnavailable means the review could not be made (API server error or
	// timeout). The caller must not treat the request as anonymous.
	ErrUnavailable = errors.New("token review unavailable")
)

// Identity is the ServiceAccount a token belongs to.
type Identity struct {
	Namespace      string
	ServiceAccount string
	// Pod is the pod the token is bound to, empty when the token is not
	// bound to a pod.
	Pod string
}

// TokenReviewCreator is the part of client-go's TokenReview client the
// reviewer uses.
type TokenReviewCreator interface {
	Create(ctx context.Context, review *authenticationv1.TokenReview, opts metav1.CreateOptions) (*authenticationv1.TokenReview, error)
}

// Options configure a Reviewer.
type Options struct {
	// Audience the token must have been issued for.
	Audience string
	// CacheTTL is how long a definite answer is cached, capped by the
	// token's own expiry.
	CacheTTL time.Duration
	// ReviewTimeout bounds one TokenReview call.
	ReviewTimeout time.Duration
	// Now returns the current time; defaults to time.Now.
	Now func() time.Time
}

type cacheEntry struct {
	identity  Identity
	rejected  bool
	expiresAt time.Time
}

// Reviewer reviews ServiceAccount tokens. It is safe for concurrent use.
type Reviewer struct {
	client  TokenReviewCreator
	options Options
	cache   *lru.Cache[[sha256.Size]byte, cacheEntry]
	group   singleflight.Group
}

// NewReviewer returns a Reviewer making reviews through client.
func NewReviewer(client TokenReviewCreator, options Options) (*Reviewer, error) {
	if client == nil {
		return nil, errors.New("authn: nil TokenReview client")
	}
	if options.Audience == "" {
		return nil, errors.New("authn: empty audience")
	}
	if options.CacheTTL <= 0 || options.ReviewTimeout <= 0 {
		return nil, errors.New("authn: cache TTL and review timeout must be positive")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	cache, err := lru.New[[sha256.Size]byte, cacheEntry](MaxCacheEntries)
	if err != nil {
		return nil, fmt.Errorf("authn: creating cache: %w", err)
	}
	return &Reviewer{client: client, options: options, cache: cache}, nil
}

// NewReviewerForConfig returns a Reviewer using a TokenReview client built
// from a copy of config, with the proxy's client rate limits.
func NewReviewerForConfig(config *rest.Config, options Options) (*Reviewer, error) {
	config = rest.CopyConfig(config)
	config.QPS = clientQPS
	config.Burst = clientBurst
	client, err := authenticationv1client.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("authn: creating TokenReview client: %w", err)
	}
	return NewReviewer(client.TokenReviews(), options)
}

type reviewResult struct {
	identity Identity
	err      error
}

// Review returns the identity of the ServiceAccount the token belongs to. It
// returns an error wrapping ErrRejected for a definite no, and one wrapping
// ErrUnavailable when the review could not be made.
func (r *Reviewer) Review(ctx context.Context, token string) (Identity, error) {
	key := sha256.Sum256([]byte(token))
	if entry, ok := r.cache.Get(key); ok {
		if r.options.Now().Before(entry.expiresAt) {
			if entry.rejected {
				return Identity{}, ErrRejected
			}
			return entry.identity, nil
		}
		r.cache.Remove(key)
	}

	// Concurrent reviews of one token share a single call. It runs on a
	// context detached from the client request, so one client giving up
	// does not fail the others.
	resultChannel := r.group.DoChan(hex.EncodeToString(key[:]), func() (any, error) {
		identity, err := r.review(token)
		r.store(key, token, identity, err)
		return reviewResult{identity: identity, err: err}, nil
	})
	select {
	case result := <-resultChannel:
		reviewed := result.Val.(reviewResult)
		return reviewed.identity, reviewed.err
	case <-ctx.Done():
		return Identity{}, fmt.Errorf("%w: %w", ErrUnavailable, ctx.Err())
	}
}

func (r *Reviewer) review(token string) (Identity, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.options.ReviewTimeout)
	defer cancel()
	response, err := r.client.Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{r.options.Audience},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		// The error comes from the API client and never contains the token.
		return Identity{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	status := response.Status
	if !status.Authenticated || !slices.Contains(status.Audiences, r.options.Audience) {
		return Identity{}, ErrRejected
	}
	namespace, name, err := serviceaccount.SplitUsername(status.User.Username)
	if err != nil {
		return Identity{}, ErrRejected
	}
	identity := Identity{Namespace: namespace, ServiceAccount: name}
	if pods := status.User.Extra[serviceaccount.PodNameKey]; len(pods) == 1 {
		identity.Pod = pods[0]
	}
	return identity, nil
}

// store caches definite answers only. The entry is keyed by the token's hash,
// never holds the token, and expires no later than the token itself.
func (r *Reviewer) store(key [sha256.Size]byte, token string, identity Identity, err error) {
	rejected := errors.Is(err, ErrRejected)
	if err != nil && !rejected {
		return
	}
	now := r.options.Now()
	expiresAt := now.Add(r.options.CacheTTL)
	if tokenExpiry, ok := tokenExpiry(token); ok && tokenExpiry.Before(expiresAt) {
		expiresAt = tokenExpiry
	}
	if !now.Before(expiresAt) {
		return
	}
	r.cache.Add(key, cacheEntry{identity: identity, rejected: rejected, expiresAt: expiresAt})
}

// tokenExpiry reads the token's exp claim without verifying the signature.
// It is used only to shorten the cache TTL; the API server did the
// verification.
func tokenExpiry(token string) (time.Time, bool) {
	claims := jwt.RegisteredClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, &claims); err != nil {
		return time.Time{}, false
	}
	if claims.ExpiresAt == nil {
		return time.Time{}, false
	}
	return claims.ExpiresAt.Time, true
}
