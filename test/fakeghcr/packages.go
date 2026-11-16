package fakeghcr

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// The packages API is the slice of GitHub's REST API the proxy uses to delete
// container package versions: listing a package's versions and deleting one,
// for user and organization owners. A package's versions are the manifests
// pushed to the fake registry, one per digest.

const (
	defaultPerPage = 30
	maxPerPage     = 100
)

// packageVersion is one manifest digest of a package and the tags on it.
type packageVersion struct {
	id     int64
	digest string
	tags   []string
}

// packageStore tracks the versions of every repository pushed to the fake.
type packageStore struct {
	mu       sync.Mutex
	nextID   int64
	versions map[string][]*packageVersion // repository → versions, oldest first
	orgs     map[string]bool
}

func (s *packageStore) manifestPut(repo, reference, digest string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.versions == nil {
		s.versions = map[string][]*packageVersion{}
	}

	var version *packageVersion

	for _, existing := range s.versions[repo] {
		if existing.digest == digest {
			version = existing
		}
	}

	if version == nil {
		s.nextID++
		version = &packageVersion{id: s.nextID, digest: digest}
		s.versions[repo] = append(s.versions[repo], version)
	}

	if reference != digest && !slices.Contains(version.tags, reference) {
		version.tags = append(version.tags, reference)
	}
}

func (s *packageStore) manifestDeleted(repo, reference string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.versions[repo] = slices.DeleteFunc(s.versions[repo], func(version *packageVersion) bool {
		version.tags = slices.DeleteFunc(version.tags, func(tag string) bool { return tag == reference })
		return version.digest == reference
	})
}

// SetOrg makes owner an organization for the packages API. Every other owner
// is a user, and each is found only under its own owner type.
func (f *Fake) SetOrg(owner string) {
	f.packages.mu.Lock()
	defer f.packages.mu.Unlock()

	if f.packages.orgs == nil {
		f.packages.orgs = map[string]bool{}
	}

	f.packages.orgs[strings.ToLower(owner)] = true
}

// trackManifests notes manifests written to and deleted from the registry, so
// the packages API lists them as versions.
func (f *Fake) trackManifests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		recorder := &statusRecorder{ResponseWriter: resp, status: http.StatusOK}
		next.ServeHTTP(recorder, req)

		repo, reference, ok := manifestRoute(req.URL.Path)
		if !ok {
			return
		}

		switch {
		case req.Method == http.MethodPut && recorder.status == http.StatusCreated:
			f.packages.manifestPut(repo, reference, resp.Header().Get("Docker-Content-Digest"))
		case req.Method == http.MethodDelete && recorder.status == http.StatusAccepted:
			f.packages.manifestDeleted(repo, reference)
		}
	})
}

func manifestRoute(path string) (repo, reference string, ok bool) {
	rest, found := strings.CutPrefix(path, "/v2/")
	if !found {
		return "", "", false
	}

	cut := strings.LastIndex(rest, "/manifests/")
	if cut <= 0 {
		return "", "", false
	}

	return rest[:cut], rest[cut+len("/manifests/"):], true
}

func (f *Fake) apiHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{ownerType}/{owner}/packages/container/{package}/versions", f.listVersions)
	mux.HandleFunc("DELETE /{ownerType}/{owner}/packages/container/{package}/versions/{id}", f.deleteVersion)
	mux.HandleFunc("/", func(resp http.ResponseWriter, _ *http.Request) {
		writeAPIError(resp, http.StatusNotFound, "Not Found")
	})

	return f.record("api", "", f.requirePAT(mux))
}

// requirePAT answers 401 to API requests without the PAT as a bearer token.
func (f *Fake) requirePAT(next http.Handler) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		if !f.bearerIsPAT(req) {
			writeAPIError(resp, http.StatusUnauthorized, "Bad credentials")
			return
		}

		next.ServeHTTP(resp, req)
	})
}

// bearerIsPAT reports whether the request carries the PAT itself, in the
// "Bearer" or the "token" scheme as GitHub's API accepts it.
func (f *Fake) bearerIsPAT(req *http.Request) bool {
	header := req.Header.Get("Authorization")
	for _, scheme := range []string{"Bearer ", "token "} {
		if token, found := strings.CutPrefix(header, scheme); found {
			return subtle.ConstantTimeCompare([]byte(token), []byte(f.pat)) == 1
		}
	}

	return false
}

func writeAPIError(resp http.ResponseWriter, status int, message string) {
	resp.Header().Set("Content-Type", "application/json")
	resp.WriteHeader(status)
	_ = json.NewEncoder(resp).Encode(map[string]string{"message": message})
}

// packageVersions returns a copy of the versions of the package the request
// names, newest first, or ok false when the owner type is wrong or the
// package has no versions.
func (f *Fake) packageVersions(req *http.Request) (repo string, versions []packageVersion, ok bool) {
	owner := strings.ToLower(req.PathValue("owner"))
	repo = owner + "/" + req.PathValue("package")

	f.packages.mu.Lock()
	defer f.packages.mu.Unlock()

	isOrg := f.packages.orgs[owner]

	switch req.PathValue("ownerType") {
	case "orgs":
		if !isOrg {
			return "", nil, false
		}
	case "users":
		if isOrg {
			return "", nil, false
		}
	default:
		return "", nil, false
	}

	for _, version := range slices.Backward(f.packages.versions[repo]) {
		versions = append(versions, packageVersion{id: version.id, digest: version.digest, tags: slices.Clone(version.tags)})
	}

	return repo, versions, len(versions) > 0
}

type apiVersion struct {
	ID       int64          `json:"id"`
	Name     string         `json:"name"`
	Metadata apiVersionMeta `json:"metadata"`
}

type apiVersionMeta struct {
	PackageType string           `json:"package_type"`
	Container   apiContainerMeta `json:"container"`
}

type apiContainerMeta struct {
	Tags []string `json:"tags"`
}

func (f *Fake) listVersions(resp http.ResponseWriter, req *http.Request) {
	_, versions, ok := f.packageVersions(req)
	if !ok {
		writeAPIError(resp, http.StatusNotFound, "Package not found.")
		return
	}

	query := req.URL.Query()

	perPage, err := strconv.Atoi(query.Get("per_page"))
	if err != nil || perPage <= 0 {
		perPage = defaultPerPage
	}

	perPage = min(perPage, maxPerPage)

	page, err := strconv.Atoi(query.Get("page"))
	if err != nil || page <= 0 {
		page = 1
	}

	start := min((page-1)*perPage, len(versions))
	end := min(start+perPage, len(versions))

	if end < len(versions) {
		query.Set("page", strconv.Itoa(page+1))
		query.Set("per_page", strconv.Itoa(perPage))
		next := url.URL{Scheme: "https", Host: f.apiHost, Path: req.URL.Path, RawPath: req.URL.RawPath, RawQuery: query.Encode()}
		resp.Header().Set("Link", "<"+next.String()+`>; rel="next"`)
	}

	body := make([]apiVersion, 0, end-start)
	for _, version := range versions[start:end] {
		tags := version.tags
		if tags == nil {
			tags = []string{}
		}

		body = append(body, apiVersion{
			ID: version.id, Name: version.digest,
			Metadata: apiVersionMeta{PackageType: "container", Container: apiContainerMeta{Tags: tags}},
		})
	}

	resp.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(resp).Encode(body)
}

func (f *Fake) deleteVersion(resp http.ResponseWriter, req *http.Request) {
	repo, versions, ok := f.packageVersions(req)
	if !ok {
		writeAPIError(resp, http.StatusNotFound, "Package not found.")
		return
	}

	index := slices.IndexFunc(versions, func(version packageVersion) bool {
		return strconv.FormatInt(version.id, 10) == req.PathValue("id")
	})
	if index < 0 {
		writeAPIError(resp, http.StatusNotFound, "Not Found")
		return
	}

	// Remove the manifest and its tags from the registry the way a registry
	// client would, so the registry and this API agree on what exists.
	version := versions[index]
	for _, reference := range append(version.tags, version.digest) {
		removal := httptest.NewRequestWithContext(req.Context(), http.MethodDelete, "/v2/"+repo+"/manifests/"+reference, nil)
		f.registryStore.ServeHTTP(httptest.NewRecorder(), removal)
	}

	resp.WriteHeader(http.StatusNoContent)
}
