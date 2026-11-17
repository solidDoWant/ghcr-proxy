# Upstream registries

For: admins who tell registrygate which registry hosts it stands in for. You know Kubernetes and cert-manager. You already installed the chart (see [Install registrygate](install.md)).

An `UpstreamRegistry` object names one or more registry hosts, for example `ghcr.io`, and the real registry that serves them. CI pods connect to the proxy under that host name. The proxy needs a TLS certificate for the host. You make the certificate from a private CA, one CA for each upstream.

You create an `UpstreamRegistry` in an admin namespace only. By default, this is the namespace of the proxy. The proxy ignores one in any other namespace, and the admission webhook rejects it.

## One CA and one certificate for each upstream

Warning: limit each CA to the hosts of its upstream with a name constraint. Without the constraint, anyone who holds the CA key can make a valid certificate for any host.

Warning: trust the CA only through the Docker directory `/etc/docker/certs.d/<host>/ca.crt`. Never add it to the system trust store of a pod or a node. The `certs.d` directory limits the CA to one registry name. The system trust store does not.

The proxy only serves the certificate. It never issues or renews it. cert-manager does that.

Name constraints need the `NameConstraints` feature gate in cert-manager. It must be on in the controller and in the webhook of cert-manager. If cert-manager is not in your cluster, install it from its Helm chart and pin the version:

<!-- e2e -->
```sh
helm install cert-manager oci://quay.io/jetstack/charts/cert-manager \
  --version v1.19.2 \
  --namespace cert-manager --create-namespace \
  --set crds.enabled=true \
  --set featureGates=NameConstraints=true \
  --set webhook.featureGates=NameConstraints=true \
  --wait
```

This example makes a CA and a certificate for the host `ghcr.io`. The CA can sign only `ghcr.io` and its subdomains. Make the objects in the namespace of the proxy, because the `UpstreamRegistry` reads the certificate Secret from its own namespace.

<!-- e2e -->
```sh
kubectl apply -f - <<'EOF'
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: selfsigned
  namespace: registrygate
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: ghcr-io-ca
  namespace: registrygate
spec:
  isCA: true
  commonName: ghcr-io-ca
  secretName: ghcr-io-ca
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
  name: ghcr-io-ca
  namespace: registrygate
spec:
  ca:
    secretName: ghcr-io-ca
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: ghcr-io-tls
  namespace: registrygate
spec:
  secretName: ghcr-io-tls
  dnsNames: [ghcr.io]
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    name: ghcr-io-ca
EOF
```

Wait until cert-manager issues the certificate:

<!-- e2e -->
```sh
kubectl -n registrygate wait --for=condition=Ready certificate/ghcr-io-tls --timeout=120s
```

The Secret `ghcr-io-tls` holds the key pair (`tls.crt` and `tls.key`) and the CA certificate (`ca.crt`). CI pods must trust `ca.crt`. [CI pod setup](ci-pods.md) shows where it goes.

For a second upstream, for example `quay.io`, make a second CA and a second certificate with its own name constraint. Never share a CA between upstreams.

## Create the UpstreamRegistry

This object stands in for `ghcr.io` and sends the requests to the real `https://ghcr.io`:

<!-- e2e -->
```sh
kubectl apply -f - <<'EOF'
apiVersion: registrygate.soliddowant.dev/v1alpha1
kind: UpstreamRegistry
metadata:
  name: ghcr
  namespace: registrygate
spec:
  hosts: [ghcr.io]
  url: https://ghcr.io
  tls:
    secretRef:
      name: ghcr-io-tls
  anonymousPulls: Challenge
  allowedNamespaces:
    names: [ci-foo]
EOF
```

These fields matter most:

- `hosts`: The host names that the proxy answers for. Each host belongs to at most one `UpstreamRegistry`. If two objects claim the same host, both get `Ready=False` with the reason `HostConflict`, and the proxy serves neither.
- `url`: Where the proxy sends requests. It has a scheme and a host, and no path.
- `tls.secretRef`: The Secret with the serving key pair. The certificate must be valid for every host in `hosts`. If it is not, the object gets the reason `CertificateMismatch`.
- `caSecretRef`: An optional Secret with the key `ca.crt`. Use it when the real registry has a certificate from a private CA.
- `tokenRealmHosts`: Hosts that can serve as the token endpoint of the registry, besides the host of `url`. The proxy refuses any other token endpoint.
- `allowedNamespaces`: The namespaces that can point a credential, a mirror or a policy at this upstream. It takes a list of `names` and a label `selector`. With no value, only the namespace of the upstream is allowed. A `selector` of `{}` allows every namespace.
- `defaultMirrorRef` and `allowedMirrorURLs`: See [mirrors](mirrors.md).
- `timeouts`: How long the proxy waits for response headers (`responseHeader`, 30 seconds by default) and for the token exchange (`token`, 10 seconds by default).

The proxy reads the Secrets again when they change. A renewed certificate applies to new connections.

Make sure that the object is ready:

<!-- e2e -->
```sh
kubectl -n registrygate wait --for=condition=Ready upstreamregistry/ghcr --timeout=120s
```

If it is not ready, read the reason with `kubectl -n registrygate get upstreamregistry ghcr -o yaml`. [What the status means](../user/policy-status.md#reasons-for-all-kinds) lists the reasons.

## Pulls with no token: anonymousPulls

BuildKit and the containerd image store try each pull with no credential first. They call the credential helper only after a `401`. If the proxy served those pulls, it would never learn who the caller is.

- `Challenge` (the default): The proxy answers every request with no `Authorization` header with `401` and a Basic challenge. The client then retries with its ServiceAccount token. Use this when the pods that intercept the host have the credential helper. A pod with no helper cannot pull from the host.
- `Forward`: The proxy serves a pull with no credential itself. It uses the default mirror of the upstream, or the upstream with no credential. Use this when pods without the helper must pull from the host. Mirrors of a namespace then see only the clients that authenticate first.

An authenticated caller that no rule covers gets the same answer in both modes: the default mirror or the upstream with no credential.

## Projects that need the API of the registry

The proxy passes only the registry API (`/v2`). If a project needs another API of the registry, do not intercept that host. A pod that calls such an API on an intercepted host gets `404`, because the proxy serves no other path. Add the `hostAliases` entry only to pods that push and pull images, and let other pods reach the real registry.
