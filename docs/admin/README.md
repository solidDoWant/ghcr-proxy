# registrygate for admins

For: people who install registrygate and set up CI pods to use it. You know Kubernetes, Helm, Flux, cert-manager and Tekton.

Read the pages in this order:

1. [Install registrygate](install.md): the chart, the optional chart objects, the admission webhook, the security limits and the audit log.
2. [Upstream registries](upstreams.md): one CA for all intercepted hosts, the `UpstreamRegistry` object, `anonymousPulls`, plain HTTP, Harbor through its Service and Docker Hub.
3. [Shared credentials](credentials.md): the PAT and the `UpstreamCredential` object with its ceiling.
4. [Mirrors](mirrors.md): `RegistryMirror` objects and the rule that mirrors serve public content only.
5. [Set up a CI pod](ci-pods.md): host aliases, the CA, the credential helper, the token and a Tekton Task example.

More pages:

- [Metrics](metrics.md): the Prometheus metrics and their labels.
- [Chart values](chart-values.md): every Helm value (generated).

[`SPEC.md`](../../SPEC.md) has the full rules.
