package fakeregistry

import (
	"net/http"
	"sort"
	"sync"
)

// Credential is the kind of credential a logged request carried.
type Credential string

const (
	// CredNone is a request with no usable credential (none, unknown or expired).
	CredNone Credential = "none"
	// CredAnonymousToken is a bearer token granted without the PAT.
	CredAnonymousToken Credential = "anonymous-token"
	// CredPATToken is a bearer token granted to the PAT.
	CredPATToken Credential = "pat-token"
	// CredPATBasic is HTTP Basic with the PAT, as sent to the token endpoint or, with Basic auth, to the registry.
	CredPATBasic Credential = "pat-basic"
)

// Entry is one request the fake saw. It holds no header values or query values.
type Entry struct {
	// Server is "registry" or "storage".
	Server string
	Method string
	// Path is the decoded path; EscapedPath is the path as the client sent it.
	Path        string
	EscapedPath string
	QueryKeys   []string
	Credential  Credential
	Status      int
}

type requestLog struct {
	mu      sync.Mutex
	entries []Entry
}

func (l *requestLog) add(entry Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.entries = append(l.entries, entry)
}

func (l *requestLog) snapshot() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]Entry(nil), l.entries...)
}

func (l *requestLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.entries = nil
}

func queryKeys(req *http.Request) []string {
	var keys []string
	for key := range req.URL.Query() {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

// statusRecorder captures the status code and rewrites relative Location
// headers to absolute registry URLs.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	absoluteTo  string
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}

	r.wroteHeader = true

	r.status = status
	if r.absoluteTo != "" {
		if loc := r.Header().Get("Location"); len(loc) > 0 && loc[0] == '/' {
			r.Header().Set("Location", r.absoluteTo+loc)
		}
	}

	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}

	return r.ResponseWriter.Write(data)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
