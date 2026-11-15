package fakeghcr

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// grant is what one issued token allows.
type grant struct {
	cred    Credential
	expires time.Time
	// scopes maps a repository to the actions the token allows on it.
	scopes map[string]map[string]bool
}

type tokenStore struct {
	mu     sync.Mutex
	tokens map[string]*grant
}

func (s *tokenStore) issue(g *grant) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}

	token := hex.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.tokens == nil {
		s.tokens = map[string]*grant{}
	}

	s.tokens[token] = g

	return token, nil
}

func (s *tokenStore) lookup(token string) *grant {
	s.mu.Lock()
	defer s.mu.Unlock()

	g := s.tokens[token]
	if g == nil || time.Now().After(g.expires) {
		return nil
	}

	return g
}

// requestScope derives the repository and action a registry request needs
// from its method and path alone. ok is false for paths outside the
// repository API (including /v2/ itself, which needs only a valid token).
func requestScope(method, path string) (repo, action string, ok bool) {
	rest, found := strings.CutPrefix(path, "/v2/")
	if !found {
		return "", "", false
	}

	cut := -1
	for _, marker := range []string{"/blobs/", "/manifests/", "/tags/", "/referrers/"} {
		if i := strings.LastIndex(rest, marker); i > cut {
			cut = i
		}
	}

	if cut <= 0 {
		return "", "", false
	}

	switch method {
	case http.MethodGet, http.MethodHead:
		action = "pull"
	case http.MethodDelete:
		action = "delete"
	default:
		action = "push"
	}

	return rest[:cut], action, true
}

func writeUnauthorized(resp http.ResponseWriter, challenge string) {
	resp.Header().Set("WWW-Authenticate", challenge)
	resp.Header().Set("Content-Type", "application/json")
	resp.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(resp).Encode(map[string]any{
		"errors": []map[string]string{{"code": "UNAUTHORIZED", "message": "authentication required"}},
	})
}

// credentialOf classifies the request's credential without judging whether it
// is enough.
func (f *Fake) credentialOf(req *http.Request) (Credential, *grant) {
	if token, found := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer "); found {
		if g := f.tokens.lookup(token); g != nil {
			return g.cred, g
		}

		return CredNone, nil
	}

	if f.basicIsPAT(req) {
		return CredPATBasic, nil
	}

	return CredNone, nil
}

func (f *Fake) basicIsPAT(req *http.Request) bool {
	_, password, ok := req.BasicAuth()
	return ok && subtle.ConstantTimeCompare([]byte(password), []byte(f.pat)) == 1
}

func (f *Fake) challenge(scope string) string {
	challenge := fmt.Sprintf("Bearer realm=%q,service=\"ghcr.io\"", f.Realm())
	if scope != "" {
		challenge += fmt.Sprintf(",scope=%q", scope)
	}

	return challenge
}

// authorize wraps the registry: every request needs a bearer token whose
// scopes cover the repository and action the request needs.
func (f *Fake) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		repo, action, scoped := requestScope(req.Method, req.URL.Path)
		scope := ""

		if scoped {
			actions := action
			if action == "push" {
				actions = "pull,push"
			}

			scope = "repository:" + repo + ":" + actions
		}

		_, g := f.credentialOf(req)
		if g == nil || (scoped && !g.scopes[repo][action]) {
			writeUnauthorized(resp, f.challenge(scope))
			return
		}

		next.ServeHTTP(resp, req)
	})
}

// tokenEndpoint issues tokens: Basic with the PAT grants every requested
// scope, anonymous requests get pull on public repositories only.
func (f *Fake) tokenEndpoint(resp http.ResponseWriter, req *http.Request) {
	patAuth := f.basicIsPAT(req)
	if _, _, hasBasic := req.BasicAuth(); hasBasic && !patAuth {
		writeUnauthorized(resp, f.challenge(""))
		return
	}

	g := &grant{cred: CredAnonymousToken, expires: time.Now().Add(f.ttl), scopes: map[string]map[string]bool{}}
	if patAuth {
		g.cred = CredPATToken
	}

	for _, scope := range req.URL.Query()["scope"] {
		parts := strings.Split(scope, ":")
		if len(parts) != 3 || parts[0] != "repository" {
			writeUnauthorized(resp, f.challenge(""))
			return
		}

		repo := parts[1]
		for _, action := range strings.Split(parts[2], ",") {
			if !patAuth && (action != "pull" || !f.isPublic(repo)) {
				writeUnauthorized(resp, f.challenge(scope))
				return
			}

			if g.scopes[repo] == nil {
				g.scopes[repo] = map[string]bool{}
			}

			g.scopes[repo][action] = true
		}
	}

	token, err := f.tokens.issue(g)
	if err != nil {
		http.Error(resp, "token generation failed", http.StatusInternalServerError)
		return
	}

	expiresIn := max(int(f.ttl/time.Second), 1)

	resp.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(resp).Encode(map[string]any{"token": token, "expires_in": expiresIn})
}
