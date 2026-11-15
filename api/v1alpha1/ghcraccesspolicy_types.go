package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Action is a registry action a rule allows.
// +kubebuilder:validation:Enum=pull;push;delete
type Action string

const (
	// ActionPull reads manifests, blobs, tag lists and referrers.
	ActionPull Action = "pull"
	// ActionPush uploads blobs and manifests. It implies pull.
	ActionPush Action = "push"
	// ActionDelete deletes manifests and blobs. It implies pull.
	ActionDelete Action = "delete"
)

// AccessRule allows some actions on the repositories its patterns match.
type AccessRule struct {
	// Repositories are glob patterns of repository names, without the registry host. `*` matches within one path
	// segment and `**` matches any number of whole segments.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=255
	// +kubebuilder:validation:XValidation:rule="self.all(p, p.matches('^(\\\\*\\\\*|\\\\*|\\\\*?([a-z0-9._-]+\\\\*?)+)(/(\\\\*\\\\*|\\\\*|\\\\*?([a-z0-9._-]+\\\\*?)+))*$'))",message="each repository pattern must be lower-case segments of [a-z0-9._-] and *, separated by /, with ** only as a whole segment"
	// +listType=atomic
	Repositories []string `json:"repositories"`
	// Actions allowed on those repositories.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=3
	// +listType=set
	Actions []Action `json:"actions"`
}

// GHCRAccessPolicySpec says which ServiceAccounts of this namespace may do what on which repositories.
type GHCRAccessPolicySpec struct {
	// ServiceAccounts in this namespace this policy applies to.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +listType=set
	ServiceAccounts []string `json:"serviceAccounts"`
	// Rules allow actions on repositories. Policies only ever add access.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	Rules []AccessRule `json:"rules"`
}

// RejectedReason says why a repository pattern lost actions.
// +kubebuilder:validation:Enum=OutsideGrant;InvalidPattern;NoGrant
type RejectedReason string

const (
	// RejectedOutsideGrant: the namespace's grant doesn't cover the pattern for these actions.
	RejectedOutsideGrant RejectedReason = "OutsideGrant"
	// RejectedInvalidPattern: the pattern is not a valid repository pattern.
	RejectedInvalidPattern RejectedReason = "InvalidPattern"
	// RejectedNoGrant: no grant applies to the namespace.
	RejectedNoGrant RejectedReason = "NoGrant"
)

// RejectedRule is one repository pattern of one rule that lost at least one action. The lost actions are ignored;
// the rest of the policy still applies.
type RejectedRule struct {
	// Rule is the index into spec.rules.
	Rule int32 `json:"rule"`
	// Repository is the pattern as written in the rule.
	Repository string `json:"repository"`
	// Actions are the actions dropped for this pattern, counting the pull that push and delete imply.
	// +listType=atomic
	Actions []Action `json:"actions"`
	// Reason says why the actions were dropped.
	Reason RejectedReason `json:"reason"`
}

// GHCRAccessPolicyStatus is the observed state of a GHCRAccessPolicy. Only the proxy writes it.
type GHCRAccessPolicyStatus struct {
	// ObservedGeneration is the last generation the proxy computed this status for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Rejected lists each repository pattern that lost actions, with the actions it lost and why.
	// +optional
	// +listType=atomic
	Rejected []RejectedRule `json:"rejected,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=gap
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GHCRAccessPolicy lets ServiceAccounts of its namespace pull, push or delete GHCR repositories through the proxy,
// within the grant the proxy's configuration gives the namespace.
type GHCRAccessPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GHCRAccessPolicySpec   `json:"spec"`
	Status GHCRAccessPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GHCRAccessPolicyList is a list of GHCRAccessPolicy.
type GHCRAccessPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GHCRAccessPolicy `json:"items"`
}
