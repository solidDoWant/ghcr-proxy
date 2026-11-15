// Package controller writes GHCRAccessPolicy status: the Ready condition and the repository patterns the
// namespace's grants drop. It runs only on the elected leader; serving never depends on it.
package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/solidDoWant/ghcr-proxy/api/v1alpha1"
	"github.com/solidDoWant/ghcr-proxy/internal/authz"
	"github.com/solidDoWant/ghcr-proxy/internal/config"
)

const (
	// LeaseName is the leader-election lease of the status controller.
	LeaseName = "ghcr-proxy-status"
	// ConditionReady is the condition type the controller writes.
	ConditionReady = "Ready"
)

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

// GHCRAccessPolicyReconciler computes each policy's effective policy under the configured grants and writes the
// result to its status.
type GHCRAccessPolicyReconciler struct {
	Client client.Client
	Grants config.Grants
}

// SetupWithManager registers the reconciler. Controllers only run on the elected leader, and the first sync
// reconciles every existing policy.
func (r *GHCRAccessPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		Named("ghcraccesspolicy").
		For(&v1alpha1.GHCRAccessPolicy{}).
		Complete(r)
	if err != nil {
		return fmt.Errorf("set up GHCRAccessPolicy status controller: %w", err)
	}

	return nil
}

// Reconcile writes the status of one policy. It writes only when the status differs, so reconciles converge.
func (r *GHCRAccessPolicyReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var policy v1alpha1.GHCRAccessPolicy
	if err := r.Client.Get(ctx, request.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	effective := authz.Effective(&policy, r.Grants.For(policy.Namespace))

	status := policy.Status.DeepCopy()
	status.ObservedGeneration = policy.Generation
	status.Rejected = effective.Rejected

	ready := metav1.ConditionFalse
	if effective.Ready() {
		ready = metav1.ConditionTrue
	}

	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             ready,
		Reason:             effective.Reason,
		Message:            effective.Message,
		ObservedGeneration: policy.Generation,
	})

	if equality.Semantic.DeepEqual(&policy.Status, status) {
		return ctrl.Result{}, nil
	}

	policy.Status = *status
	if err := r.Client.Status().Update(ctx, &policy); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}

	return ctrl.Result{}, nil
}
