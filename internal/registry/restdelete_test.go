package registry_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/internal/metrics"
	"github.com/solidDoWant/registrygate/internal/registry"
	"github.com/solidDoWant/registrygate/test/fakeghcr"
)

// withAPI sends deletes by digest to the REST API at apiURL.
func withAPI(apiURL string, transport http.RoundTripper, timeout time.Duration) func(*registry.ForwarderConfig) {
	return func(cfg *registry.ForwarderConfig) {
		parsed, err := url.Parse(apiURL)
		if err != nil {
			panic(err)
		}

		cfg.API = registry.APIConfig{URL: parsed, Transport: transport, Timeout: timeout}
	}
}

func withFakeAPI(fake *fakeghcr.Fake) func(*registry.ForwarderConfig) {
	return withAPI(fake.APIURL(), fake.Transport(), 10*time.Second)
}

// pushRandom pushes a random image to repo on the fake and returns its digest.
func pushRandom(t *testing.T, fake *fakeghcr.Fake, repo string) string {
	t.Helper()

	image, err := random.Image(256, 1)
	require.NoError(t, err)
	require.NoError(t, fake.Push(repo+":v1", image))

	hash, err := image.Digest()
	require.NoError(t, err)

	return hash.String()
}

// apiCalls are the REST calls the fake saw, as "METHOD escaped-path".
func apiCalls(fake *fakeghcr.Fake) []string {
	var calls []string

	for _, entry := range fake.Log() {
		if entry.Server == "api" {
			calls = append(calls, entry.Method+" "+entry.EscapedPath)
		}
	}

	return calls
}

// registryDeletes are the registry DELETE requests the fake saw.
func registryDeletes(fake *fakeghcr.Fake) []string {
	var deletes []string

	for _, entry := range fake.Log() {
		if entry.Server == "registry" && entry.Method == http.MethodDelete {
			deletes = append(deletes, entry.Path)
		}
	}

	return deletes
}

func TestDeleteByDigestDeletesThePackageVersionThroughTheRESTAPI(t *testing.T) {
	tests := []struct {
		name      string
		owner     string
		org       bool
		wantCalls []string
	}{
		{
			name: "user owner", owner: "someone",
			wantCalls: []string{
				"GET /orgs/someone/packages/container/team%2Fapp/versions",
				"GET /users/someone/packages/container/team%2Fapp/versions",
				"DELETE /users/someone/packages/container/team%2Fapp/versions/1",
			},
		},
		{
			name: "organization owner", owner: "someorg", org: true,
			wantCalls: []string{
				"GET /orgs/someorg/packages/container/team%2Fapp/versions",
				"DELETE /orgs/someorg/packages/container/team%2Fapp/versions/1",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := fakeghcr.New(t, fakeghcr.Options{})
			if test.org {
				fake.SetOrg(test.owner)
			}

			harness := newProxyHarness(t, fake, fake.PAT(), withPAT, withFakeAPI(fake))
			repo := test.owner + "/team/app"
			digest := pushRandom(t, fake, repo)
			fake.ResetLog()

			status, _, body := harness.do(t, http.MethodDelete, "/v2/"+repo+"/manifests/"+digest)
			require.Equal(t, http.StatusAccepted, status, body)

			assert.Equal(t, test.wantCalls, apiCalls(fake))
			assert.Empty(t, registryDeletes(fake), "no registry DELETE reaches GHCR")

			for _, entry := range fake.Log() {
				if entry.Server == "api" {
					assert.Equal(t, fakeghcr.CredPATBearer, entry.Credential)
				}
			}

			status, _, _ = harness.do(t, http.MethodGet, "/v2/"+repo+"/manifests/"+digest)
			assert.Equal(t, http.StatusNotFound, status, "the manifest is gone")
		})
	}
}

func TestDeleteByDigestCachesTheOwnerType(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})
	harness := newProxyHarness(t, fake, fake.PAT(), withPAT, withFakeAPI(fake))

	first := pushRandom(t, fake, "someone/one")
	second := pushRandom(t, fake, "someone/two")

	status, _, body := harness.do(t, http.MethodDelete, "/v2/someone/one/manifests/"+first)
	require.Equal(t, http.StatusAccepted, status, body)

	fake.ResetLog()

	status, _, body = harness.do(t, http.MethodDelete, "/v2/someone/two/manifests/"+second)
	require.Equal(t, http.StatusAccepted, status, body)

	assert.Equal(t, []string{
		"GET /users/someone/packages/container/two/versions",
		"DELETE /users/someone/packages/container/two/versions/2",
	}, apiCalls(fake))
}

func TestDeleteByDigestRecordsRESTCallsUnderTheAPITarget(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})
	proxyMetrics := metrics.New()
	harness := newProxyHarness(t, fake, fake.PAT(), withPAT, withMetrics(proxyMetrics), withFakeAPI(fake))

	digest := pushRandom(t, fake, "someone/app")

	status, _, body := harness.do(t, http.MethodDelete, "/v2/someone/app/manifests/"+digest)
	require.Equal(t, http.StatusAccepted, status, body)

	require.NoError(t, testutil.GatherAndCompare(proxyMetrics.Gatherer(), strings.NewReader(`
# HELP registrygate_upstream_requests_total Requests sent to the upstream or a mirror, by status class.
# TYPE registrygate_upstream_requests_total counter
registrygate_upstream_requests_total{code="2xx",target="api"} 2
registrygate_upstream_requests_total{code="4xx",target="api"} 1
`), "registrygate_upstream_requests_total"))
}

func TestDeleteByDigestOutsideThePolicyGetsNoPATBackedAccess(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})
	notAllowed := func(request registry.Request) registry.Forward {
		return registry.Forward{Request: request, Credential: registry.CredentialAnonymous}
	}
	harness := newProxyHarness(t, fake, fake.PAT(), notAllowed, withFakeAPI(fake))

	digest := pushRandom(t, fake, "someone/app")
	fake.ResetLog()

	status, _, _ := harness.do(t, http.MethodDelete, "/v2/someone/app/manifests/"+digest)
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Empty(t, fake.Log(), "neither the registry nor the REST API is called")
}

func TestDeleteByDigestOfASingleSegmentNameIsNameUnknown(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})
	harness := newProxyHarness(t, fake, fake.PAT(), withPAT, withFakeAPI(fake))

	status, _, body := harness.do(t, http.MethodDelete,
		"/v2/someone/manifests/sha256:0000000000000000000000000000000000000000000000000000000000000000")
	assert.Equal(t, http.StatusNotFound, status)
	assert.Contains(t, body, "NAME_UNKNOWN")
	assert.Empty(t, fake.Log())
}

func TestDeleteByTagAndBlobDeleteGoToTheRegistryWithTheDeleteScope(t *testing.T) {
	fake := fakeghcr.New(t, fakeghcr.Options{})
	harness := newProxyHarness(t, fake, fake.PAT(), withPAT, withFakeAPI(fake))

	pushRandom(t, fake, "someone/app")
	fake.ResetLog()

	harness.do(t, http.MethodDelete, "/v2/someone/app/manifests/v1")
	harness.do(t, http.MethodDelete,
		"/v2/someone/app/blobs/sha256:0000000000000000000000000000000000000000000000000000000000000000")

	assert.Empty(t, apiCalls(fake))
	assert.Equal(t, []string{
		"/v2/someone/app/manifests/v1",
		"/v2/someone/app/blobs/sha256:0000000000000000000000000000000000000000000000000000000000000000",
	}, registryDeletes(fake))
	assert.Contains(t, harness.recorder.tokenScopes(), "repository:someone/app:delete")
}

// stubAPI is a REST API double that answers each call with a scripted
// answer, for the answers the fake GHCR can't give.
type stubAPI struct {
	server *httptest.Server

	mu    sync.Mutex
	calls []string
}

type stubAnswer struct {
	status int
	header map[string]string
	body   string
}

const stubDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func newStubAPI(t *testing.T, answer func(req *http.Request) stubAnswer) *stubAPI {
	t.Helper()

	stub := &stubAPI{}
	stub.server = httptest.NewTLSServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		stub.mu.Lock()
		stub.calls = append(stub.calls, req.Method+" "+req.URL.RequestURI())
		stub.mu.Unlock()

		got := answer(req)
		for key, value := range got.header {
			resp.Header().Set(key, value)
		}

		resp.Header().Set("Content-Type", "application/json")
		resp.WriteHeader(got.status)
		_, _ = resp.Write([]byte(got.body))
	}))
	t.Cleanup(stub.server.Close)

	return stub
}

func (s *stubAPI) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.calls...)
}

// versionsBody is a versions listing holding one version.
func versionsBody(id int, digest string) string {
	return `[{"id":` + strconv.Itoa(id) + `,"name":"` + digest + `"}]`
}

// listingThenDelete answers the org listing with the stub digest and the
// delete with deleteAnswer.
func listingThenDelete(deleteAnswer stubAnswer) func(*http.Request) stubAnswer {
	return func(req *http.Request) stubAnswer {
		if req.Method == http.MethodGet {
			return stubAnswer{status: http.StatusOK, body: versionsBody(7, stubDigest)}
		}

		return deleteAnswer
	}
}

func TestDeleteByDigestMapsTheRESTAnswer(t *testing.T) {
	tests := []struct {
		name           string
		answer         func(*http.Request) stubAnswer
		wantStatus     int
		wantCode       string
		wantMessage    string
		wantRetryAfter string
	}{
		{
			name:       "204 is 202 Accepted",
			answer:     listingThenDelete(stubAnswer{status: http.StatusNoContent}),
			wantStatus: http.StatusAccepted,
		},
		{
			name: "version not in the listing is MANIFEST_UNKNOWN",
			answer: func(*http.Request) stubAnswer {
				return stubAnswer{status: http.StatusOK, body: versionsBody(7, "sha256:2222222222222222222222222222222222222222222222222222222222222222")}
			},
			wantStatus: http.StatusNotFound, wantCode: "MANIFEST_UNKNOWN",
		},
		{
			name: "package 404 is MANIFEST_UNKNOWN",
			answer: func(*http.Request) stubAnswer {
				return stubAnswer{status: http.StatusNotFound, body: `{"message":"Package not found."}`}
			},
			wantStatus: http.StatusNotFound, wantCode: "MANIFEST_UNKNOWN",
		},
		{
			name:       "400 is 405 UNSUPPORTED",
			answer:     listingThenDelete(stubAnswer{status: http.StatusBadRequest, body: `{"message":"You cannot delete the last tagged version of a package."}`}),
			wantStatus: http.StatusMethodNotAllowed, wantCode: "UNSUPPORTED", wantMessage: "GHCR refused to delete this version",
		},
		{
			name:       "422 is 405 UNSUPPORTED",
			answer:     listingThenDelete(stubAnswer{status: http.StatusUnprocessableEntity, body: `{"message":"no"}`}),
			wantStatus: http.StatusMethodNotAllowed, wantCode: "UNSUPPORTED", wantMessage: "GHCR refused to delete this version",
		},
		{
			name: "403 with no rate limit left is 503 with Retry-After",
			answer: listingThenDelete(stubAnswer{status: http.StatusForbidden, body: `{"message":"API rate limit exceeded"}`, header: map[string]string{
				"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
			}}),
			wantStatus: http.StatusServiceUnavailable, wantCode: "UNAVAILABLE",
		},
		{
			name:       "429 with Retry-After is 503 with GitHub's Retry-After",
			answer:     listingThenDelete(stubAnswer{status: http.StatusTooManyRequests, body: `{"message":"slow down"}`, header: map[string]string{"Retry-After": "42"}}),
			wantStatus: http.StatusServiceUnavailable, wantCode: "UNAVAILABLE", wantRetryAfter: "42",
		},
		{
			name: "403 with Retry-After on the listing is 503",
			answer: func(*http.Request) stubAnswer {
				return stubAnswer{status: http.StatusForbidden, header: map[string]string{"Retry-After": "17"}, body: `{"message":"secondary rate limit"}`}
			},
			wantStatus: http.StatusServiceUnavailable, wantCode: "UNAVAILABLE", wantRetryAfter: "17",
		},
		{
			name:       "other 403 is 502 naming the credential",
			answer:     listingThenDelete(stubAnswer{status: http.StatusForbidden, body: `{"message":"Must have admin rights"}`}),
			wantStatus: http.StatusBadGateway, wantCode: "UNAVAILABLE", wantMessage: "the proxy's credential can't delete packages",
		},
		{
			name: "401 is 502 credential rejected",
			answer: func(*http.Request) stubAnswer {
				return stubAnswer{status: http.StatusUnauthorized, body: `{"message":"Bad credentials"}`}
			},
			wantStatus: http.StatusBadGateway, wantCode: "UNAVAILABLE", wantMessage: "upstream rejected the proxy's credential",
		},
		{
			name:       "5xx is 502 upstream unavailable",
			answer:     listingThenDelete(stubAnswer{status: http.StatusInternalServerError, body: `{"message":"oops"}`}),
			wantStatus: http.StatusBadGateway, wantCode: "UNAVAILABLE", wantMessage: "upstream unavailable",
		},
		{
			name: "redirect is not followed and is 502 upstream unavailable",
			answer: listingThenDelete(stubAnswer{
				status: http.StatusTemporaryRedirect, header: map[string]string{"Location": "https://elsewhere.invalid/x"},
			}),
			wantStatus: http.StatusBadGateway, wantCode: "UNAVAILABLE", wantMessage: "upstream unavailable",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := fakeghcr.New(t, fakeghcr.Options{})
			stub := newStubAPI(t, test.answer)
			harness := newProxyHarness(t, fake, fake.PAT(), withPAT,
				withAPI(stub.server.URL+"/", stub.server.Client().Transport, 10*time.Second))

			status, header, body := harness.do(t, http.MethodDelete, "/v2/someorg/app/manifests/"+stubDigest)

			assert.Equal(t, test.wantStatus, status, body)

			if test.wantCode != "" {
				assert.Contains(t, body, `"code":"`+test.wantCode+`"`)
			}

			if test.wantMessage != "" {
				assert.Contains(t, body, test.wantMessage)
			}

			if test.wantStatus == http.StatusServiceUnavailable {
				assert.NotEmpty(t, header.Get("Retry-After"))
			}

			if test.wantRetryAfter != "" {
				assert.Equal(t, test.wantRetryAfter, header.Get("Retry-After"))
			}

			assert.NotContains(t, body, fake.PAT())
			assert.Empty(t, registryDeletes(fake))

			for _, call := range stub.recorded() {
				assert.NotContains(t, call, "elsewhere")
			}
		})
	}
}

func TestDeleteByDigestTimeoutAndConnectionErrorsAre502(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		release := make(chan struct{})

		t.Cleanup(func() { close(release) })

		stub := newStubAPI(t, func(req *http.Request) stubAnswer {
			select {
			case <-release:
			case <-req.Context().Done():
			}

			return stubAnswer{status: http.StatusOK, body: "[]"}
		})
		fake := fakeghcr.New(t, fakeghcr.Options{})
		harness := newProxyHarness(t, fake, fake.PAT(), withPAT,
			withAPI(stub.server.URL+"/", stub.server.Client().Transport, 100*time.Millisecond))

		status, _, body := harness.do(t, http.MethodDelete, "/v2/someorg/app/manifests/"+stubDigest)
		assert.Equal(t, http.StatusBadGateway, status)
		assert.Contains(t, body, "upstream unavailable")
	})

	t.Run("connection error", func(t *testing.T) {
		stub := newStubAPI(t, func(*http.Request) stubAnswer { return stubAnswer{status: http.StatusOK} })
		apiURL := stub.server.URL + "/"
		transport := stub.server.Client().Transport
		stub.server.Close()

		fake := fakeghcr.New(t, fakeghcr.Options{})
		harness := newProxyHarness(t, fake, fake.PAT(), withPAT, withAPI(apiURL, transport, 10*time.Second))

		status, _, body := harness.do(t, http.MethodDelete, "/v2/someorg/app/manifests/"+stubDigest)
		assert.Equal(t, http.StatusBadGateway, status)
		assert.Contains(t, body, "upstream unavailable")
	})
}

func TestDeleteByDigestPagesThroughTheVersions(t *testing.T) {
	var stub *stubAPI

	stub = newStubAPI(t, func(req *http.Request) stubAnswer {
		switch {
		case req.Method == http.MethodDelete:
			return stubAnswer{status: http.StatusNoContent}
		case req.URL.Query().Get("page") == "2":
			return stubAnswer{status: http.StatusOK, body: versionsBody(9, stubDigest)}
		}

		next := stub.server.URL + req.URL.Path + "?per_page=100&page=2"

		return stubAnswer{
			status: http.StatusOK, body: versionsBody(8, "sha256:2222222222222222222222222222222222222222222222222222222222222222"),
			header: map[string]string{"Link": "<" + next + `>; rel="next"`},
		}
	})
	fake := fakeghcr.New(t, fakeghcr.Options{})
	harness := newProxyHarness(t, fake, fake.PAT(), withPAT,
		withAPI(stub.server.URL+"/", stub.server.Client().Transport, 10*time.Second))

	status, _, body := harness.do(t, http.MethodDelete, "/v2/someorg/app/manifests/"+stubDigest)
	require.Equal(t, http.StatusAccepted, status, body)

	assert.Equal(t, []string{
		"GET /orgs/someorg/packages/container/app/versions?per_page=100&page=1",
		"GET /orgs/someorg/packages/container/app/versions?per_page=100&page=2",
		"DELETE /orgs/someorg/packages/container/app/versions/9",
	}, stub.recorded())
}

func TestDeleteByDigestNeverCachesAFailedOwnerLookup(t *testing.T) {
	var (
		mu       sync.Mutex
		failing  = true
		answered int
	)

	stub := newStubAPI(t, func(req *http.Request) stubAnswer {
		mu.Lock()
		defer mu.Unlock()

		answered++

		switch {
		case failing:
			return stubAnswer{status: http.StatusInternalServerError, body: `{"message":"oops"}`}
		case strings.HasPrefix(req.URL.Path, "/orgs/"):
			return stubAnswer{status: http.StatusNotFound, body: `{"message":"Not Found"}`}
		case req.Method == http.MethodGet:
			return stubAnswer{status: http.StatusOK, body: versionsBody(3, stubDigest)}
		}

		return stubAnswer{status: http.StatusNoContent}
	})
	fake := fakeghcr.New(t, fakeghcr.Options{})
	harness := newProxyHarness(t, fake, fake.PAT(), withPAT,
		withAPI(stub.server.URL+"/", stub.server.Client().Transport, 10*time.Second))

	status, _, _ := harness.do(t, http.MethodDelete, "/v2/someone/app/manifests/"+stubDigest)
	require.Equal(t, http.StatusBadGateway, status)

	mu.Lock()
	failing = false
	mu.Unlock()

	status, _, body := harness.do(t, http.MethodDelete, "/v2/someone/app/manifests/"+stubDigest)
	require.Equal(t, http.StatusAccepted, status, body)

	calls := stub.recorded()
	assert.Equal(t, "GET /orgs/someone/packages/container/app/versions?per_page=100&page=1", calls[1],
		"the second delete looks the owner type up again")
}

func TestDeleteByDigestNeverFollowsARedirect(t *testing.T) {
	var stub *stubAPI

	stub = newStubAPI(t, func(req *http.Request) stubAnswer {
		switch {
		case req.URL.Path == "/redirected":
			return stubAnswer{status: http.StatusNoContent}
		case req.Method == http.MethodGet:
			return stubAnswer{status: http.StatusOK, body: versionsBody(7, stubDigest)}
		}

		return stubAnswer{status: http.StatusTemporaryRedirect, header: map[string]string{"Location": stub.server.URL + "/redirected"}}
	})
	fake := fakeghcr.New(t, fakeghcr.Options{})
	harness := newProxyHarness(t, fake, fake.PAT(), withPAT,
		withAPI(stub.server.URL+"/", stub.server.Client().Transport, 10*time.Second))

	status, _, body := harness.do(t, http.MethodDelete, "/v2/someorg/app/manifests/"+stubDigest)
	assert.Equal(t, http.StatusBadGateway, status)
	assert.Contains(t, body, "upstream unavailable")
	assert.NotContains(t, strings.Join(stub.recorded(), "\n"), "/redirected")
}
