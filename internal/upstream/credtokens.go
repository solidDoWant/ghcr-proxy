package upstream

import (
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

// Credential is what a request is authenticated with upstream. The zero
// value is anonymous. It hides its values from fmt and slog.
type Credential struct {
	// ID identifies the credential, normally the UpstreamCredential's UID.
	// Empty means anonymous.
	ID string
	// Version is the version of the values, normally the Secret's
	// resourceVersion. Tokens fetched with another version are never used.
	Version  string
	Username string
	Password string
}

// Anonymous reports whether the credential is the anonymous one.
func (c Credential) Anonymous() bool { return c.ID == "" }

// String hides the values.
func (Credential) String() string { return redactedValue }

// GoString hides the values from %#v.
func (Credential) GoString() string { return redactedValue }

// LogValue hides the values from slog.
func (Credential) LogValue() slog.Value { return slog.StringValue(redactedValue) }

const redactedValue = "[redacted]"

// credentialToken is a token in the cache, with the key it is stored under
// so it can be dropped.
type credentialToken struct {
	value string
	key   tokenKey
}

type tokenKey struct {
	id      string
	version string
	scopes  string
}

type tokenEntry struct {
	value      string
	validUntil time.Time
}

// tokenCache keeps bearer tokens per credential version and scope set. A
// credential's entries go when a newer version of it is used.
type tokenCache struct {
	mu      sync.Mutex
	entries *lru.Cache[tokenKey, tokenEntry]
	// versions is the latest version seen of each credential ID.
	versions map[string]string
	now      func() time.Time
}

func newTokenCache(now func() time.Time) *tokenCache {
	entries, err := lru.New[tokenKey, tokenEntry](maxCachedTokens)
	if err != nil {
		// lru.New fails only for a non-positive size.
		panic(err)
	}

	return &tokenCache{entries: entries, versions: map[string]string{}, now: now}
}

func keyOf(cred Credential, scopes []string) tokenKey {
	sorted := slices.Clone(scopes)
	slices.Sort(sorted)

	return tokenKey{id: cred.ID, version: cred.Version, scopes: strings.Join(sorted, " ")}
}

// get returns the cached token for key, evicting the entries of older
// versions of its credential first.
func (c *tokenCache) get(key tokenKey) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if latest, seen := c.versions[key.id]; !seen || latest != key.version {
		c.evictLocked(key.id)
		c.versions[key.id] = key.version

		return "", false
	}

	entry, ok := c.entries.Get(key)
	if !ok {
		return "", false
	}

	if !c.now().Before(entry.validUntil) {
		c.entries.Remove(key)
		return "", false
	}

	return entry.value, true
}

// put stores a token that the endpoint said lasts expiresIn seconds (0 when
// it gave no lifetime), unless a newer version of its credential is in use.
func (c *tokenCache) put(key tokenKey, value string, expiresIn int) {
	lifetime := defaultTokenLifetime
	if expiresIn > 0 {
		lifetime = time.Duration(expiresIn) * time.Second
	}

	validUntil := c.now().Add(lifetime - expiryMargin)

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.versions[key.id] != key.version || !c.now().Before(validUntil) {
		return
	}

	c.entries.Add(key, tokenEntry{value: value, validUntil: validUntil})
}

// drop removes the token, if it is still the cached one.
func (c *tokenCache) drop(token credentialToken) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if entry, ok := c.entries.Peek(token.key); ok && entry.value == token.value {
		c.entries.Remove(token.key)
	}
}

func (c *tokenCache) evictLocked(id string) {
	for _, key := range c.entries.Keys() {
		if key.id == id {
			c.entries.Remove(key)
		}
	}
}
