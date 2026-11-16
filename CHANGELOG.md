# Changelog

## 0.1.1

ghcr-proxy is a proxy for the GitHub container registry, `ghcr.io`. It runs in a Kubernetes cluster and holds one GitHub personal access token (PAT). CI pods push and pull images through the proxy with the token of their own ServiceAccount, and they never see the PAT. This patch release fixes pushes to `ghcr.io`, which did not work in 0.1.0. It also includes the first parts of the next phase of work. These parts are off by default. Install it with the Helm chart `oci://ghcr.io/soliddowant/charts/ghcr-proxy`, version `0.1.1`. [Install ghcr-proxy](https://github.com/solidDoWant/ghcr-proxy/blob/v0.1.1/docs/admin/install.md) tells you how.

### Upgrade notes for admins

- If you run 0.1.0, upgrade to 0.1.1. With 0.1.0, every push to `ghcr.io` through the proxy fails. You do not have to change your chart values. All new chart values are off by default.

### Pushing and pulling

- Pushes through the proxy to `ghcr.io` work. In 0.1.0, they failed with `404 NOT_FOUND`. The reason is that `ghcr.io` gives upload paths of the form `/v2/<name>/blobs/upload/<session>` (singular `upload`), and the proxy accepted only the plural form of the registry specification. The proxy now accepts both forms ([#76](https://github.com/solidDoWant/ghcr-proxy/pull/76), [#79](https://github.com/solidDoWant/ghcr-proxy/pull/79)).

### Mirrors

This work is part of the next phase and is still in progress.

- The proxy can send pulls to a pull mirror, which is a registry that keeps copies of GHCR images (for example a Harbor proxy-cache project). Set the chart values `mirrors.policy` and `mirrors.anonymous`. Allowed pulls of manifests and blobs go to the policy mirror. Anonymous and not-allowed pulls go to the anonymous mirror. If a mirror fails, the pull goes to `ghcr.io`, unless you set `fallbackToUpstream: false`. Each mirror has its own credentials Secret and CA Secret. The proxy never sends the PAT to a mirror. [Configure pull mirrors](https://github.com/solidDoWant/ghcr-proxy/blob/v0.1.1/docs/admin/install.md#configure-pull-mirrors) tells you how ([#71](https://github.com/solidDoWant/ghcr-proxy/pull/71), [#75](https://github.com/solidDoWant/ghcr-proxy/pull/75)).

### Access policies and grants

This work is part of the next phase and is still in progress.

- An optional admission webhook rejects a `GHCRAccessPolicy` at `kubectl apply` when a repository pattern or an action is outside the grants of its namespace. The proxy enforces the grants with or without the webhook. Set `webhook.enabled=true` and give the webhook a certificate, from cert-manager or from your own Secret. [Turn on the admission webhook](https://github.com/solidDoWant/ghcr-proxy/blob/v0.1.1/docs/admin/install.md#turn-on-the-admission-webhook) tells you how ([#70](https://github.com/solidDoWant/ghcr-proxy/pull/70)).

### Deployment

This work is part of the next phase and is still in progress.

- The chart can create a PodDisruptionBudget, a NetworkPolicy and a Prometheus Operator ServiceMonitor. Each one is off by default. Set `podDisruptionBudget.enabled`, `networkPolicy.enabled` or `serviceMonitor.enabled` to `true`. [Optional chart objects](https://github.com/solidDoWant/ghcr-proxy/blob/v0.1.1/docs/admin/install.md#optional-chart-objects) tells you about them ([#72](https://github.com/solidDoWant/ghcr-proxy/pull/72)).
- The proxy serves Prometheus metrics at `/metrics` on the health port (`8080`). They count requests, requests to `ghcr.io` and the mirrors, mirror fallbacks, token reviews, policies and credential reloads. No metric has a label for a repository, namespace or ServiceAccount. [Metrics](https://github.com/solidDoWant/ghcr-proxy/blob/v0.1.1/docs/admin/metrics.md) lists them ([#81](https://github.com/solidDoWant/ghcr-proxy/pull/81)).
- The admin guides have full commands that you can copy and run, from an empty cluster to a CI pod that pushes an image. Tests run these commands ([#74](https://github.com/solidDoWant/ghcr-proxy/pull/74)).

Internal work: the design for the next phase, tests against the real `ghcr.io` that the operator runs by hand, test fixes, scheduler fixes and dependency updates ([#60](https://github.com/solidDoWant/ghcr-proxy/pull/60), [#61](https://github.com/solidDoWant/ghcr-proxy/pull/61), [#64](https://github.com/solidDoWant/ghcr-proxy/pull/64), [#73](https://github.com/solidDoWant/ghcr-proxy/pull/73), [#80](https://github.com/solidDoWant/ghcr-proxy/pull/80), [#83](https://github.com/solidDoWant/ghcr-proxy/pull/83)).

### Known issues

- Deleting images through the proxy is not yet tested with `ghcr.io`, and it can fail. The next phase finishes it ([#65](https://github.com/solidDoWant/ghcr-proxy/issues/65)).
- The pull mirrors are not yet tested with Harbor ([#68](https://github.com/solidDoWant/ghcr-proxy/issues/68)).

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
