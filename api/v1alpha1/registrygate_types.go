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

// Condition types and reasons shared by the status of every registrygate kind.
const (
	// ConditionReady is the condition type every kind reports.
	ConditionReady = "Ready"
	// ConditionAccepted is the condition type of a single rule of a RegistryAccessPolicy.
	ConditionAccepted = "Accepted"

	ReasonReady               = "Ready"
	ReasonAccepted            = "Accepted"
	ReasonInvalidSpec         = "InvalidSpec"
	ReasonNotAllowed          = "NotAllowed"
	ReasonRefNotFound         = "RefNotFound"
	ReasonRefNotReady         = "RefNotReady"
	ReasonSecretUnavailable   = "SecretUnavailable"
	ReasonSecretInvalid       = "SecretInvalid"
	ReasonHostConflict        = "HostConflict"
	ReasonCertificateMismatch = "CertificateMismatch"
	ReasonURLNotAllowed       = "URLNotAllowed"
	ReasonOutsideCeiling      = "OutsideCeiling"
	// ReasonRulesNotAccepted is the Ready=False reason of a policy when any rule isn't accepted.
	ReasonRulesNotAccepted = "RulesNotAccepted"
)

// AnonymousPulls values of UpstreamRegistrySpec.AnonymousPulls.
const (
	// AnonymousPullsChallenge answers an unauthenticated request with a Basic challenge (the default).
	AnonymousPullsChallenge = "Challenge"
	// AnonymousPullsForward forwards an unauthenticated pull upstream with no credential.
	AnonymousPullsForward = "Forward"
)

// LocalRef names a Secret in the namespace of the object that refers to it.
type LocalRef struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// ObjectRef names another object. The model treats an empty namespace as the referring object's.
type ObjectRef struct {
	// Namespace of the object. Defaults to the namespace of the referring object.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?)?$`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// Name of the object.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// CredentialsSecretRef names a Secret with a username and a password.
type CredentialsSecretRef struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
	// UsernameKey is the Secret key of the username. Default "username".
	// +kubebuilder:validation:MaxLength=253
	// +optional
	UsernameKey string `json:"usernameKey,omitempty"`
	// PasswordKey is the Secret key of the password. Default "password".
	// +kubebuilder:validation:MaxLength=253
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

// ServingTLS names the Secret with the serving certificate (keys tls.crt and tls.key).
type ServingTLS struct {
	// SecretRef is the Secret with the serving key pair.
	SecretRef LocalRef `json:"secretRef"`
}

// AllowedNamespaces says which namespaces may refer to an object. The object's own namespace is always allowed.
type AllowedNamespaces struct {
	// Names of allowed namespaces.
	// +kubebuilder:validation:MaxItems=256
	// +listType=set
	// +optional
	Names []string `json:"names,omitempty"`
	// Selector allows every namespace whose labels match. An empty selector allows every namespace.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// Timeouts bound the calls to an upstream or mirror. There is no whole-request limit, so uploads of several GB fit.
type Timeouts struct {
	// ResponseHeader is the time from the request being sent to the response headers arriving. Default 30s.
	// +optional
	ResponseHeader *metav1.Duration `json:"responseHeader,omitempty"`
	// Token is the time for the ping and the token exchange together. Default 10s.
	// +optional
	Token *metav1.Duration `json:"token,omitempty"`
}

// AccessGrant is a set of actions on the repositories its globs match.
type AccessGrant struct {
	// Repositories are glob patterns of repository names, without the registry host.
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

// UpstreamRegistrySpec says which hosts the proxy answers for and where their requests go.
// +kubebuilder:validation:XValidation:rule="self.url.matches('^https?://[^/?#@]+$')",message="url must be http(s)://host[:port] with no path, query or userinfo"
type UpstreamRegistrySpec struct {
	// Hosts are the DNS names the proxy answers for.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +listType=set
	Hosts []string `json:"hosts"`
	// URL is where requests go: https://host[:port].
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`
	// CASecretRef names the Secret (key ca.crt) with the CA that signs the upstream.
	// +optional
	CASecretRef *LocalRef `json:"caSecretRef,omitempty"`
	// TLS is the serving certificate.
	TLS ServingTLS `json:"tls"`
	// TokenRealmHosts are hosts (host or host:port) allowed as the token endpoint besides the url host.
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=260
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*(:[0-9]{1,5})?$`
	// +listType=set
	// +optional
	TokenRealmHosts []string `json:"tokenRealmHosts,omitempty"`
	// AnonymousPulls says what happens to an unauthenticated pull. Default Challenge.
	// +kubebuilder:validation:Enum=Challenge;Forward
	// +optional
	AnonymousPulls string `json:"anonymousPulls,omitempty"`
	// DefaultMirrorRef is the mirror that serves pulls no rule decides.
	// +optional
	DefaultMirrorRef *ObjectRef `json:"defaultMirrorRef,omitempty"`
	// AllowedMirrorURLs are the URL prefixes a mirror of this upstream may use.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=2048
	// +kubebuilder:validation:items:XValidation:rule="self.matches('^https?://[^/?#@%]+(/[^/?#%]+)*/?$') && !self.matches('/\\\\.\\\\.?(/|$)')",message="must be an http(s) URL prefix with no userinfo, query, fragment, %, empty or dot segments"
	// +listType=set
	// +optional
	AllowedMirrorURLs []string `json:"allowedMirrorURLs,omitempty"`
	// Timeouts of calls to the upstream.
	// +optional
	Timeouts Timeouts `json:"timeouts,omitempty"`
	// AllowedNamespaces may point credentials, mirrors and policies at this upstream.
	// +optional
	AllowedNamespaces AllowedNamespaces `json:"allowedNamespaces,omitempty"`
}

// UpstreamCredentialSpec is a shared credential for an upstream, capped by a ceiling.
type UpstreamCredentialSpec struct {
	// UpstreamRef is the upstream this credential is for.
	UpstreamRef ObjectRef `json:"upstreamRef"`
	// SecretRef is the Secret with the username and password.
	SecretRef CredentialsSecretRef `json:"secretRef"`
	// Ceiling is the most any rule using this credential may allow.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	Ceiling []AccessGrant `json:"ceiling"`
	// AllowedNamespaces may use this credential in policy rules.
	// +optional
	AllowedNamespaces AllowedNamespaces `json:"allowedNamespaces,omitempty"`
}

// RegistryMirrorSpec is a pull-only mirror of an upstream.
// +kubebuilder:validation:XValidation:rule="self.url.matches('^https?://[^/?#@%]+(/[^/?#%]+)*/?$') && !self.url.matches('/\\\\.\\\\.?(/|$)')",message="url must be an http(s) URL with no userinfo, query, fragment, %, empty or dot segments"
type RegistryMirrorSpec struct {
	// UpstreamRef is the upstream this mirror serves.
	UpstreamRef ObjectRef `json:"upstreamRef"`
	// URL of the mirror.
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`
	// PathPrefix is put before the repository name on the mirror.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`
	PathPrefix string `json:"pathPrefix"`
	// SecretRef is the Secret with the mirror's username and password.
	// +optional
	SecretRef *CredentialsSecretRef `json:"secretRef,omitempty"`
	// CASecretRef names the Secret (key ca.crt) with the CA that signs the mirror.
	// +optional
	CASecretRef *LocalRef `json:"caSecretRef,omitempty"`
	// FallbackToUpstream sends a failed mirror pull to the upstream. Default true.
	// +optional
	FallbackToUpstream *bool `json:"fallbackToUpstream,omitempty"`
	// Timeouts of calls to the mirror.
	// +optional
	Timeouts Timeouts `json:"timeouts,omitempty"`
	// AllowedNamespaces may use this mirror in policy rules.
	// +optional
	AllowedNamespaces AllowedNamespaces `json:"allowedNamespaces,omitempty"`
}

// RegistryAccessPolicySpec says which ServiceAccounts of this namespace may do what.
type RegistryAccessPolicySpec struct {
	// ServiceAccounts in this namespace this policy applies to.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +listType=set
	ServiceAccounts []string `json:"serviceAccounts"`
	// Rules allow actions on repositories through a credential or a mirror.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	Rules []AccessRule `json:"rules"`
}

// AccessRule allows actions on repositories through exactly one of a credential and a mirror.
// +kubebuilder:validation:XValidation:rule="has(self.credentialRef) != has(self.mirrorRef)",message="exactly one of credentialRef and mirrorRef must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.mirrorRef) || (self.actions.size() == 1 && self.actions[0] == 'pull')",message="a mirror rule may only allow pull"
type AccessRule struct {
	// Name identifies the rule in the policy and in its status.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// CredentialRef is the UpstreamCredential that backs the rule.
	// +optional
	CredentialRef *ObjectRef `json:"credentialRef,omitempty"`
	// MirrorRef is the RegistryMirror that serves the rule's pulls.
	// +optional
	MirrorRef *ObjectRef `json:"mirrorRef,omitempty"`
	// Repositories are glob patterns of repository names, without the registry host.
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

// RegistryStatus is the observed state of an UpstreamRegistry, UpstreamCredential or RegistryMirror.
// Only the proxy writes it.
type RegistryStatus struct {
	// ObservedGeneration is the last generation the proxy computed this status for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RuleStatus is the observed state of one rule of a policy.
type RuleStatus struct {
	// Name of the rule.
	Name string `json:"name"`
	// Conditions: Accepted.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RegistryAccessPolicyStatus is the observed state of a RegistryAccessPolicy. Only the proxy writes it.
type RegistryAccessPolicyStatus struct {
	// ObservedGeneration is the last generation the proxy computed this status for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Rules has the status of each rule.
	// +listType=map
	// +listMapKey=name
	// +optional
	Rules []RuleStatus `json:"rules,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=registrygate
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`

// UpstreamRegistry is a registry the proxy fronts. Only admin namespaces may hold one.
type UpstreamRegistry struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UpstreamRegistrySpec `json:"spec"`
	Status RegistryStatus       `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// UpstreamRegistryList is a list of UpstreamRegistry.
type UpstreamRegistryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UpstreamRegistry `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=registrygate
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`

// UpstreamCredential is a shared credential for an upstream with a ceiling on what rules may do with it.
type UpstreamCredential struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UpstreamCredentialSpec `json:"spec"`
	Status RegistryStatus         `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// UpstreamCredentialList is a list of UpstreamCredential.
type UpstreamCredentialList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UpstreamCredential `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=registrygate
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`

// RegistryMirror is a pull-only mirror of an upstream.
type RegistryMirror struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RegistryMirrorSpec `json:"spec"`
	Status RegistryStatus     `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RegistryMirrorList is a list of RegistryMirror.
type RegistryMirrorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RegistryMirror `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=registrygate
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`

// RegistryAccessPolicy lets ServiceAccounts of its namespace use credentials and mirrors through the proxy.
type RegistryAccessPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RegistryAccessPolicySpec   `json:"spec"`
	Status RegistryAccessPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RegistryAccessPolicyList is a list of RegistryAccessPolicy.
type RegistryAccessPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RegistryAccessPolicy `json:"items"`
}
