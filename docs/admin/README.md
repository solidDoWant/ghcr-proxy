# ghcr-proxy for admins

For: people who install ghcr-proxy and set up CI pods to use it. You know Kubernetes, Helm, Flux, cert-manager and Tekton.

- [Install ghcr-proxy](install.md): the PAT, the certificate and its CA, the grants, the chart, the security limits and the audit log.
- [Set up a CI pod](ci-pods.md): host aliases, the CA, the credential helper, the token and a Tekton Task example.
- [Metrics](metrics.md): the Prometheus metrics and their labels.
- [Chart values](chart-values.md): every Helm value (generated).

[`SPEC.md`](../../SPEC.md) has the full rules.
