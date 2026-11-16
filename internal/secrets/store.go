package secrets

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// Retry timing for a Secret watch that fails, for example because the
// tenant has not created the Role yet: 1 s, doubling, at most 5 minutes.
const (
	retryInitial = time.Second
	retryMax     = 5 * time.Minute
)

// Options configure a Store.
type Options struct {
	// AdminNamespaces get one Secret informer each. Every other namespace
	// is read one Secret at a time.
	AdminNamespaces []string
	// OnChange is called, without the store's lock held, after any Secret
	// the store tracks changes state, data or resource version.
	OnChange func()
	// Logger receives watch errors. It never receives Secret values.
	Logger *slog.Logger
}

// Store tracks Secrets. Admin namespaces are watched as a whole from Run;
// other Secrets are watched while SetReferenced names them.
type Store struct {
	client   kubernetes.Interface
	onChange func()
	logger   *slog.Logger

	mutex   sync.Mutex
	ctx     context.Context // set by Run; nil until then
	admin   map[string]*adminNamespace
	watches map[Key]*secretWatch
	wanted  map[Key]struct{}
}

// adminNamespace is the informer for one admin namespace.
type adminNamespace struct {
	informer  cache.SharedIndexInformer
	forbidden bool // guarded by Store.mutex
}

// secretWatch is the narrowed watch for one Secret outside the admin
// namespaces.
type secretWatch struct {
	cancel context.CancelFunc
	secret Secret // guarded by Store.mutex
}

// New returns a store. Call Run to start it.
func New(client kubernetes.Interface, options Options) *Store {
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	onChange := options.OnChange
	if onChange == nil {
		onChange = func() {}
	}
	store := &Store{
		client:   client,
		onChange: onChange,
		logger:   logger,
		admin:    map[string]*adminNamespace{},
		watches:  map[Key]*secretWatch{},
		wanted:   map[Key]struct{}{},
	}
	for _, namespace := range options.AdminNamespaces {
		store.admin[namespace] = &adminNamespace{}
	}
	return store
}

// Run starts the admin namespace informers and the watches SetReferenced
// asked for, and blocks until ctx is done, when every watch stops.
func (s *Store) Run(ctx context.Context) error {
	s.mutex.Lock()
	s.ctx = ctx
	for namespace, admin := range s.admin {
		admin.informer = s.newAdminInformer(namespace, admin)
		go admin.informer.RunWithContext(ctx)
	}
	s.reconcileLocked()
	s.mutex.Unlock()

	<-ctx.Done()

	s.mutex.Lock()
	for key, watch := range s.watches {
		watch.cancel()
		delete(s.watches, key)
	}
	s.mutex.Unlock()
	return nil
}

// SetReferenced replaces the set of Secrets outside the admin namespaces
// that the store watches. A new key starts a watch; a key that is no longer
// in the set stops its watch and is forgotten. Keys in admin namespaces are
// ignored: their informers already cover them.
func (s *Store) SetReferenced(keys []Key) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.wanted = map[Key]struct{}{}
	for _, key := range keys {
		if _, isAdmin := s.admin[key.Namespace]; isAdmin {
			continue
		}
		s.wanted[key] = struct{}{}
	}
	s.reconcileLocked()
}

// Watched returns the Secrets that currently have their own watch, sorted.
func (s *Store) Watched() []Key {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return slices.SortedFunc(maps.Keys(s.watches), func(a, b Key) int {
		if a.Namespace != b.Namespace {
			if a.Namespace < b.Namespace {
				return -1
			}
			return 1
		}
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
}

// Get returns what the store knows about a Secret. The boolean is false
// when the store does not track the Secret: it is outside the admin
// namespaces and SetReferenced does not name it.
func (s *Store) Get(key Key) (Secret, bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if admin, isAdmin := s.admin[key.Namespace]; isAdmin {
		return s.getAdminLocked(admin, key), true
	}
	if _, wanted := s.wanted[key]; !wanted {
		return Secret{}, false
	}
	watch, ok := s.watches[key]
	if !ok {
		// Referenced before Run started the watch.
		return Secret{State: Pending}, true
	}
	return watch.secret, true
}

func (s *Store) getAdminLocked(admin *adminNamespace, key Key) Secret {
	if admin.informer == nil || !admin.informer.HasSynced() {
		if admin.forbidden {
			return Secret{State: Forbidden}
		}
		return Secret{State: Pending}
	}
	object, exists, err := admin.informer.GetIndexer().GetByKey(key.String())
	if err != nil || !exists {
		return Secret{State: NotFound}
	}
	secret, ok := object.(*corev1.Secret)
	if !ok {
		return Secret{State: NotFound}
	}
	return presentSecret(secret)
}

// reconcileLocked starts and stops watches so they match s.wanted. It does
// nothing before Run.
func (s *Store) reconcileLocked() {
	if s.ctx == nil {
		return
	}
	for key, watch := range s.watches {
		if _, wanted := s.wanted[key]; !wanted {
			watch.cancel()
			delete(s.watches, key)
		}
	}
	for key := range s.wanted {
		if _, running := s.watches[key]; running {
			continue
		}
		ctx, cancel := context.WithCancel(s.ctx)
		watch := &secretWatch{cancel: cancel, secret: Secret{State: Pending}}
		s.watches[key] = watch
		go s.runSecretWatch(ctx, key, watch)
	}
}

func (s *Store) newAdminInformer(namespace string, admin *adminNamespace) cache.SharedIndexInformer {
	secrets := s.client.CoreV1().Secrets(namespace)
	listWatch := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			list, err := secrets.List(ctx, options)
			s.setAdminForbidden(admin, err)
			return list, err
		},
		WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
			return secrets.Watch(ctx, options)
		},
	}
	informer := cache.NewSharedIndexInformer(listWatch, &corev1.Secret{}, 0, cache.Indexers{})
	_ = informer.SetWatchErrorHandlerWithContext(func(_ context.Context, _ *cache.Reflector, err error) {
		s.logger.Warn("watching Secrets failed", "namespace", namespace, "error", err)
	})
	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { s.onChange() },
		UpdateFunc: func(any, any) { s.onChange() },
		DeleteFunc: func(any) { s.onChange() },
	})
	return informer
}

// setAdminForbidden records whether listing an admin namespace was refused.
func (s *Store) setAdminForbidden(admin *adminNamespace, err error) {
	forbidden := apierrors.IsForbidden(err)
	s.mutex.Lock()
	changed := admin.forbidden != forbidden
	admin.forbidden = forbidden
	s.mutex.Unlock()
	if changed {
		s.onChange()
	}
}

// runSecretWatch lists and watches one Secret by name until ctx is done,
// retrying failures with backoff. A 403 marks the Secret Forbidden; the
// retries pick it up once a Role allows the read.
func (s *Store) runSecretWatch(ctx context.Context, key Key, tracked *secretWatch) {
	selector := fields.OneTermEqualSelector("metadata.name", key.Name).String()
	secrets := s.client.CoreV1().Secrets(key.Namespace)
	failed := func(err error) {
		if apierrors.IsForbidden(err) {
			s.update(tracked, Secret{State: Forbidden})
			return
		}
		if err != nil && ctx.Err() == nil {
			s.logger.Warn("watching a Secret failed", "secret", key.String(), "error", err)
		}
	}
	listWatch := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			options.FieldSelector = selector
			list, err := secrets.List(ctx, options)
			failed(err)
			return list, err
		},
		WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
			options.FieldSelector = selector
			result, err := secrets.Watch(ctx, options)
			failed(err)
			return result, err
		},
	}
	reflector := cache.NewReflectorWithOptions(listWatch, &corev1.Secret{}, &secretSink{store: s, watch: tracked}, cache.ReflectorOptions{
		Name:            "secret " + key.String(),
		TypeDescription: "*v1.Secret",
		Backoff: &wait.Backoff{
			Duration: retryInitial,
			Factor:   2,
			Cap:      retryMax,
			// Steps must outlast the doubling so the delay reaches the cap
			// and stays there.
			Steps: 1 << 30,
		},
	})
	reflector.RunWithContext(ctx)
}

// update sets a watched Secret's state and reports a change. It ignores
// watches that SetReferenced has already dropped.
func (s *Store) update(watch *secretWatch, secret Secret) {
	s.mutex.Lock()
	current := watch.secret
	watch.secret = secret
	s.mutex.Unlock()
	if current.State != secret.State || current.ResourceVersion != secret.ResourceVersion {
		s.onChange()
	}
}

func presentSecret(secret *corev1.Secret) Secret {
	data := make(Data, len(secret.Data))
	for name, value := range secret.Data {
		data[name] = slices.Clone(value)
	}
	return Secret{State: Present, Data: data, ResourceVersion: secret.ResourceVersion}
}

// secretSink receives the reflector's view of one Secret. The field
// selector means it never holds more than one object.
type secretSink struct {
	store *Store
	watch *secretWatch
}

func (k *secretSink) Add(object any) error    { return k.set(object) }
func (k *secretSink) Update(object any) error { return k.set(object) }
func (k *secretSink) Delete(any) error {
	k.store.update(k.watch, Secret{State: NotFound})
	return nil
}

func (k *secretSink) Replace(objects []any, _ string) error {
	if len(objects) == 0 {
		k.store.update(k.watch, Secret{State: NotFound})
		return nil
	}
	return k.set(objects[len(objects)-1])
}

func (k *secretSink) Resync() error { return nil }

func (k *secretSink) set(object any) error {
	secret, ok := object.(*corev1.Secret)
	if !ok {
		return nil
	}
	k.store.update(k.watch, presentSecret(secret))
	return nil
}
