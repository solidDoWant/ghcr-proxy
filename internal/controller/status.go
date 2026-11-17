// Package controller writes the status of the four registrygate kinds from the model's snapshot. It runs only
// on the elected leader; serving never depends on it. It holds no readiness logic: the snapshot already says
// what serving does.
package controller

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/model"
)

// LeaseName is the leader-election lease of the status controller.
const LeaseName = "registrygate-status"

// ManagerOptions returns the manager options for the proxy: leader election on the LeaseName lease in
// leaseNamespace, released when the manager stops so another replica takes over at once, and no metrics or probe
// listeners of the manager's own (the proxy serves those).
func ManagerOptions(scheme *runtime.Scheme, leaseNamespace string) ctrl.Options {
	return ctrl.Options{
		Scheme:                        scheme,
		LeaderElection:                true,
		LeaderElectionID:              LeaseName,
		LeaderElectionNamespace:       leaseNamespace,
		LeaderElectionReleaseOnCancel: true,
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:        "0",
	}
}

// Status writes the computed status of the four kinds. Pass Notify to the model as its OnRebuild callback and
// call SetupWithManager once per manager.
type Status struct {
	// Snapshot returns the model's current snapshot, or nil before the first build.
	Snapshot func() *model.Snapshot

	trigger chan struct{}
	once    sync.Once
}

func (s *Status) init() { s.once.Do(func() { s.trigger = make(chan struct{}, 1) }) }

// Notify tells the controller that the model published a snapshot. It never blocks, and costs nothing when this
// replica isn't the leader.
func (s *Status) Notify(*model.Snapshot) {
	s.init()

	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// kind describes one kind to the generic reconciler.
type kind struct {
	name   string
	object func() client.Object
	// computed returns the snapshot's answer for every object of the kind.
	computed func(*model.Snapshot) map[types.NamespacedName]computed
}

// computed is the snapshot's answer for one object.
type computed struct {
	status model.Status
	rules  []*model.Rule
}

func (c computed) equal(other computed) bool {
	if c.status != other.status || len(c.rules) != len(other.rules) {
		return false
	}

	for index := range c.rules {
		if c.rules[index].Name != other.rules[index].Name || c.rules[index].Status != other.rules[index].Status {
			return false
		}
	}

	return true
}

var kinds = []kind{
	{
		name:   "upstreamregistry",
		object: func() client.Object { return &v1alpha1.UpstreamRegistry{} },
		computed: func(snapshot *model.Snapshot) map[types.NamespacedName]computed {
			out := make(map[types.NamespacedName]computed, len(snapshot.Upstreams))
			for name, upstream := range snapshot.Upstreams {
				out[name] = computed{status: upstream.Status}
			}

			return out
		},
	},
	{
		name:   "upstreamcredential",
		object: func() client.Object { return &v1alpha1.UpstreamCredential{} },
		computed: func(snapshot *model.Snapshot) map[types.NamespacedName]computed {
			out := make(map[types.NamespacedName]computed, len(snapshot.Credentials))
			for name, credential := range snapshot.Credentials {
				out[name] = computed{status: credential.Status}
			}

			return out
		},
	},
	{
		name:   "registrymirror",
		object: func() client.Object { return &v1alpha1.RegistryMirror{} },
		computed: func(snapshot *model.Snapshot) map[types.NamespacedName]computed {
			out := make(map[types.NamespacedName]computed, len(snapshot.Mirrors))
			for name, mirror := range snapshot.Mirrors {
				out[name] = computed{status: mirror.Status}
			}

			return out
		},
	},
	{
		name:   "registryaccesspolicy",
		object: func() client.Object { return &v1alpha1.RegistryAccessPolicy{} },
		computed: func(snapshot *model.Snapshot) map[types.NamespacedName]computed {
			out := make(map[types.NamespacedName]computed, len(snapshot.Policies))
			for name, policy := range snapshot.Policies {
				out[name] = computed{status: policy.Status, rules: policy.Rules}
			}

			return out
		},
	},
}

// SetupWithManager registers one reconciler per kind, each triggered by its own kind and by snapshot rebuilds
// that changed the object's computed status. Controllers only run on the elected leader.
func (s *Status) SetupWithManager(mgr ctrl.Manager) error {
	s.init()

	channels := make([]chan event.GenericEvent, len(kinds))

	for index, k := range kinds {
		channels[index] = make(chan event.GenericEvent, 1024)

		err := ctrl.NewControllerManagedBy(mgr).
			Named(k.name).
			For(k.object()).
			WatchesRawSource(source.Channel(channels[index], &handler.EnqueueRequestForObject{})).
			Complete(&kindReconciler{client: mgr.GetClient(), snapshot: s.Snapshot, kind: k})
		if err != nil {
			return fmt.Errorf("set up %s status controller: %w", k.name, err)
		}
	}

	// Runnables run only on the leader, so a standby replica never queues or writes anything.
	err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		s.forward(ctx, channels)
		return nil
	}))
	if err != nil {
		return fmt.Errorf("set up status controller trigger: %w", err)
	}

	return nil
}

// forward turns each snapshot rebuild into events for the objects whose computed status changed since the last
// snapshot it saw. The first snapshot after becoming leader reports every object.
func (s *Status) forward(ctx context.Context, channels []chan event.GenericEvent) {
	var previous *model.Snapshot

	for {
		snapshot := s.Snapshot()
		if snapshot != nil && snapshot != previous {
			if !s.enqueueChanged(ctx, channels, previous, snapshot) {
				return
			}

			previous = snapshot
		}

		select {
		case <-ctx.Done():
			return
		case <-s.trigger:
		}
	}
}

// enqueueChanged returns false when ctx ended first.
func (s *Status) enqueueChanged(ctx context.Context, channels []chan event.GenericEvent, old, snapshot *model.Snapshot) bool {
	for index, k := range kinds {
		var before map[types.NamespacedName]computed
		if old != nil {
			before = k.computed(old)
		}

		for name, next := range k.computed(snapshot) {
			if previous, found := before[name]; found && previous.equal(next) {
				continue
			}

			object := k.object()
			object.SetNamespace(name.Namespace)
			object.SetName(name.Name)

			select {
			case channels[index] <- event.GenericEvent{Object: object}:
			case <-ctx.Done():
				return false
			}
		}
	}

	return true
}

type kindReconciler struct {
	client   client.Client
	snapshot func() *model.Snapshot
	kind     kind
}

// Reconcile writes the status of one object. It writes only when the status differs, so reconciles converge,
// and nothing for an object the snapshot doesn't know or marks pending.
func (r *kindReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	snapshot := r.snapshot()
	if snapshot == nil {
		return reconcile.Result{}, nil
	}

	answer, found := r.kind.computed(snapshot)[request.NamespacedName]
	if !found || answer.status.Pending {
		return reconcile.Result{}, nil
	}

	for _, rule := range answer.rules {
		if rule.Status.Pending {
			return reconcile.Result{}, nil
		}
	}

	object := r.kind.object()
	if err := r.client.Get(ctx, request.NamespacedName, object); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	original, ok := object.DeepCopyObject().(client.Object)
	if !ok {
		return reconcile.Result{}, fmt.Errorf("copy %T: not a client.Object", object)
	}

	if !apply(object, answer) {
		return reconcile.Result{}, nil
	}

	if err := r.client.Status().Patch(ctx, object, client.MergeFrom(original)); err != nil {
		return reconcile.Result{}, fmt.Errorf("patch status: %w", err)
	}

	return reconcile.Result{}, nil
}

// apply sets the object's status to the computed one and reports whether that changed anything.
func apply(object client.Object, answer computed) bool {
	switch typed := object.(type) {
	case *v1alpha1.UpstreamRegistry:
		return applyRegistry(&typed.Status, answer.status)
	case *v1alpha1.UpstreamCredential:
		return applyRegistry(&typed.Status, answer.status)
	case *v1alpha1.RegistryMirror:
		return applyRegistry(&typed.Status, answer.status)
	case *v1alpha1.RegistryAccessPolicy:
		return applyPolicy(&typed.Status, answer)
	}

	return false
}

func applyRegistry(status *v1alpha1.RegistryStatus, answer model.Status) bool {
	next := status.DeepCopy()
	next.ObservedGeneration = answer.ObservedGeneration
	setCondition(&next.Conditions, v1alpha1.ConditionReady, answer)

	if equality.Semantic.DeepEqual(status, next) {
		return false
	}

	*status = *next

	return true
}

func applyPolicy(status *v1alpha1.RegistryAccessPolicyStatus, answer computed) bool {
	next := status.DeepCopy()
	next.ObservedGeneration = answer.status.ObservedGeneration
	setCondition(&next.Conditions, v1alpha1.ConditionReady, answer.status)

	previous := map[string][]metav1.Condition{}
	for _, rule := range status.Rules {
		previous[rule.Name] = rule.Conditions
	}

	next.Rules = make([]v1alpha1.RuleStatus, 0, len(answer.rules))

	for _, rule := range answer.rules {
		conditions := append([]metav1.Condition(nil), previous[rule.Name]...)
		setCondition(&conditions, v1alpha1.ConditionAccepted, rule.Status)
		next.Rules = append(next.Rules, v1alpha1.RuleStatus{Name: rule.Name, Conditions: conditions})
	}

	if equality.Semantic.DeepEqual(status, next) {
		return false
	}

	*status = *next

	return true
}

// setCondition sets one condition. SetStatusCondition keeps lastTransitionTime unless the status changes.
func setCondition(conditions *[]metav1.Condition, conditionType string, answer model.Status) {
	status := metav1.ConditionFalse
	if answer.Ready {
		status = metav1.ConditionTrue
	}

	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             answer.Reason,
		Message:            answer.Message,
		ObservedGeneration: answer.ObservedGeneration,
	})
}
