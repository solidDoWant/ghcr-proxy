# ghcr-proxy for chart authors

For: people who write a `GHCRAccessPolicy` for a CI namespace, for example in a Helm chart that sets up CI. You know Kubernetes YAML, Helm and Docker.

- [Write a GHCRAccessPolicy](policies.md): the fields, an example and why a push is denied.
- [What the status of a policy means](policy-status.md): the `Ready` condition and `status.rejected`.
- [Field reference](crd-reference.md): every field of the `GHCRAccessPolicy` resource (generated).

Your admin must install the proxy and give your namespace a grant. See [`docs/admin/`](../admin/README.md). [`SPEC.md`](../../SPEC.md) has the full rules.
