// Package webhook is the validating admission webhook for GHCRAccessPolicy. It gives fast feedback at apply time:
// a policy that the namespace's grants would cut down is rejected with the patterns named. The proxy stays the
// enforcement point; the webhook only needs the config and its own certificate.
package webhook

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/authz"
	"github.com/solidDoWant/registrygate/internal/config"
)

// Path is the URL path the validating webhook is served at.
const Path = "/validate-ghcraccesspolicy"

// Validator rejects policies the grants would not fully accept.
type Validator struct {
	Grants config.Grants
}

var _ admission.Validator[*v1alpha1.GHCRAccessPolicy] = (*Validator)(nil)

// ValidateCreate rejects a new policy that is not fully inside its namespace's grants.
func (v *Validator) ValidateCreate(_ context.Context, policy *v1alpha1.GHCRAccessPolicy) (admission.Warnings, error) {
	return nil, v.check(policy)
}

// ValidateUpdate rejects a spec change that is not fully inside the grants. An update that leaves the spec as it
// was, or one on an object that is being deleted, is admitted, so labels, annotations and finalizers can still
// change after a grant is narrowed.
func (v *Validator) ValidateUpdate(_ context.Context, oldPolicy, policy *v1alpha1.GHCRAccessPolicy) (admission.Warnings, error) {
	if policy.DeletionTimestamp != nil || equality.Semantic.DeepEqual(oldPolicy.Spec, policy.Spec) {
		return nil, nil
	}

	return nil, v.check(policy)
}

// ValidateDelete admits every delete.
func (v *Validator) ValidateDelete(context.Context, *v1alpha1.GHCRAccessPolicy) (admission.Warnings, error) {
	return nil, nil
}

func (v *Validator) check(policy *v1alpha1.GHCRAccessPolicy) error {
	effective := authz.Effective(policy, v.Grants.For(policy.Namespace))
	if effective.Ready() {
		return nil
	}

	parts := make([]string, 0, len(effective.Rejected))
	for _, rejected := range effective.Rejected {
		actions := make([]string, 0, len(rejected.Actions))
		for _, action := range rejected.Actions {
			actions = append(actions, string(action))
		}

		parts = append(parts, fmt.Sprintf("rules[%d] %q (%s: %s)", rejected.Rule, rejected.Repository,
			strings.Join(actions, ", "), rejected.Reason))
	}

	if len(parts) == 0 {
		return errors.New("the policy is not inside the namespace's grants: " + effective.Message)
	}

	return fmt.Errorf("the policy is not inside the namespace's grants; these repository patterns are rejected: %s",
		strings.Join(parts, "; "))
}

// Register serves the validator at Path on server.
func Register(server webhook.Server, scheme *runtime.Scheme, grants config.Grants) {
	server.Register(Path, admission.WithValidator(scheme, &Validator{Grants: grants}))
}
