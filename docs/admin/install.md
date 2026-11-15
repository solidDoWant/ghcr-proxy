# Install ghcr-proxy

For: admins who install ghcr-proxy in a cluster. You know Kubernetes, Helm and cert-manager.

This page covers the PAT, the TLS certificate and its CA, the grants, the chart install and the audit log. To set up a CI pod after the install, read [CI pod setup](ci-pods.md).

## Requirements

- Kubernetes 1.33 or later. The `ImageVolume` feature must be on, because CI pods mount the credential helper as an image volume.
- A `TokenReview` API that the proxy can reach.
- Docker CLI and daemon 25 or later in the CI pods.

## Create the PAT Secret

The proxy holds one GitHub classic personal access token (PAT). GitHub has no API that makes GHCR credentials, so you make the PAT by hand.

1. Create a GitHub machine-user account that is only for this proxy.
2. Give the machine user access to the packages that CI must push to.
3. Create a classic PAT for that account with exactly these scopes: `read:packages`, `write:packages` and `delete:packages`.
4. Store the PAT in a Secret in the namespace of the proxy.

```
kubectl -n ghcr-proxy create secret generic ghcr-proxy-pat --from-file=token=./pat.txt
```

All callers share this one GitHub identity and its rate limits. The [audit log](#audit-log) tells you who did what.

The proxy reads the PAT from a mounted file and reloads it when the file changes. The proxy never logs the PAT, never returns it to a client and never sends it anywhere except GHCR.

## Make the TLS certificate

CI pods connect to the proxy under the name `ghcr.io`. Thus the proxy needs a certificate for `ghcr.io`. You issue it from a private CA. The proxy only serves the certificate. It never issues or rotates it.

Warning: the private CA must have a name constraint that limits it to `ghcr.io`. Without the constraint, anyone who holds the CA key can make a valid certificate for any host.

Warning: trust this CA only through the Docker directory `/etc/docker/certs.d/ghcr.io/ca.crt`. Never add it to the system trust store of a pod or a node. The `certs.d` directory limits the CA to one registry name. The system trust store does not.

This example uses cert-manager. The CA certificate carries the name constraint through `nameConstraints`.

```yaml
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: selfsigned
  namespace: ghcr-proxy
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: ghcr-proxy-ca
  namespace: ghcr-proxy
spec:
  isCA: true
  commonName: ghcr-proxy-ca
  secretName: ghcr-proxy-ca
  privateKey:
    algorithm: ECDSA
    size: 256
  nameConstraints:
    critical: true
    permitted:
      dnsDomains: [ghcr.io]
  issuerRef:
    name: selfsigned
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: ghcr-proxy-ca
  namespace: ghcr-proxy
spec:
  ca:
    secretName: ghcr-proxy-ca
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: ghcr-proxy-tls
  namespace: ghcr-proxy
spec:
  secretName: ghcr-proxy-tls
  dnsNames: [ghcr.io]
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    name: ghcr-proxy-ca
```

The `nameConstraints` field needs the `NameConstraints` feature gate of cert-manager. Make sure that it is on in your cert-manager version. If you use another tool, make sure that the CA certificate carries a permitted DNS name of `ghcr.io` and no other.

The Secret `ghcr-proxy-tls` has the type `kubernetes.io/tls`. The chart mounts it. The proxy reloads the certificate when the files change. If a reload fails, the proxy keeps the old certificate and logs an error.

The file `ca.crt` in the Secret `ghcr-proxy-tls` is the CA that CI pods must trust. [CI pod setup](ci-pods.md) shows where it goes.

## Plan the grants

A grant caps what policies in a set of namespaces can allow. A `GHCRAccessPolicy` can never allow more than its grant. A namespace without a grant only gets anonymous pulls.

Each grant has these fields:

- `namespaces`: A regular expression (RE2). The proxy anchors it, so it must match the whole namespace name.
- `repositories`: A list of repository patterns. A pattern can use `*` inside one path segment and `**` for whole segments. It can also use `${name}` for a named capture of `namespaces`.
- `actions`: Any of `pull`, `push` and `delete`. Both `push` and `delete` include `pull`.

Example: each namespace `ci-<name>` can use the repositories `soliddowant/<name>` and everything below it.

```yaml
grants:
  - namespaces: "ci-(?P<repo>[a-z0-9-]+)"
    repositories:
      - soliddowant/${repo}
      - soliddowant/${repo}/**
    actions: [pull, push, delete]
```

A grant does not apply to a namespace if a capture is empty or expands to a value outside `[a-z0-9._-]+`. A grant that does not apply never widens anything. If several grants match a namespace, each one adds its patterns and actions.

The proxy reads the grants at start only. The chart restarts the Deployment when you change them. During a rolling restart, replicas can run different grants for a short time. The status of a policy shows the grants of the leader replica, and it settles when the restart ends.

## Install the chart

Create the Secrets first. Then install the chart in the namespace that holds them.

```
helm install ghcr-proxy oci://ghcr.io/soliddowant/charts/ghcr-proxy \
  --namespace ghcr-proxy \
  --set pat.existingSecret=ghcr-proxy-pat \
  --set tls.existingSecret=ghcr-proxy-tls \
  --set service.clusterIP=10.96.0.50 \
  --values grants.yaml
```

Pick a free address in the service range of your cluster for `service.clusterIP`. CI pods map `ghcr.io` to this fixed address with `hostAliases`, so it must not change. `grants.yaml` holds the `grants` list from the section above under the key `grants`.

The chart installs the `GHCRAccessPolicy` CRD, the Deployment (two replicas), the Service and the RBAC. The chart never creates the PAT or TLS Secrets. [Chart values](chart-values.md) lists every value.

Make sure that the install works:

1. Run `kubectl -n ghcr-proxy rollout status deploy/ghcr-proxy`.
2. Make sure that `/readyz` on port 8080 of a proxy pod answers `200`. The proxy is ready when it loaded the PAT and the certificate and synced the policies.

If the proxy cannot decide, it fails closed. It denies the request or forwards it with no credential. It never uses the PAT as a fallback.

## Security limits

- A synced replica keeps the last policies it saw. During an outage of the Kubernetes API server, a revoked policy keeps working until the watch recovers.
- The proxy caches a token review for up to 30 seconds (`authentication.cacheTTL`). A deleted ServiceAccount can keep working for that time.
- All callers share one GitHub identity and its rate limits.
- ServiceAccount tokens must use the audience `ghcr-proxy` and a short life of 1 to 6 hours. Do not accept the default API server audience.

## Audit log

The proxy writes one JSON line to standard output for each request. This includes `404`, `401` and `503` answers. The lines never hold header values, tokens or query strings. Set `audit.enabled` to `false` to turn the log off.

These fields are in each line:

| Field | Meaning |
|---|---|
| `time` | When the request ended. |
| `clientIP` | The address of the client. |
| `namespace` | The namespace of the ServiceAccount. Empty for an anonymous request. |
| `serviceAccount` | The name of the ServiceAccount. |
| `pod` | The pod that the token is bound to, when the token review reports it. |
| `method` | The HTTP method. |
| `path` | The escaped request path, without the query. |
| `repository` | The repository of the request. |
| `from` | The source repository of a blob mount that the proxy forwarded. |
| `action` | `none`, `pull`, `push`, `delete` or `unknown`. |
| `decision` | The decision. See the next table. |
| `policy` | The name of the policy that allowed the request. |
| `status` | The HTTP status that the client got. |
| `upstreamStatus` | The HTTP status from GHCR, when the request reached it. |
| `bytes` | The number of body bytes sent to the client. |
| `durationMs` | How long the request took, in milliseconds. |
| `digest` | The manifest digest, for a manifest push or delete. |

The `decision` field has one of these values:

| Value | Meaning |
|---|---|
| `allowed` | A policy and its grant allowed the request. The proxy used the PAT. |
| `anonymous` | The proxy forwarded the request with no credential (a pull that no policy allows). |
| `denied` | A policy or grant did not allow the request. The client got `403`. |
| `unauthenticated` | The credential was missing or not valid. The client got `401`. |
| `unavailable` | The proxy could not decide or reach GHCR. The client got `502` or `503`. |
| `notfound` | The route is not one that the proxy serves. The client got `404`. |
| `invalid` | The request was malformed. The client got `400`. |

To find out why a push failed, search for the `serviceAccount` and `repository` and read `decision` and `status`. To see how clients read the answers, see [why a push is denied](../user/policies.md#why-a-push-is-denied).
