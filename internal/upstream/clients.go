package upstream

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/registrygate/internal/model"
)

// Clients keeps one Client per UpstreamRegistry, keyed by UID. A client is
// rebuilt when the object's generation or its CA Secret's resourceVersion
// changes, so a new URL, CA, realm list or timeout applies to the next
// request.
type Clients struct {
	options ClientOptions

	mu      sync.Mutex
	clients map[types.UID]cachedClient
}

type cachedClient struct {
	generation int64
	caVersion  string
	client     *Client
}

// NewClients returns an empty client cache.
func NewClients(options ClientOptions) *Clients {
	return &Clients{options: options, clients: map[types.UID]cachedClient{}}
}

// Get returns the client for the ready upstream u, building it when there
// is none or u has changed since.
func (c *Clients) Get(u *model.Upstream) (*Client, error) {
	uid := u.Object.UID

	c.mu.Lock()
	defer c.mu.Unlock()

	if cached, ok := c.clients[uid]; ok &&
		cached.generation == u.Object.Generation && cached.caVersion == u.CAResourceVersion {
		return cached.client, nil
	}

	client, err := NewClient(u, c.options)
	if err != nil {
		return nil, err
	}

	c.clients[uid] = cachedClient{generation: u.Object.Generation, caVersion: u.CAResourceVersion, client: client}

	return client, nil
}

// Retain drops the clients of upstreams that keep reports false for.
func (c *Clients) Retain(keep func(types.UID) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for uid, cached := range c.clients {
		if !keep(uid) {
			cached.client.transport.CloseIdleConnections()
			delete(c.clients, uid)
		}
	}
}
