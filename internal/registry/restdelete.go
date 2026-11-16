package registry

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v93/github"
	"github.com/opencontainers/go-digest"

	"github.com/solidDoWant/registrygate/internal/metrics"
	"github.com/solidDoWant/registrygate/internal/upstream"
)

// GHCR refuses every registry delete endpoint, so a manifest delete by digest
// deletes the package version through GitHub's REST API instead.

const (
	// ownerTypeTTL is how long a found owner type is reused.
	ownerTypeTTL = time.Hour
	// versionsPerPage is the page size of the versions listing.
	versionsPerPage = 100

	messageDeleteRefused     = "GHCR refused to delete this version"
	messageCannotDelete      = "the proxy's credential can't delete packages"
	messageRateLimited       = "upstream rate limit reached"
	defaultRateLimitRetrySec = 60
)

// APIConfig configures the REST delete path.
type APIConfig struct {
	// URL is the GitHub REST API base URL, for example https://api.github.com/.
	URL *url.URL
	// Transport carries REST calls. It must add no credentials.
	// http.DefaultTransport when nil.
	Transport http.RoundTripper
	// Timeout bounds one whole delete, listing included.
	Timeout time.Duration
	PAT     upstream.PATSource
}

// restDeleter deletes container package versions through the REST API.
type restDeleter struct {
	client  *github.Client
	timeout time.Duration

	mu         sync.Mutex
	ownerTypes map[string]ownerTypeEntry
	now        func() time.Time
}

type ownerTypeEntry struct {
	ownerType string
	expires   time.Time
}

func newRESTDeleter(cfg APIConfig, proxyMetrics *metrics.Metrics) (*restDeleter, error) {
	inner := cfg.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}

	baseURL := *cfg.URL
	if !strings.HasSuffix(baseURL.Path, "/") {
		baseURL.Path += "/"
	}

	httpClient := &http.Client{
		Transport: &patTransport{inner: inner, pat: cfg.PAT, host: baseURL.Host, metrics: proxyMetrics},
		// A redirect is answered as it is: following it could carry the PAT
		// to another host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	base := baseURL.String()

	client, err := github.NewClient(
		github.WithHTTPClient(httpClient),
		github.WithURLs(&base, &base),
		github.WithDisableRateLimitCheck(),
	)
	if err != nil {
		return nil, err
	}

	return &restDeleter{client: client, timeout: cfg.Timeout, ownerTypes: map[string]ownerTypeEntry{}, now: time.Now}, nil
}

// errPATMissing is returned by patTransport when no PAT is loaded.
var errPATMissing = errors.New("PAT not loaded")

// patTransport adds the PAT to REST calls to the API host and refuses every
// other host, so the PAT never leaves for anywhere else.
type patTransport struct {
	inner   http.RoundTripper
	pat     upstream.PATSource
	host    string
	metrics *metrics.Metrics
}

func (t *patTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.EqualFold(req.URL.Host, t.host) {
		return nil, errors.New("REST call outside the API host refused")
	}

	pat, _, ok := t.pat.Get()
	if !ok {
		return nil, errPATMissing
	}

	out := req.Clone(req.Context())
	out.Header.Set("Authorization", "Bearer "+pat)

	started := time.Now()
	resp, err := t.inner.RoundTrip(out)
	t.metrics.UpstreamRequest(metrics.TargetAPI, statusOf(resp), time.Since(started))

	return resp, err
}

// isRESTDelete reports whether the request is a manifest delete by digest.
func isRESTDelete(request Request) bool {
	if request.Route != RouteManifest || request.Method != http.MethodDelete {
		return false
	}

	_, err := digest.Parse(request.Reference)

	return err == nil
}

// restOutcome is a REST call's failure, mapped to the client's answer.
type restOutcome struct {
	answer     *Error
	retryAfter string
	// packageMissing is set when the versions listing answered 404.
	packageMissing bool
	// listed is set when the listing succeeded but held no such version.
	listed bool
}

// serve deletes the version named by request and writes the answer. It
// returns the DELETE call's status, or 0 when none was made.
func (d *restDeleter) serve(ctx context.Context, resp http.ResponseWriter, request Request) int {
	owner, pkg, found := strings.Cut(request.Name, "/")
	if !found || owner == "" || pkg == "" {
		WriteError(resp, &Error{Status: http.StatusNotFound, Code: "NAME_UNKNOWN", Message: "repository name not known to registry"})
		return 0
	}

	if d.timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, d.timeout)
		defer cancel()
	}

	ownerType, versionID, failure := d.findVersion(ctx, owner, pkg, request.Reference)
	if failure != nil {
		writeOutcome(resp, failure)
		return 0
	}

	path := ownerType + "/" + owner + "/packages/container/" + url.PathEscape(pkg) + "/versions/" + strconv.FormatInt(versionID, 10)

	req, err := d.client.NewRequest(ctx, http.MethodDelete, path, nil)
	if err != nil {
		writeOutcome(resp, &restOutcome{answer: errBadGateway(messageUnavailable)})
		return 0
	}

	answer, err := d.client.Do(req, nil)

	status := 0
	if answer != nil {
		status = answer.StatusCode
	}

	if err != nil || status != http.StatusNoContent {
		writeOutcome(resp, mapRESTFailure(answer, err))
		return status
	}

	resp.Header().Set("Content-Length", "0")
	resp.WriteHeader(http.StatusAccepted)

	return status
}

// findVersion finds the owner type and the ID of the version named digest.
func (d *restDeleter) findVersion(ctx context.Context, owner, pkg, reference string) (string, int64, *restOutcome) {
	if ownerType, ok := d.cachedOwnerType(owner); ok {
		id, failure := d.listVersions(ctx, ownerType, owner, pkg, reference)
		return ownerType, id, failure
	}

	for _, ownerType := range []string{"orgs", "users"} {
		id, failure := d.listVersions(ctx, ownerType, owner, pkg, reference)
		if failure == nil || failure.listed {
			d.storeOwnerType(owner, ownerType)
		}

		// A 404 from the organization listing means the owner is a user.
		if ownerType == "orgs" && failure != nil && failure.packageMissing {
			continue
		}

		return ownerType, id, failure
	}

	return "", 0, &restOutcome{answer: errManifestUnknown()}
}

// listVersions pages through the package's versions for the one named
// reference.
func (d *restDeleter) listVersions(ctx context.Context, ownerType, owner, pkg, reference string) (int64, *restOutcome) {
	page := 1

	for {
		path := ownerType + "/" + owner + "/packages/container/" + url.PathEscape(pkg) +
			"/versions?per_page=" + strconv.Itoa(versionsPerPage) + "&page=" + strconv.Itoa(page)

		req, err := d.client.NewRequest(ctx, http.MethodGet, path, nil)
		if err != nil {
			return 0, &restOutcome{answer: errBadGateway(messageUnavailable)}
		}

		var versions []*github.PackageVersion

		answer, err := d.client.Do(req, &versions)
		if err != nil {
			failure := mapRESTFailure(answer, err)
			failure.packageMissing = answer != nil && answer.StatusCode == http.StatusNotFound

			return 0, failure
		}

		for _, version := range versions {
			if version.GetName() == reference {
				return version.GetID(), nil
			}
		}

		if answer.NextPage == 0 {
			return 0, &restOutcome{answer: errManifestUnknown(), listed: true}
		}

		page = answer.NextPage
	}
}

func errManifestUnknown() *Error {
	return &Error{Status: http.StatusNotFound, Code: "MANIFEST_UNKNOWN", Message: "manifest unknown"}
}

func (d *restDeleter) cachedOwnerType(owner string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	entry, ok := d.ownerTypes[owner]
	if !ok || !d.now().Before(entry.expires) {
		return "", false
	}

	return entry.ownerType, true
}

func (d *restDeleter) storeOwnerType(owner, ownerType string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.ownerTypes[owner] = ownerTypeEntry{ownerType: ownerType, expires: d.now().Add(ownerTypeTTL)}
}

// mapRESTFailure maps a failed REST call to the client's answer.
func mapRESTFailure(answer *github.Response, err error) *restOutcome {
	if errors.Is(err, errPATMissing) {
		return &restOutcome{answer: errNotReady()}
	}

	if answer == nil || answer.Response == nil {
		return &restOutcome{answer: errBadGateway(messageUnavailable)}
	}

	switch status := answer.StatusCode; {
	case status == http.StatusNotFound:
		return &restOutcome{answer: errManifestUnknown()}
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return &restOutcome{answer: &Error{Status: http.StatusMethodNotAllowed, Code: "UNSUPPORTED", Message: messageDeleteRefused}}
	case status == http.StatusTooManyRequests || status == http.StatusForbidden && isRateLimited(answer.Header):
		return &restOutcome{
			answer:     &Error{Status: http.StatusServiceUnavailable, Code: "UNAVAILABLE", Message: messageRateLimited},
			retryAfter: retryAfter(answer.Header),
		}
	case status == http.StatusForbidden:
		return &restOutcome{answer: errBadGateway(messageCannotDelete)}
	case status == http.StatusUnauthorized:
		return &restOutcome{answer: errBadGateway(messageCredentialRejected)}
	}

	return &restOutcome{answer: errBadGateway(messageUnavailable)}
}

func isRateLimited(header http.Header) bool {
	return header.Get("X-RateLimit-Remaining") == "0" || header.Get("Retry-After") != ""
}

// retryAfter is the delay in seconds GitHub asks for: its Retry-After, or the
// time until its rate limit resets.
func retryAfter(header http.Header) string {
	if seconds, err := strconv.Atoi(header.Get("Retry-After")); err == nil && seconds >= 0 {
		return strconv.Itoa(seconds)
	}

	if reset, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		if wait := time.Until(time.Unix(reset, 0)); wait > 0 {
			return strconv.Itoa(int(wait.Seconds()) + 1)
		}

		return "1"
	}

	return strconv.Itoa(defaultRateLimitRetrySec)
}

func writeOutcome(resp http.ResponseWriter, outcome *restOutcome) {
	if outcome.retryAfter != "" {
		resp.Header().Set("Retry-After", outcome.retryAfter)
	}

	WriteError(resp, outcome.answer)
}
