package server_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/registrygate/internal/authn"
	"github.com/solidDoWant/registrygate/internal/metrics"
	"github.com/solidDoWant/registrygate/internal/secrets"
	"github.com/solidDoWant/registrygate/internal/server"
	"github.com/solidDoWant/registrygate/test/fakeregistry"
)

const (
	builderToken     = "builder-token"
	rejectedToken    = "rejected-token"
	unavailableToken = "unavailable-token"
)

var builder = authn.Identity{Namespace: "ci-foo", ServiceAccount: "builder", Pod: "build-1"}

// stubReviewer stands in for TokenReview: one token belongs to builder, one is rejected, one can't be reviewed.
type stubReviewer struct{}

func (stubReviewer) Review(_ context.Context, token string) (authn.Identity, error) {
	switch token {
	case builderToken:
		return builder, nil
	case unavailableToken:
		return authn.Identity{}, authn.ErrUnavailable
	}

	return authn.Identity{}, authn.ErrRejected
}

// basic is an Authorization value carrying token as the Basic password, as the credential helper's answer does.
func basic(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("registrygate:"+token))
}

type answer struct {
	status int
	header http.Header
	code   string
	body   string
}

// do sends one request to the proxy; authorization values are sent as separate Authorization headers.
func (h *harness) do(t *testing.T, method, target string, authorization ...string) answer {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, h.url()+target, nil)
	require.NoError(t, err)

	for _, value := range authorization {
		req.Header.Add("Authorization", value)
	}

	resp, err := h.client().Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var parsed struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}

	out := answer{status: resp.StatusCode, header: resp.Header, body: string(body)}
	if json.Unmarshal(body, &parsed) == nil && len(parsed.Errors) == 1 {
		out.code = parsed.Errors[0].Code
		out.body = parsed.Errors[0].Message
	}

	return out
}

func TestVersionCheckChallengesAnonymousCallersAndAnswersServiceAccountsLocally(t *testing.T) {
	proxy := newHarness(t, harnessOptions{})

	anonymous := proxy.do(t, http.MethodGet, "/v2/")
	assert.Equal(t, http.StatusUnauthorized, anonymous.status)
	assert.Equal(t, `Basic realm="registrygate"`, anonymous.header.Get("WWW-Authenticate"))
	assert.Equal(t, "UNAUTHORIZED", anonymous.code)

	authenticated := proxy.do(t, http.MethodGet, "/v2/", basic(builderToken))
	assert.Equal(t, http.StatusOK, authenticated.status)
	assert.JSONEq(t, `{}`, authenticated.body)
	assert.Empty(t, authenticated.header.Get("WWW-Authenticate"))

	assert.Empty(t, proxy.fake.Log(), "the version check never reaches the upstream")
}

func TestAuthenticationRefusesCredentialsItCannotAccept(t *testing.T) {
	proxy := newHarness(t, harnessOptions{})

	tests := []struct {
		name          string
		authorization []string
		status        int
		code          string
	}{
		{name: "bearer scheme", authorization: []string{"Bearer " + builderToken}, status: http.StatusUnauthorized, code: "UNAUTHORIZED"},
		{name: "bad base64", authorization: []string{"Basic !!!not-base64"}, status: http.StatusUnauthorized, code: "UNAUTHORIZED"},
		{name: "empty password", authorization: []string{basic("")}, status: http.StatusUnauthorized, code: "UNAUTHORIZED"},
		{
			name: "two Authorization headers", authorization: []string{basic(builderToken), basic(builderToken)},
			status: http.StatusUnauthorized, code: "UNAUTHORIZED",
		},
		{name: "token rejected by review", authorization: []string{basic(rejectedToken)}, status: http.StatusUnauthorized, code: "UNAUTHORIZED"},
		{name: "token review unavailable", authorization: []string{basic(unavailableToken)}, status: http.StatusServiceUnavailable, code: "UNAVAILABLE"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy.fake.ResetLog()

			got := proxy.do(t, http.MethodGet, "/v2/soliddowant/foo/source/manifests/latest", test.authorization...)

			assert.Equal(t, test.status, got.status)
			assert.Equal(t, test.code, got.code)

			if test.status == http.StatusUnauthorized {
				assert.Equal(t, `Basic realm="registrygate"`, got.header.Get("WWW-Authenticate"))
			} else {
				assert.Equal(t, "5", got.header.Get("Retry-After"))
			}

			assert.Empty(t, proxy.fake.Log(), "a refused credential is never served anonymously")
		})
	}
}

// tokenReviews answers TokenReview calls the way an API server would for the tokens it knows.
type tokenReviews struct {
	down bool
}

func (r *tokenReviews) Create(_ context.Context, review *authenticationv1.TokenReview, _ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
	if r.down {
		return nil, errors.New("connection refused")
	}

	answer := review.DeepCopy()

	switch review.Spec.Token {
	case "token-for-proxy":
		answer.Status = authenticationv1.TokenReviewStatus{
			Authenticated: true, Audiences: []string{"registrygate"},
			User: authenticationv1.UserInfo{
				Username: "system:serviceaccount:ci-foo:builder",
				Extra:    map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-name": {"build-1"}},
			},
		}
	case "token-for-api-server":
		answer.Status = authenticationv1.TokenReviewStatus{
			Authenticated: true, Audiences: []string{"https://kubernetes.default.svc"},
			User: authenticationv1.UserInfo{Username: "system:serviceaccount:ci-foo:builder"},
		}
	default:
		answer.Status = authenticationv1.TokenReviewStatus{Authenticated: false, Error: "invalid bearer token"}
	}

	return answer, nil
}

func TestRealReviewerKeepsUnauthenticatedRequestsAwayFromTheUpstream(t *testing.T) {
	reviews := &tokenReviews{}
	reviewer, err := authn.NewReviewer(reviews, authn.Options{Audience: "registrygate", CacheTTL: time.Second, ReviewTimeout: 5 * time.Second})
	require.NoError(t, err)

	proxy := newHarness(t, harnessOptions{reviewer: reviewer})

	target := "/v2/soliddowant/foo/source/manifests/latest"

	tests := []struct {
		name   string
		token  string
		down   bool
		status int
	}{
		{name: "token without the proxy audience", token: "token-for-api-server", status: http.StatusUnauthorized},
		{name: "credential that is not a token", token: "not-a-token", status: http.StatusUnauthorized},
		{name: "token review failing", token: "token-for-proxy", down: true, status: http.StatusServiceUnavailable},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reviews.down = test.down

			proxy.fake.ResetLog()

			got := proxy.do(t, http.MethodGet, target, basic(test.token))
			assert.Equal(t, test.status, got.status)

			for _, request := range []struct{ method, target string }{
				{http.MethodPut, "/v2/soliddowant/foo/app/manifests/1"},
				{http.MethodGet, target},
			} {
				answer := proxy.do(t, request.method, request.target, basic(test.token))
				assert.Equal(t, test.status, answer.status, request.target)
			}

			assert.Empty(t, proxy.fake.Log(), "nothing reaches the upstream, with or without the PAT")
		})
	}
}

func TestDeniedPushAnswers403NamingTheCallerAndSendsNothingUpstream(t *testing.T) {
	proxy := newHarness(t, harnessOptions{})

	tests := []struct {
		name   string
		method string
		target string
	}{
		{name: "manifest push", method: http.MethodPut, target: "/v2/soliddowant/foo/other/manifests/1"},
		{name: "upload start", method: http.MethodPost, target: "/v2/soliddowant/foo/other/blobs/uploads/"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy.fake.ResetLog()

			got := proxy.do(t, test.method, test.target, basic(builderToken))

			assert.Equal(t, http.StatusForbidden, got.status)
			assert.Equal(t, "DENIED", got.code)
			assert.Contains(t, got.body, "ci-foo/builder")
			assert.Contains(t, got.body, "push")
			assert.Contains(t, got.body, "soliddowant/foo/other")
			assert.Empty(t, proxy.fake.Log())
		})
	}
}

func TestCatalogAndUnknownPathsAre404AndNeverReachTheUpstream(t *testing.T) {
	proxy := newHarness(t, harnessOptions{})

	targets := []string{
		"/v2/_catalog",
		"/v2/soliddowant/foo/app/unknown",
		"/v2/soliddowant/foo/app/manifests",
		"/v2/Soliddowant/foo/app/manifests/latest",
		"/v3/",
		"/",
	}

	for _, target := range targets {
		for _, authorization := range [][]string{nil, {basic(builderToken)}} {
			got := proxy.do(t, http.MethodGet, target, authorization...)

			assert.Equal(t, http.StatusNotFound, got.status, target)
			assert.Equal(t, "NOT_FOUND", got.code, target)
		}
	}

	assert.Empty(t, proxy.fake.Log())
}

func TestNonCanonicalPathsAre400AndNeverReachTheUpstream(t *testing.T) {
	proxy := newHarness(t, harnessOptions{})

	targets := []string{
		"/v2/soliddowant/foo/../foo/app/manifests/latest",
		"/v2/soliddowant/foo/./app/manifests/latest",
		"/v2/soliddowant//foo/app/manifests/latest",
		"/v2/soliddowant%2Ffoo/app/manifests/latest",
		"/%76%32/soliddowant/foo/app/manifests/latest",
		"/V2/soliddowant/foo/app/manifests/latest",
	}

	for _, target := range targets {
		for _, authorization := range [][]string{nil, {basic(builderToken)}} {
			got := proxy.do(t, http.MethodGet, target, authorization...)

			assert.Equal(t, http.StatusBadRequest, got.status, target)
			assert.Contains(t, []string{"NAME_INVALID", "UNSUPPORTED"}, got.code, target)
		}
	}

	assert.Empty(t, proxy.fake.Log())
}

func TestNotReadyReplicaAnswers503ToAuthenticatedRequests(t *testing.T) {
	tests := []struct {
		name    string
		options harnessOptions
	}{
		{name: "a referenced Secret still pending", options: harnessOptions{configure: func(objects *objects) {
			objects.secrets[secrets.Key{Namespace: adminNamespace, Name: "shared-credential"}] = secrets.Secret{State: secrets.Pending}
		}}},
		{name: "upload signing key not loaded", options: harnessOptions{noSigningKey: true}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy := newHarness(t, test.options)

			require.False(t, proxy.readiness.Ready())

			for _, request := range []struct{ method, target string }{
				{http.MethodGet, "/v2/"},
				{http.MethodGet, "/v2/soliddowant/foo/source/manifests/latest"},
				{http.MethodPut, "/v2/soliddowant/foo/app/manifests/1"},
			} {
				got := proxy.do(t, request.method, request.target, basic(builderToken))

				assert.Equal(t, http.StatusServiceUnavailable, got.status, request.target)
				assert.Equal(t, "UNAVAILABLE", got.code, request.target)
				assert.Equal(t, "registrygate is not ready", got.body, request.target)
				assert.Equal(t, "5", got.header.Get("Retry-After"), request.target)
			}

			for _, entry := range proxy.fake.Log() {
				assert.NotEqual(t, fakeregistry.CredPATToken, entry.Credential, "%s %s", entry.Method, entry.Path)
				assert.NotEqual(t, fakeregistry.CredPATBasic, entry.Credential, "%s %s", entry.Method, entry.Path)
			}

			assert.Equal(t, http.StatusUnauthorized, proxy.do(t, http.MethodGet, "/v2/").status,
				"anonymous version checks don't need readiness")
		})
	}
}

func TestMountFromRepositoryTheCallerCannotPullIsStripped(t *testing.T) {
	proxy := newHarness(t, harnessOptions{})

	image, err := random.Image(256, 1)
	require.NoError(t, err)

	layers, err := image.Layers()
	require.NoError(t, err)

	layerDigest, err := layers[0].Digest()
	require.NoError(t, err)

	require.NoError(t, proxy.fake.Push("soliddowant/foo/source", image))
	require.NoError(t, proxy.fake.Push("soliddowant/foo/private", image))

	tests := []struct {
		name      string
		from      string
		mountKept bool
	}{
		{name: "source the caller may pull", from: "soliddowant/foo/source", mountKept: true},
		{name: "source the caller may not pull", from: "soliddowant/foo/private"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy.fake.ResetLog()

			target := "/v2/soliddowant/foo/app/blobs/uploads/?mount=" + layerDigest.String() + "&from=" + test.from
			got := proxy.do(t, http.MethodPost, target, basic(builderToken))
			assert.Less(t, got.status, 300, got.body)

			var uploads []fakeregistry.Entry

			for _, entry := range proxy.fake.Log() {
				if entry.Method == http.MethodPost {
					uploads = append(uploads, entry)
				}
			}

			require.Len(t, uploads, 1)

			if test.mountKept {
				assert.ElementsMatch(t, []string{"from", "mount"}, uploads[0].QueryKeys)
			} else {
				assert.Empty(t, uploads[0].QueryKeys)
			}
		})
	}
}

func TestReadyzFollowsReadiness(t *testing.T) {
	var notReady atomic.Bool

	readiness := server.NewReadiness(func() bool { return !notReady.Load() })
	health := httptest.NewServer(server.NewHealthHandler(readiness.Ready, nil))
	t.Cleanup(health.Close)

	status := func(path string) int {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, health.URL+path, nil)
		require.NoError(t, err)

		resp, err := health.Client().Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		return resp.StatusCode
	}

	assert.Equal(t, http.StatusOK, status("/readyz"))
	assert.Equal(t, http.StatusOK, status("/healthz"))
	assert.Equal(t, http.StatusOK, status("/metrics"))

	notReady.Store(true)
	assert.Equal(t, http.StatusServiceUnavailable, status("/readyz"))
	assert.Equal(t, http.StatusOK, status("/healthz"))

	notReady.Store(false)
	readiness.ShutDown()
	assert.Equal(t, http.StatusServiceUnavailable, status("/readyz"), "shutdown makes the replica not ready")
}

func TestMetricsServeGoAndProcessCollectors(t *testing.T) {
	health := httptest.NewServer(server.NewHealthHandler(func() bool { return true }, nil))
	t.Cleanup(health.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, health.URL+"/metrics", nil)
	require.NoError(t, err)

	resp, err := health.Client().Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Contains(t, string(body), "go_goroutines")

	if _, err := os.Stat("/proc/self/stat"); err == nil {
		assert.Contains(t, string(body), "process_resident_memory_bytes")
	}

	assert.False(t, strings.Contains(string(body), "ghcr"), "no proxy metrics beyond the Go and process collectors")
}

func TestRequestsAreCountedByActionDecisionAndStatusClass(t *testing.T) {
	proxyMetrics := metrics.New()
	proxy := newHarness(t, harnessOptions{metrics: proxyMetrics})
	proxyMetrics.SetPolicyCounts(func() (int, int) { return 1, 0 })

	image, err := random.Image(64, 1)
	require.NoError(t, err)
	require.NoError(t, proxy.fake.Push("soliddowant/foo/source:latest", image))

	proxy.do(t, http.MethodGet, "/v2/")
	proxy.do(t, http.MethodGet, "/v2/soliddowant/foo/source/manifests/latest", basic(rejectedToken))
	proxy.do(t, http.MethodGet, "/v2/soliddowant/foo/source/manifests/latest", basic(unavailableToken))
	proxy.do(t, http.MethodPut, "/v2/soliddowant/foo/other/manifests/1", basic(builderToken))
	proxy.do(t, http.MethodGet, "/v2/_catalog", basic(builderToken))
	proxy.do(t, http.MethodGet, "/v2/soliddowant/foo/source/manifests/latest", basic(builderToken))

	// The count is recorded after the response is written, so the last one can land just after the client returns.
	expected := `
# HELP registrygate_requests_total Requests served, by action, decision and status class.
# TYPE registrygate_requests_total counter
registrygate_requests_total{action="none",code="4xx",decision="unauthenticated"} 1
registrygate_requests_total{action="pull",code="4xx",decision="unauthenticated"} 1
registrygate_requests_total{action="pull",code="5xx",decision="unavailable"} 1
registrygate_requests_total{action="push",code="4xx",decision="denied"} 1
registrygate_requests_total{action="pull",code="2xx",decision="allowed"} 1
registrygate_requests_total{action="unknown",code="4xx",decision="notfound"} 1
`

	var compareErr error

	require.Eventually(t, func() bool {
		compareErr = testutil.GatherAndCompare(proxyMetrics.Gatherer(), strings.NewReader(expected), "registrygate_requests_total")
		return compareErr == nil
	}, 10*time.Second, 10*time.Millisecond, "%v", compareErr)
}
