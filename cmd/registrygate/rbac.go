package main

// The proxy's cluster-wide permissions: review ServiceAccount tokens, read every GHCRAccessPolicy and write their
// status. Nothing else. The leader-election lease is namespaced, so the chart grants it with a Role. `make generate`
// writes these into config/rbac/role.yaml.

// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=registrygate.soliddowant.dev,resources=ghcraccesspolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=registrygate.soliddowant.dev,resources=ghcraccesspolicies/status,verbs=update
