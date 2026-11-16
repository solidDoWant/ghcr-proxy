package model

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	toolscache "k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/secrets"
)

// rebuildInterval is the shortest time between two rebuilds.
const rebuildInterval = 100 * time.Millisecond

// SecretStore is the Secret store as the model uses it: it reads Secrets
// and tells the store which Secrets to watch.
type SecretStore interface {
	SecretSource
	SetReferenced(keys []secrets.Key)
}

// Options configure a Model.
type Options struct {
	// Reader lists the four kinds and Namespaces, normally from the
	// informer cache.
	Reader client.Reader
	// Informers, when set, get event handlers that mark the snapshot
	// dirty, and Run waits for them to sync. The caller starts them.
	Informers ctrlcache.Informers
	// Secrets is the Secret store. Its OnChange must call MarkDirty.
	Secrets SecretStore
	// AdminNamespaces may hold UpstreamRegistries.
	AdminNamespaces []string
	// AllowHTTP accepts http URLs; tests only.
	AllowHTTP bool
	// OnRebuild is called with every snapshot after it is published.
	OnRebuild func(*Snapshot)
	// Logger receives list errors.
	Logger *slog.Logger
}

// Model keeps the current snapshot. Readers call Snapshot once per decision
// and use only that snapshot for it.
type Model struct {
	options Options
	logger  *slog.Logger
	current atomic.Pointer[Snapshot]
	synced  atomic.Bool
	dirty   chan struct{}
}

// New returns a model. Call Run to start building snapshots.
func New(options Options) *Model {
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &Model{options: options, logger: logger, dirty: make(chan struct{}, 1)}
}

// MarkDirty asks for a rebuild. It never blocks.
func (m *Model) MarkDirty() {
	select {
	case m.dirty <- struct{}{}:
	default:
	}
}

// Snapshot returns the current snapshot, or nil before the first build.
func (m *Model) Snapshot() *Snapshot { return m.current.Load() }

// Ready reports whether the replica may serve: the informers have synced,
// a snapshot has been built, and no Secret it references is pending.
func (m *Model) Ready() bool {
	snapshot := m.current.Load()
	return m.synced.Load() && snapshot != nil && !snapshot.PendingSecrets()
}

// Run registers the informer handlers, waits for the informers to sync,
// and then rebuilds the snapshot after every change, at most once every
// 100 ms, until ctx is done.
func (m *Model) Run(ctx context.Context) error {
	if m.options.Informers != nil {
		if err := m.watchInformers(ctx); err != nil {
			return err
		}

		if !m.options.Informers.WaitForCacheSync(ctx) {
			return ctx.Err()
		}
	}

	m.synced.Store(true)
	m.MarkDirty()

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-m.dirty:
		}

		m.rebuild(ctx)

		timer.Reset(rebuildInterval)
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
	}
}

func (m *Model) watchInformers(ctx context.Context) error {
	handler := toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { m.MarkDirty() },
		UpdateFunc: func(any, any) { m.MarkDirty() },
		DeleteFunc: func(any) { m.MarkDirty() },
	}

	for _, object := range []client.Object{
		&v1alpha1.UpstreamRegistry{},
		&v1alpha1.UpstreamCredential{},
		&v1alpha1.RegistryMirror{},
		&v1alpha1.RegistryAccessPolicy{},
		&corev1.Namespace{},
	} {
		informer, err := m.options.Informers.GetInformer(ctx, object)
		if err != nil {
			return fmt.Errorf("model: informer for %T: %w", object, err)
		}

		if _, err := informer.AddEventHandler(handler); err != nil {
			return fmt.Errorf("model: event handler for %T: %w", object, err)
		}
	}

	return nil
}

// rebuild lists the inputs, builds a snapshot and publishes it. When a list
// fails the previous snapshot stays, and the rebuild is retried.
func (m *Model) rebuild(ctx context.Context) {
	inputs, err := m.list(ctx)
	if err != nil {
		m.logger.Error("listing model inputs", slog.Any("error", err))
		m.MarkDirty()

		return
	}

	snapshot := Build(inputs)
	if m.options.Secrets != nil {
		m.options.Secrets.SetReferenced(snapshot.Referenced())
	}

	m.current.Store(snapshot)

	if m.options.OnRebuild != nil {
		m.options.OnRebuild(snapshot)
	}
}

func (m *Model) list(ctx context.Context) (Inputs, error) {
	var (
		upstreams   v1alpha1.UpstreamRegistryList
		credentials v1alpha1.UpstreamCredentialList
		mirrors     v1alpha1.RegistryMirrorList
		policies    v1alpha1.RegistryAccessPolicyList
		namespaces  corev1.NamespaceList
	)

	for _, list := range []client.ObjectList{&upstreams, &credentials, &mirrors, &policies, &namespaces} {
		if err := m.options.Reader.List(ctx, list); err != nil {
			return Inputs{}, fmt.Errorf("model: list %T: %w", list, err)
		}
	}

	namespaceLabels := make(map[string]labels.Set, len(namespaces.Items))
	for _, namespace := range namespaces.Items {
		namespaceLabels[namespace.Name] = labels.Set(namespace.Labels)
	}

	inputs := Inputs{
		Upstreams:       upstreams.Items,
		Credentials:     credentials.Items,
		Mirrors:         mirrors.Items,
		Policies:        policies.Items,
		Namespaces:      namespaceLabels,
		AdminNamespaces: m.options.AdminNamespaces,
		AllowHTTP:       m.options.AllowHTTP,
	}
	if m.options.Secrets != nil {
		inputs.Secrets = m.options.Secrets
	}

	return inputs, nil
}
