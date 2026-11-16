// Package webhook is the validating admission webhook for UpstreamRegistry and RegistryAccessPolicy. It gives fast
// feedback at apply time. The proxy stays the enforcement point; the webhook reads the same snapshot as serving, so
// it never decides differently from it.
package webhook

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/model"
)

// URL paths the validating webhooks are served at.
const (
	UpstreamPath = "/validate-upstreamregistry"
	PolicyPath   = "/validate-registryaccesspolicy"
)

// SnapshotFunc returns the current snapshot, or nil before the first build.
type SnapshotFunc func() *model.Snapshot

// UpstreamValidator rejects an UpstreamRegistry outside the admin namespaces.
type UpstreamValidator struct {
	AdminNamespaces []string
}

var _ admission.Validator[*v1alpha1.UpstreamRegistry] = (*UpstreamValidator)(nil)

// ValidateCreate rejects an upstream outside the admin namespaces.
func (v *UpstreamValidator) ValidateCreate(_ context.Context, upstream *v1alpha1.UpstreamRegistry) (admission.Warnings, error) {
	return nil, v.check(upstream)
}

// ValidateUpdate rejects a spec change outside the admin namespaces. An update that leaves the spec as it was, or
// one on an object that is being deleted, is admitted, so labels and finalizers can still change.
func (v *UpstreamValidator) ValidateUpdate(_ context.Context, oldUpstream, upstream *v1alpha1.UpstreamRegistry) (admission.Warnings, error) {
	if upstream.DeletionTimestamp != nil || equality.Semantic.DeepEqual(oldUpstream.Spec, upstream.Spec) {
		return nil, nil
	}

	return nil, v.check(upstream)
}

// ValidateDelete admits every delete.
func (v *UpstreamValidator) ValidateDelete(context.Context, *v1alpha1.UpstreamRegistry) (admission.Warnings, error) {
	return nil, nil
}

func (v *UpstreamValidator) check(upstream *v1alpha1.UpstreamRegistry) error {
	if slices.Contains(v.AdminNamespaces, upstream.Namespace) {
		return nil
	}

	return fmt.Errorf("an UpstreamRegistry may only be created in an admin namespace (%s), not in %q",
		strings.Join(v.AdminNamespaces, ", "), upstream.Namespace)
}

// PolicyValidator rejects a RegistryAccessPolicy with a credential rule outside its credential's ceiling.
type PolicyValidator struct {
	Snapshot SnapshotFunc
}

var _ admission.Validator[*v1alpha1.RegistryAccessPolicy] = (*PolicyValidator)(nil)

// ValidateCreate rejects a new policy with a rule outside its credential's ceiling.
func (v *PolicyValidator) ValidateCreate(_ context.Context, policy *v1alpha1.RegistryAccessPolicy) (admission.Warnings, error) {
	return nil, v.check(policy)
}

// ValidateUpdate rejects a spec change with a rule outside the ceiling. An update that leaves the spec as it was,
// or one on an object that is being deleted, is admitted, so labels and finalizers can still change after an admin
// narrows a ceiling.
func (v *PolicyValidator) ValidateUpdate(_ context.Context, oldPolicy, policy *v1alpha1.RegistryAccessPolicy) (admission.Warnings, error) {
	if policy.DeletionTimestamp != nil || equality.Semantic.DeepEqual(oldPolicy.Spec, policy.Spec) {
		return nil, nil
	}

	return nil, v.check(policy)
}

// ValidateDelete admits every delete.
func (v *PolicyValidator) ValidateDelete(context.Context, *v1alpha1.RegistryAccessPolicy) (admission.Warnings, error) {
	return nil, nil
}

func (v *PolicyValidator) check(policy *v1alpha1.RegistryAccessPolicy) error {
	snapshot := v.Snapshot()
	if snapshot == nil {
		return errors.New("registrygate is not ready to check the policy; try again")
	}

	violations := snapshot.CeilingViolations(policy)
	if len(violations) == 0 {
		return nil
	}

	parts := make([]string, 0, len(violations))
	for _, violation := range violations {
		parts = append(parts, violation.String())
	}

	return errors.New(strings.Join(parts, "; "))
}

// Register serves the validators at UpstreamPath and PolicyPath on server.
func Register(server webhook.Server, scheme *runtime.Scheme, adminNamespaces []string, snapshot SnapshotFunc) {
	server.Register(UpstreamPath, admission.WithValidator(scheme, &UpstreamValidator{AdminNamespaces: adminNamespaces}))
	server.Register(PolicyPath, admission.WithValidator(scheme, &PolicyValidator{Snapshot: snapshot}))
}
