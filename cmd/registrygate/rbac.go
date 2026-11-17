package main

// The proxy's cluster-wide permissions: review ServiceAccount tokens, read the four custom resources and write their
// status, and read Namespaces for `allowedNamespaces.selector`. Nothing else. Secrets and the leader-election lease
// are namespaced, so the chart grants them with Roles. `make generate` writes these into config/rbac/role.yaml.

// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=registrygate.soliddowant.dev,resources=upstreamregistries;upstreamcredentials;registrymirrors;registryaccesspolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=registrygate.soliddowant.dev,resources=upstreamregistries/status;upstreamcredentials/status;registrymirrors/status;registryaccesspolicies/status,verbs=update;patch
