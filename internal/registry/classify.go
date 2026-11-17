// Package registry classifies registry API requests and forwards them to the
// upstream registry.
package registry

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/uploadtoken"
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
	// RouteUploadSession is /v2/<name>/blobs/uploads/<token>, where <token>
	// is a signed upload session token the proxy issued.
	RouteUploadSession
)

const (
	maxPathLength = 2048
	// maxSessionPathLength is the path limit of the upload session route,
	// whose token is long.
	maxSessionPathLength = 4096
	maxNameLength        = reference.RepositoryNameTotalLengthMax
	basePath             = "/v2/"
)

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
	// Session is the upload session token of an upload session route. The
	// forwarder opens it into the upstream's session URL.
	Session string
	// Mount is set on an upload start with a valid mount. Setting it to nil
	// turns the request into a plain upload start.
	Mount *Mount

	// monolithicDigest is the digest of a monolithic upload start.
	monolithicDigest digest.Digest
	// RawQuery is the client's query on an upload session route. The
	// forwarder adds it to the opened session URL.
	RawQuery string
	// query holds the kept parameters of a route that filters its query.
	query url.Values
}

// UpstreamURL returns the path and query to send upstream, rebuilt from the
// classified fields. The upload session route has none here: its URL is in
// the session token, which only the forwarder can open.
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

func errBlobUploadUnknown() *Error {
	return &Error{Status: http.StatusNotFound, Code: "BLOB_UPLOAD_UNKNOWN", Message: "blob upload unknown"}
}

func errDigestInvalid() *Error {
	return &Error{Status: http.StatusBadRequest, Code: "DIGEST_INVALID", Message: "invalid digest"}
}

// Classify maps a request to its route, action and repository. It reads only
// the raw path, the raw query and the method, refuses a non-canonical path
// with 400 or 404, and rejects anything that
// isn't exactly one route of the registry API, so that a request can never be
// matched to a repository other than the one the upstream would serve.
func Classify(r *http.Request) (Request, error) {
	if r.URL == nil || r.URL.Opaque != "" {
		return Request{}, errNotFound()
	}

	path := requestPath(r)
	if err := checkCanonical(path); err != nil {
		return Request{}, err
	}

	if len(path) > maxSessionPathLength || !strings.HasPrefix(path, basePath) {
		return Request{}, errNotFound()
	}

	if path == basePath {
		if !isMethod(r.Method, http.MethodGet, http.MethodHead) {
			return Request{}, errNotFound()
		}

		return Request{Method: r.Method, Route: RouteBase}, nil
	}

	segments := strings.Split(strings.TrimPrefix(path, basePath), "/")

	request, nameSegments, err := classifyRoute(r.Method, segments)
	if err != nil {
		return Request{}, err
	}

	if request.Route != RouteUploadSession && len(path) > maxPathLength {
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
// route fields and the segments that make up the name. Upload session
// requests whose token doesn't have the token grammar get 404
// BLOB_UPLOAD_UNKNOWN; every other request outside the route table gets 404.
func classifyRoute(method string, segments []string) (Request, []string, error) {
	request, nameSegments, ok := matchRoute(method, segments)
	if !ok {
		return Request{}, nil, errNotFound()
	}

	if request.Route == RouteUploadSession && !uploadtoken.Valid(request.Session) {
		return Request{}, nil, errBlobUploadUnknown()
	}

	return request, nameSegments, nil
}

func matchRoute(method string, segments []string) (Request, []string, bool) {
	count := len(segments)
	if count < 3 {
		return Request{}, nil, false
	}

	last, beforeLast := segments[count-1], segments[count-2]

	if count >= 4 && segments[count-3] == "blobs" && beforeLast == "uploads" {
		nameSegments := segments[:count-3]

		if last == "" {
			if method != http.MethodPost {
				return Request{}, nil, false
			}

			return Request{Route: RouteUploadStart, Action: v1alpha1.ActionPush}, nameSegments, true
		}

		if !isMethod(method, http.MethodGet, http.MethodPatch, http.MethodPut, http.MethodDelete) {
			return Request{}, nil, false
		}

		return Request{Route: RouteUploadSession, Action: v1alpha1.ActionPush, Session: last}, nameSegments, true
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
		if digests, present := values["digest"]; present {
			if len(digests) != 1 {
				return errDigestInvalid()
			}

			if _, err := digest.Parse(digests[0]); err != nil {
				return errDigestInvalid()
			}
		}
		// Which parameters a session request may carry depends on the
		// session URL in the token, so the forwarder checks them.
		request.RawQuery = rawQuery
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
