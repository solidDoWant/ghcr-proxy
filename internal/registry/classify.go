// Package registry classifies registry API requests and forwards them to the
// upstream registry.
package registry

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"

	"github.com/solidDoWant/ghcr-proxy/api/v1alpha1"
)

// Route identifies which registry API endpoint a request addresses.
type Route int

const (
	// RouteBase is the version check, /v2/.
	RouteBase Route = iota + 1
	// RouteManifest is /v2/<name>/manifests/<ref>.
	RouteManifest
	// RouteBlob is /v2/<name>/blobs/<digest>.
	RouteBlob
	// RouteTagsList is /v2/<name>/tags/list.
	RouteTagsList
	// RouteReferrers is /v2/<name>/referrers/<digest>.
	RouteReferrers
	// RouteUploadStart is /v2/<name>/blobs/uploads/.
	RouteUploadStart
	// RouteUploadSession is /v2/<name>/blobs/uploads/<session>, or GHCR's
	// /v2/<name>/blobs/upload/<session>.
	RouteUploadSession
)

const (
	maxPathLength = 2048
	maxNameLength = reference.RepositoryNameTotalLengthMax
	basePath      = "/v2/"
)

// sessionPattern is the grammar of an upload session identifier: one path
// segment of URL-safe characters, so it can be put back into a path as is.
var sessionPattern = regexp.MustCompile(`^[A-Za-z0-9._=~+-]{1,256}$`)

// Mount is a valid cross-repository mount on an upload start. It needs pull
// on From in addition to push on the request's name.
type Mount struct {
	Digest digest.Digest
	From   string
}

// Request is a classified registry request. Every field the upstream sees is
// rebuilt from these values by UpstreamURL, so a decision made on them is a
// decision on exactly what is forwarded.
type Request struct {
	Method string
	Route  Route
	// Action is the action needed on Name. It is empty for the version check.
	Action v1alpha1.Action
	// Name is the repository name, lower-case.
	Name string
	// Reference is the tag or digest of a manifest route.
	Reference string
	// Digest is the digest of a blob or referrers route.
	Digest digest.Digest
	// Session is the upload session identifier of an upload session route.
	Session string
	// Mount is set on an upload start with a valid mount. Setting it to nil
	// turns the request into a plain upload start.
	Mount *Mount

	// uploadSegment is the segment before the session of an upload session
	// route, "uploads" or GHCR's "upload". The session URL is the upstream's
	// own, so it is forwarded in the form the client was given.
	uploadSegment string
	// monolithicDigest is the digest of a monolithic upload start.
	monolithicDigest digest.Digest
	// rawQuery is the query of a route that forwards the query as sent.
	rawQuery string
	// query holds the kept parameters of a route that filters its query.
	query url.Values
}

// UpstreamURL returns the path and query to send upstream, rebuilt from the
// classified fields.
func (r Request) UpstreamURL() *url.URL {
	upstream := &url.URL{Path: basePath}

	switch r.Route {
	case RouteBase:
		return upstream
	case RouteManifest:
		upstream.Path += r.Name + "/manifests/" + r.Reference
	case RouteBlob:
		upstream.Path += r.Name + "/blobs/" + r.Digest.String()
	case RouteTagsList:
		upstream.Path += r.Name + "/tags/list"
	case RouteReferrers:
		upstream.Path += r.Name + "/referrers/" + r.Digest.String()
	case RouteUploadStart:
		upstream.Path += r.Name + "/blobs/uploads/"

		query := url.Values{}
		if r.monolithicDigest != "" {
			query.Set("digest", r.monolithicDigest.String())
		}

		if r.Mount != nil {
			query.Set("mount", r.Mount.Digest.String())
			query.Set("from", r.Mount.From)
		}

		upstream.RawQuery = query.Encode()
	case RouteUploadSession:
		upstream.Path += r.Name + "/blobs/" + r.uploadSegment + "/" + r.Session
		upstream.RawQuery = r.rawQuery
	}

	if r.query != nil {
		upstream.RawQuery = r.query.Encode()
	}

	return upstream
}

// Error is a request the proxy answers itself with a registry error, sending
// nothing upstream.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return e.Code + ": " + e.Message
}

func errNotFound() *Error {
	return &Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "not found"}
}

func errDigestInvalid() *Error {
	return &Error{Status: http.StatusBadRequest, Code: "DIGEST_INVALID", Message: "invalid digest"}
}

// Classify maps a request to its route, action and repository. It reads only
// the escaped path, the raw query and the method, and rejects anything that
// isn't exactly one route of the registry API, so that a request can never be
// matched to a repository other than the one the upstream would serve.
func Classify(r *http.Request) (Request, error) {
	if r.URL == nil || r.URL.Opaque != "" {
		return Request{}, errNotFound()
	}

	path := r.URL.EscapedPath()
	// Percent-encoding is rejected outright: no valid route needs it, and
	// decoding it would let one path name two different repositories.
	if len(path) > maxPathLength || strings.Contains(path, "%") || !strings.HasPrefix(path, basePath) {
		return Request{}, errNotFound()
	}

	if path == basePath {
		if !isMethod(r.Method, http.MethodGet, http.MethodHead) {
			return Request{}, errNotFound()
		}

		return Request{Method: r.Method, Route: RouteBase}, nil
	}

	segments := strings.Split(strings.TrimPrefix(path, basePath), "/")

	request, nameSegments, ok := classifyRoute(r.Method, segments)
	if !ok {
		return Request{}, errNotFound()
	}

	name, ok := parseName(nameSegments)
	if !ok {
		return Request{}, errNotFound()
	}

	request.Method = r.Method
	request.Name = name

	if err := classifyQuery(&request, r.URL.RawQuery); err != nil {
		return Request{}, err
	}

	return request, nil
}

// classifyRoute matches the route from the right, returning the classified
// route fields and the segments that make up the name.
func classifyRoute(method string, segments []string) (Request, []string, bool) {
	count := len(segments)
	if count < 3 {
		return Request{}, nil, false
	}

	last, beforeLast := segments[count-1], segments[count-2]

	if count >= 4 && segments[count-3] == "blobs" && (beforeLast == "uploads" || beforeLast == "upload") {
		nameSegments := segments[:count-3]

		if last == "" {
			// Only the plural form starts an upload; GHCR uses the
			// singular form for session URLs alone.
			if beforeLast != "uploads" || method != http.MethodPost {
				return Request{}, nil, false
			}

			return Request{Route: RouteUploadStart, Action: v1alpha1.ActionPush}, nameSegments, true
		}

		if !isMethod(method, http.MethodGet, http.MethodPatch, http.MethodPut, http.MethodDelete) ||
			!validSession(last) {
			return Request{}, nil, false
		}

		return Request{
			Route: RouteUploadSession, Action: v1alpha1.ActionPush, Session: last, uploadSegment: beforeLast,
		}, nameSegments, true
	}

	nameSegments := segments[:count-2]

	switch beforeLast {
	case "manifests":
		if !validReference(last) {
			return Request{}, nil, false
		}

		action, ok := actionFor(method, map[string]v1alpha1.Action{
			http.MethodGet:    v1alpha1.ActionPull,
			http.MethodHead:   v1alpha1.ActionPull,
			http.MethodPut:    v1alpha1.ActionPush,
			http.MethodDelete: v1alpha1.ActionDelete,
		})

		return Request{Route: RouteManifest, Action: action, Reference: last}, nameSegments, ok
	case "blobs":
		blobDigest, err := digest.Parse(last)
		if err != nil {
			return Request{}, nil, false
		}

		action, ok := actionFor(method, map[string]v1alpha1.Action{
			http.MethodGet:    v1alpha1.ActionPull,
			http.MethodHead:   v1alpha1.ActionPull,
			http.MethodDelete: v1alpha1.ActionDelete,
		})

		return Request{Route: RouteBlob, Action: action, Digest: blobDigest}, nameSegments, ok
	case "tags":
		if last != "list" || !isMethod(method, http.MethodGet, http.MethodHead) {
			return Request{}, nil, false
		}

		return Request{Route: RouteTagsList, Action: v1alpha1.ActionPull}, nameSegments, true
	case "referrers":
		subject, err := digest.Parse(last)
		if err != nil || !isMethod(method, http.MethodGet, http.MethodHead) {
			return Request{}, nil, false
		}

		return Request{Route: RouteReferrers, Action: v1alpha1.ActionPull, Digest: subject}, nameSegments, true
	}

	return Request{}, nil, false
}

// classifyQuery keeps only the query parameters the route allows.
func classifyQuery(request *Request, rawQuery string) error {
	// A malformed pair is dropped by ParseQuery; the remaining pairs are what
	// the rules below judge.
	values, _ := url.ParseQuery(rawQuery)

	switch request.Route {
	case RouteUploadStart:
		if digests, present := values["digest"]; present {
			if len(digests) != 1 {
				return errDigestInvalid()
			}

			monolithic, err := digest.Parse(digests[0])
			if err != nil {
				return errDigestInvalid()
			}

			request.monolithicDigest = monolithic
		}

		request.Mount = parseMount(values, request.Name)
	case RouteUploadSession:
		if request.Method != http.MethodPut {
			return nil
		}

		digests := values["digest"]
		if len(digests) != 1 {
			return errDigestInvalid()
		}

		if _, err := digest.Parse(digests[0]); err != nil {
			return errDigestInvalid()
		}
		// The upload session URL is the upstream's own, so its state
		// parameters go back as the client received them.
		request.rawQuery = rawQuery
	case RouteTagsList:
		request.query = keepParameters(values, "n", "last")
	case RouteReferrers:
		request.query = keepParameters(values, "artifactType")
	}

	return nil
}

// parseMount returns the mount of an upload start, or nil if the mount
// parameters are anything but exactly one valid digest and one valid other
// repository name.
func parseMount(values url.Values, name string) *Mount {
	mounts, froms := values["mount"], values["from"]
	if len(mounts) != 1 || len(froms) != 1 {
		return nil
	}

	mountDigest, err := digest.Parse(mounts[0])
	if err != nil {
		return nil
	}

	from, ok := parseName(strings.Split(froms[0], "/"))
	if !ok || from == name {
		return nil
	}

	return &Mount{Digest: mountDigest, From: from}
}

// parseName joins and validates the components of a repository name. Each
// component must match the reference grammar's path component, which admits
// only lower-case letters, digits and single separators, so upper case, empty
// components, "." and ".." are all rejected.
func parseName(components []string) (string, bool) {
	if len(components) == 0 {
		return "", false
	}

	for _, component := range components {
		// A single component holds no "/", so the reference grammar can
		// only read it as a path component, never as a domain.
		if component == "" || strings.Contains(component, "/") {
			return "", false
		}

		if _, err := reference.WithName(component); err != nil {
			return "", false
		}
	}

	name := strings.Join(components, "/")
	if len(name) > maxNameLength {
		return "", false
	}

	return name, true
}

func validReference(ref string) bool {
	if _, err := digest.Parse(ref); err == nil {
		return true
	}
	// Any valid name will do: the tag grammar doesn't depend on it.
	named, err := reference.WithName("tag")
	if err != nil {
		return false
	}

	_, err = reference.WithTag(named, ref)

	return err == nil
}

func validSession(session string) bool {
	return session != "." && session != ".." && sessionPattern.MatchString(session)
}

func actionFor(method string, actions map[string]v1alpha1.Action) (v1alpha1.Action, bool) {
	action, ok := actions[method]
	return action, ok
}

func isMethod(method string, allowed ...string) bool {
	for _, candidate := range allowed {
		if method == candidate {
			return true
		}
	}

	return false
}

func keepParameters(values url.Values, keys ...string) url.Values {
	kept := url.Values{}

	for _, key := range keys {
		if value, present := values[key]; present {
			kept[key] = value
		}
	}

	return kept
}
