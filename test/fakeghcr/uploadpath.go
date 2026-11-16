package fakeghcr

import (
	"net/http"
	"regexp"
)

// The real ghcr.io answers an upload start with a session URL of the form
// /v2/<name>/blobs/upload/<session> (singular "upload"), while the registry
// the fake wraps uses the standard plural form. These patterns translate
// between the two so clients of the fake see GHCR's form.
var (
	singularSessionPath = regexp.MustCompile(`^(/v2/.+/blobs/)upload/([^/]+)$`)
	pluralSessionPath   = regexp.MustCompile(`^(/v2/.+/blobs/)uploads/([^/?]+)`)
)

// ghcrUploadPaths serves GHCR's singular upload session paths with next and
// rewrites the upload session Location headers next returns to that form.
func ghcrUploadPaths(next http.Handler) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		if singularSessionPath.MatchString(req.URL.Path) {
			// A copy, so the request log keeps the path the client sent.
			req = req.Clone(req.Context())
			req.URL.Path = singularSessionPath.ReplaceAllString(req.URL.Path, "${1}uploads/${2}")
			req.URL.RawPath = ""
		}

		next.ServeHTTP(&singularLocationWriter{ResponseWriter: resp}, req)
	})
}

// singularLocationWriter rewrites an upload session Location header to
// GHCR's singular form before the header is written.
type singularLocationWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *singularLocationWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		w.wroteHeader = true

		if loc := w.Header().Get("Location"); loc != "" {
			w.Header().Set("Location", pluralSessionPath.ReplaceAllString(loc, "${1}upload/${2}"))
		}
	}

	w.ResponseWriter.WriteHeader(status)
}

func (w *singularLocationWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}

	return w.ResponseWriter.Write(data)
}

func (w *singularLocationWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
