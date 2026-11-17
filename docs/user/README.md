# registrygate for chart authors

For: people who write a `RegistryAccessPolicy` for a CI namespace, for example in a Helm chart that sets up CI. You know Kubernetes YAML, Helm and Docker.

- [Write a RegistryAccessPolicy](policies.md): the fields, an example, tenant mirrors and the Role for their Secret, and why a request is denied or ambiguous.
- [What the status of an object means](policy-status.md): the `Ready` condition, the reasons and the status of each rule.
- [Field reference](crd-reference.md): every field of the four resources (generated).

Your admin must install the proxy and make the upstream and the credentials that your namespace uses. See [`docs/admin/`](../admin/README.md). [`SPEC.md`](../../SPEC.md) has the full rules.
