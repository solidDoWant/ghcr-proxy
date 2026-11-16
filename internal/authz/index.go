package authz

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	"github.com/solidDoWant/ghcr-proxy/api/v1alpha1"
	"github.com/solidDoWant/ghcr-proxy/internal/config"
)

// uncapped is a grant that contains every pattern and allows every action. A policy's effective policy under it is
// what the policy asks for, which tells a denial by the grant apart from a denial for want of a policy.
var uncapped = []config.ExpandedGrant{{
	Patterns: []string{"**"},
	Actions:  []config.Action{config.ActionPull, config.ActionPush, config.ActionDelete},
}}

type indexedPolicy struct {
	name            string
	serviceAccounts []string
	effective       EffectivePolicy
	requested       EffectivePolicy
}

// Index holds every GHCRAccessPolicy's effective policy under the configured grants, by namespace. It is fed by an
// informer, so a policy change applies to the next decision without a restart. It never reads status.
type Index struct {
	grants config.Grants

	mu          sync.RWMutex
	byNamespace map[string]map[string]indexedPolicy

	synced atomic.Pointer[func() bool]
}

// NewIndex returns an empty index that caps policies with grants. It is not synced until Watch registers it with an
// informer and that informer has delivered its initial list.
func NewIndex(grants config.Grants) *Index {
	return &Index{grants: grants, byNamespace: map[string]map[string]indexedPolicy{}}
}

// Watch feeds the index from the GHCRAccessPolicy informer of informers.
func (i *Index) Watch(ctx context.Context, informers cache.Informers) error {
	informer, err := informers.GetInformer(ctx, &v1alpha1.GHCRAccessPolicy{})
	if err != nil {
		return fmt.Errorf("get GHCRAccessPolicy informer: %w", err)
	}

	registration, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { i.upsert(obj) },
		UpdateFunc: func(_, obj any) { i.upsert(obj) },
		DeleteFunc: i.delete,
	})
	if err != nil {
		return fmt.Errorf("watch GHCRAccessPolicy informer: %w", err)
	}

	hasSynced := registration.HasSynced
	i.synced.Store(&hasSynced)

	return nil
}

// Synced reports whether the index has seen every policy that existed when it started watching.
func (i *Index) Synced() bool {
	hasSynced := i.synced.Load()

	return hasSynced != nil && (*hasSynced)()
}

// Counts returns how many indexed policies are inside their grants (Ready) and how many are not.
func (i *Index) Counts() (ready, notReady int) {
	i.mu.RLock()
	defer i.mu.RUnlock()

	for _, policies := range i.byNamespace {
		for _, policy := range policies {
			if policy.effective.Ready() {
				ready++
			} else {
				notReady++
			}
		}
	}

	return ready, notReady
}

// set adds or replaces a policy.
func (i *Index) set(policy *v1alpha1.GHCRAccessPolicy) {
	entry := indexedPolicy{
		name:            policy.Name,
		serviceAccounts: slices.Clone(policy.Spec.ServiceAccounts),
		effective:       Effective(policy, i.grants.For(policy.Namespace)),
		requested:       Effective(policy, uncapped),
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	policies := i.byNamespace[policy.Namespace]
	if policies == nil {
		policies = map[string]indexedPolicy{}
		i.byNamespace[policy.Namespace] = policies
	}

	policies[policy.Name] = entry
}

// remove drops a policy.
func (i *Index) remove(namespace, name string) {
	i.mu.Lock()
	defer i.mu.Unlock()

	delete(i.byNamespace[namespace], name)

	if len(i.byNamespace[namespace]) == 0 {
		delete(i.byNamespace, namespace)
	}
}

func (i *Index) upsert(obj any) {
	if policy, ok := obj.(*v1alpha1.GHCRAccessPolicy); ok {
		i.set(policy)
	}
}

func (i *Index) delete(obj any) {
	if tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}

	if policy, ok := obj.(*v1alpha1.GHCRAccessPolicy); ok {
		i.remove(policy.Namespace, policy.Name)
	}
}

// match is the result of looking up one (namespace, ServiceAccount, repository, action).
type match struct {
	// policy is the name of a policy whose effective policy allows the action, empty if none does.
	policy string
	// requested is set when some policy listing the ServiceAccount asks for the action, granted or not.
	requested bool
}

// lookup finds a policy in namespace that lists serviceAccount and allows action on name. Policies are tried in name
// order, so the reported policy is the same on every replica.
func (i *Index) lookup(namespace, serviceAccount, name string, action v1alpha1.Action) match {
	i.mu.RLock()
	defer i.mu.RUnlock()

	policies := i.byNamespace[namespace]
	names := make([]string, 0, len(policies))

	for policyName := range policies {
		names = append(names, policyName)
	}

	slices.Sort(names)

	var out match

	for _, policyName := range names {
		policy := policies[policyName]
		if !slices.Contains(policy.serviceAccounts, serviceAccount) {
			continue
		}

		if policy.effective.Allows(name, action) {
			return match{policy: policy.name, requested: true}
		}

		if policy.requested.Allows(name, action) {
			out.requested = true
		}
	}

	return out
}
