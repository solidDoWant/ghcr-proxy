package mirror

import (
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/registrygate/internal/metrics"
	"github.com/solidDoWant/registrygate/internal/model"
)

const (
	defaultResponseHeaderTimeout = 30 * time.Second
	defaultTokenTimeout          = 10 * time.Second
)

// ClientOptions are the settings a Client takes from the proxy rather than
// from its RegistryMirror.
type ClientOptions struct {
	Logger *slog.Logger
	// Metrics counts token fetches; nil turns it off.
	Metrics *metrics.Metrics
}

// staticState is a StateSource that always returns the same state.
type staticState struct{ state State }

func (s staticState) Get() (State, bool) { return s.state, true }

// NewClientFromMirror returns a client for a ready mirror of a snapshot. The
// client holds only the mirror's own credential and CA.
func NewClientFromMirror(mirror *model.Mirror, options ClientOptions) (*Client, error) {
	spec := mirror.Object.Spec

	responseHeader, token := defaultResponseHeaderTimeout, defaultTokenTimeout
	if spec.Timeouts.ResponseHeader != nil && spec.Timeouts.ResponseHeader.Duration > 0 {
		responseHeader = spec.Timeouts.ResponseHeader.Duration
	}

	if spec.Timeouts.Token != nil && spec.Timeouts.Token.Duration > 0 {
		token = spec.Timeouts.Token.Duration
	}

	state := State{RootCAs: mirror.CAs}
	if mirror.Credentials != nil {
		state.Credentials = &Credentials{Username: mirror.Credentials.Username, Password: mirror.Credentials.Password}
	}

	return newClient(ClientConfig{
		Mirror: &Settings{
			URL:                   mirror.URL,
			PathPrefix:            spec.PathPrefix,
			FallbackToUpstream:    mirror.FallbackToUpstream,
			ResponseHeaderTimeout: responseHeader,
			TokenRealmHosts:       spec.TokenRealmHosts,
			AliasHosts:            spec.AliasHosts,
		},
		TokenTimeout: token,
		Logger:       options.Logger,
		Metrics:      options.Metrics,
		Upstream:     upstreamName(mirror),
	}, staticState{state: state})
}

// Cache holds one Client per mirror UID. A client is rebuilt when the mirror's
// generation, its upstream's allowedMirrorURLs or the resource version of a
// Secret it used changes.
type Cache struct {
	options ClientOptions

	mu      sync.Mutex
	entries map[types.UID]cacheEntry
}

type cacheEntry struct {
	fingerprint string
	client      *Client
}

// NewCache returns an empty cache.
func NewCache(options ClientOptions) *Cache {
	return &Cache{options: options, entries: map[types.UID]cacheEntry{}}
}

// Client returns the client for mirror, building it when none is cached or
// the cached one is out of date.
func (c *Cache) Client(mirror *model.Mirror) (*Client, error) {
	fingerprint := fingerprintOf(mirror)
	uid := mirror.Object.UID

	c.mu.Lock()
	defer c.mu.Unlock()

	if entry, ok := c.entries[uid]; ok && entry.fingerprint == fingerprint {
		return entry.client, nil
	}

	client, err := NewClientFromMirror(mirror, c.options)
	if err != nil {
		return nil, err
	}

	c.entries[uid] = cacheEntry{fingerprint: fingerprint, client: client}

	return client, nil
}

// Retain drops the clients of mirrors that are not in live.
func (c *Cache) Retain(live map[types.UID]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for uid := range c.entries {
		if _, ok := live[uid]; !ok {
			delete(c.entries, uid)
		}
	}
}

func fingerprintOf(mirror *model.Mirror) string {
	var allowed []string
	if mirror.Upstream != nil {
		allowed = mirror.Upstream.Object.Spec.AllowedMirrorURLs
	}

	// Control characters cannot appear in a URL or a resource version, so parts can't run together.
	return strings.Join([]string{
		strconv.FormatInt(mirror.Object.Generation, 10),
		strings.Join(allowed, "\x00"),
		mirror.SecretResourceVersion,
		mirror.CAResourceVersion,
	}, "\x01")
}

// RequestURL returns the mirror URL that serves path, a client request path
// beginning with /v2/: `<url path>/v2/<pathPrefix>/<name>/…`.
func (c *Client) RequestURL(path, rawQuery string) *url.URL {
	base := c.cfg.Mirror.URL

	return &url.URL{
		Scheme:   base.Scheme,
		Host:     base.Host,
		Path:     strings.TrimSuffix(base.Path, "/") + "/v2/" + c.cfg.Mirror.PathPrefix + "/" + strings.TrimPrefix(path, "/v2/"),
		RawQuery: rawQuery,
	}
}

// upstreamName is the mirror's upstream as "<namespace>/<name>", or metrics.NoUpstream when it has none.
func upstreamName(mirror *model.Mirror) string {
	if mirror.Upstream == nil {
		return metrics.NoUpstream
	}

	return mirror.Upstream.Name().String()
}
