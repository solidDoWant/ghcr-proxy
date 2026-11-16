package registry

import (
	"net/http"
	"strings"
)

// requestPath is the path of the request target exactly as the client sent it. Nothing in front of the handler
// cleans or decodes it, so the checks below judge the bytes the client chose.
func requestPath(r *http.Request) string {
	if strings.HasPrefix(r.RequestURI, "/") {
		path, _, _ := strings.Cut(r.RequestURI, "?")
		return path
	}

	return r.URL.EscapedPath()
}

// checkCanonical refuses a path that is not in the one canonical form the route table matches. Registry paths
// never need percent-encoding, empty segments or dot segments, and an upstream or a path-cleaning hop could read
// such a path as a different repository than the proxy decided on, so they are refused before classification.
// A single trailing "/" is allowed here; the route table decides where it may appear.
func checkCanonical(path string) *Error {
	if !strings.HasPrefix(path, "/") {
		return errNotFound()
	}

	invalid := errUnsupported()
	if strings.HasPrefix(path, basePath) {
		// Below /v2/ every segment is part of a repository name or a route of that name.
		invalid = errNameInvalid()
	}

	if strings.Contains(path, "%") {
		return invalid
	}

	segments := strings.Split(strings.TrimSuffix(path[1:], "/"), "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			if path == "/" {
				return errNotFound()
			}

			return invalid
		}
	}

	switch {
	case segments[0] == "v2":
		return nil
	case strings.EqualFold(segments[0], "v2"):
		return invalid
	default:
		return errNotFound()
	}
}

func errNameInvalid() *Error {
	return &Error{Status: http.StatusBadRequest, Code: "NAME_INVALID", Message: "invalid repository name"}
}

func errUnsupported() *Error {
	return &Error{Status: http.StatusBadRequest, Code: "UNSUPPORTED", Message: "path is not canonical"}
}
