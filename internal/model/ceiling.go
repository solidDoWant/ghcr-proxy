package model

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/solidDoWant/registrygate/api/v1alpha1"
	"github.com/solidDoWant/registrygate/internal/glob"
)

// CeilingViolation is a credential rule whose patterns or actions are outside the ceiling of the credential it
// names.
type CeilingViolation struct {
	// Rule is the rule's name.
	Rule string
	// Credential is the credential's namespace and name.
	Credential string
	// Outside lists "pattern action" for each part of the rule the ceiling does not contain.
	Outside []string
}

// String describes the violation with the rule name and the patterns.
func (v CeilingViolation) String() string {
	return fmt.Sprintf("rule %q is outside the ceiling of UpstreamCredential %s: %s",
		v.Rule, v.Credential, strings.Join(v.Outside, ", "))
}

// CeilingViolations checks each credential rule of the policy whose credential exists in the snapshot against that
// credential's ceiling. A rule whose credential, patterns or actions can't be resolved is skipped: the status
// reports those.
func (s *Snapshot) CeilingViolations(policy *v1alpha1.RegistryAccessPolicy) []CeilingViolation {
	var violations []CeilingViolation

	for _, spec := range policy.Spec.Rules {
		if spec.CredentialRef == nil {
			continue
		}

		name := nameOf(cmp.Or(spec.CredentialRef.Namespace, policy.Namespace), spec.CredentialRef.Name)

		credential := s.Credentials[name]
		if credential == nil || credential.ceiling == nil {
			continue
		}

		rule := &Rule{}

		for _, pattern := range spec.Repositories {
			parsed, err := glob.Parse(pattern)
			if err != nil {
				rule = nil
				break
			}

			rule.Repositories = append(rule.Repositories, parsed)
		}

		if rule == nil {
			continue
		}

		actions, err := impliedActions(spec.Actions)
		if err != nil {
			continue
		}

		rule.Actions = actions

		if outside := credential.outsideCeiling(rule); len(outside) > 0 {
			violations = append(violations, CeilingViolation{Rule: spec.Name, Credential: name.String(), Outside: outside})
		}
	}

	return violations
}
