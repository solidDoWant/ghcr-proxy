# Changelog

## 0.1.0

ghcr-proxy is a proxy for the GitHub container registry, `ghcr.io`. It runs in a Kubernetes cluster and holds one GitHub personal access token (PAT). CI pods push and pull images through the proxy with the token of their own ServiceAccount, and they never see the PAT. This is the first release. It covers pushing and pulling through the proxy. Install it with the Helm chart `oci://ghcr.io/soliddowant/charts/ghcr-proxy`, version `0.1.0`. [Install ghcr-proxy](https://github.com/solidDoWant/ghcr-proxy/blob/v0.1.0/docs/admin/install.md) tells you how, and [Set up CI pods](https://github.com/solidDoWant/ghcr-proxy/blob/v0.1.0/docs/admin/ci-pods.md) tells you how to connect Docker in your CI pods to the proxy.

### Pushing and pulling

- Docker in a CI pod can push and pull images through the proxy with the same image names and commands as on `ghcr.io` ([#29](https://github.com/solidDoWant/ghcr-proxy/pull/29), [#34](https://github.com/solidDoWant/ghcr-proxy/pull/34), [#36](https://github.com/solidDoWant/ghcr-proxy/pull/36)).
- The proxy uses the PAT only for a request that a policy allows. It sends every other pull to `ghcr.io` with no credential, so public images still work ([#34](https://github.com/solidDoWant/ghcr-proxy/pull/34), [#35](https://github.com/solidDoWant/ghcr-proxy/pull/35)).
- A push that a policy does not allow fails with `403`. The error that Docker prints names the repository and the action, and tells you if no policy or the grant refused it ([#35](https://github.com/solidDoWant/ghcr-proxy/pull/35)).
- `/v2/_catalog` and unknown paths get `404`. The proxy does not send them to `ghcr.io` ([#29](https://github.com/solidDoWant/ghcr-proxy/pull/29)).

### Access policies and grants

- A `GHCRAccessPolicy` resource in a namespace says which repositories each ServiceAccount of that namespace can pull from or push to. Repository names can use globs (`*` and `**`) ([#28](https://github.com/solidDoWant/ghcr-proxy/pull/28), [#35](https://github.com/solidDoWant/ghcr-proxy/pull/35)).
- Grants in the configuration of the proxy limit what the policies of each namespace can allow ([#27](https://github.com/solidDoWant/ghcr-proxy/pull/27)).
- The proxy writes the status of each policy: the `Ready` condition (with the reason `Accepted`, `OutsideGrant`, `InvalidPattern` or `NoGrant`) and a `status.rejected` list of the repository patterns that lost actions. Only one replica writes the status ([#33](https://github.com/solidDoWant/ghcr-proxy/pull/33)).
- A change to a policy takes effect within a few seconds, without a restart ([#35](https://github.com/solidDoWant/ghcr-proxy/pull/35)).

### Credential helper

- The Docker credential helper `docker-credential-ghcr-proxy` gives the ServiceAccount token of the pod to Docker for `ghcr.io`. It reads the token file again on each request, so a rotated token works without a restart. Its image, `ghcr.io/soliddowant/ghcr-proxy-credential-helper`, holds only the static binary, for amd64 and arm64 ([#30](https://github.com/solidDoWant/ghcr-proxy/pull/30)).

### Deployment

- The Helm chart installs the proxy with two replicas, the Service, the configuration, the `GHCRAccessPolicy` CRD and the RBAC. You create the PAT and TLS Secrets before you install it. The pods run as non-root with a read-only root filesystem ([#37](https://github.com/solidDoWant/ghcr-proxy/pull/37)).
- The proxy serves the registry API over TLS, with `/healthz`, `/readyz` and `/metrics`. A replica is not ready until its policies are synced and the PAT and the certificate are loaded ([#36](https://github.com/solidDoWant/ghcr-proxy/pull/36)).
- The proxy loads a new PAT or a new TLS certificate from its Secret without a restart ([#34](https://github.com/solidDoWant/ghcr-proxy/pull/34), [#36](https://github.com/solidDoWant/ghcr-proxy/pull/36)).
- The chart value `upstream.ca.existingSecret` adds a CA certificate to trust for a private or test upstream ([#40](https://github.com/solidDoWant/ghcr-proxy/pull/40)).
- The proxy writes one JSON audit line for each request, with the namespace, the ServiceAccount, the repository, the action, the decision and the upstream status ([#38](https://github.com/solidDoWant/ghcr-proxy/pull/38)).
- New guides for admins (install and CI pod setup) and for users (how to write a policy) ([#39](https://github.com/solidDoWant/ghcr-proxy/pull/39)).

### Security

- The proxy checks each ServiceAccount token with the Kubernetes `TokenReview` API. A token without the `ghcr-proxy` audience gets `401`. If the API server is down, the proxy answers `503` and does not use the PAT ([#32](https://github.com/solidDoWant/ghcr-proxy/pull/32)).
- If `ghcr.io` rejects the PAT, the proxy answers `502` with a message that says the credential of the proxy was rejected. The PAT is never in a log line, a response or a status ([#34](https://github.com/solidDoWant/ghcr-proxy/pull/34)).
- The binaries are built with Go 1.26.11, `golang.org/x/net` v0.60.0 and `github.com/klauspost/compress` v1.20.2, which fix known vulnerabilities ([#52](https://github.com/solidDoWant/ghcr-proxy/pull/52), [#55](https://github.com/solidDoWant/ghcr-proxy/pull/55)).

### Fixes

- The proxy no longer sends interim (1xx) responses from `ghcr.io` on to the client ([#53](https://github.com/solidDoWant/ghcr-proxy/pull/53)).
- A replica that loses the lease for writing policy status keeps serving requests. It also gives up the lease when it shuts down ([#56](https://github.com/solidDoWant/ghcr-proxy/pull/56)).

Internal work: the design review, the fake GHCR for tests, end-to-end tests on kind and the tests for the acceptance checklist ([#26](https://github.com/solidDoWant/ghcr-proxy/pull/26), [#31](https://github.com/solidDoWant/ghcr-proxy/pull/31), [#40](https://github.com/solidDoWant/ghcr-proxy/pull/40), [#44](https://github.com/solidDoWant/ghcr-proxy/pull/44), [#45](https://github.com/solidDoWant/ghcr-proxy/pull/45), [#46](https://github.com/solidDoWant/ghcr-proxy/pull/46)).

### Known issues

- Deleting images through the proxy is not tested against `ghcr.io` yet. The next phase covers it ([#19](https://github.com/solidDoWant/ghcr-proxy/issues/19)).
