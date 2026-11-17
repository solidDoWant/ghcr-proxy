# Upstream registries

For: admins who tell registrygate which registry hosts it stands in for. You know Kubernetes and cert-manager. You already installed the chart (see [Install registrygate](install.md)).

An `UpstreamRegistry` object names one or more registry hosts, for example `ghcr.io`, and the real registry that serves them. CI pods connect to the proxy under that host name. The proxy needs a TLS certificate for the host. You make the certificate from one private CA. A name constraint limits that CA to the hosts that the proxy intercepts (the hosts that CI pods send to the proxy).

You create an `UpstreamRegistry` in an admin namespace only. By default, this is the namespace of the proxy. The proxy ignores one in any other namespace, and the admission webhook rejects it.

## One CA for all intercepted hosts

Use one CA for all the hosts that the proxy intercepts, and one serving certificate for each `UpstreamRegistry`.

Warning: limit the CA to exactly the intercepted hosts with a name constraint. Without the constraint, anyone who holds the CA key can make a valid certificate for any host.

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

This example makes a CA and a certificate for the host `ghcr.io`. The CA can sign only `ghcr.io` and its subdomains. If the proxy intercepts more hosts, put all of them in `dnsDomains` of the CA, for example `[ghcr.io, harbor.example.com, registry-1.docker.io, docker.io, index.docker.io]`. Make the objects in the namespace of the proxy, because the `UpstreamRegistry` reads the certificate Secret from its own namespace.

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

For a second upstream, for example `quay.io`, add its hosts to `dnsDomains` of the CA. A CA cannot change its name constraint, so cert-manager must issue the CA again: delete its Secret, and cert-manager makes a new one. Then make a second serving certificate for the new hosts from the same CA, and give the new `ca.crt` to the CI pods.

For example, this makes the CA, the certificate and the `UpstreamRegistry` for `quay.io`.

<!-- e2e -->
```sh
kubectl apply -f - <<'EOF'
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: quay-io-ca
  namespace: registrygate
spec:
  isCA: true
  commonName: quay-io-ca
  secretName: quay-io-ca
  privateKey:
    algorithm: ECDSA
    size: 256
  nameConstraints:
    critical: true
    permitted:
      dnsDomains: [quay.io]
  issuerRef:
    name: selfsigned
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: quay-io-ca
  namespace: registrygate
spec:
  ca:
    secretName: quay-io-ca
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: quay-io-tls
  namespace: registrygate
spec:
  secretName: quay-io-tls
  dnsNames: [quay.io]
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    name: quay-io-ca
---
apiVersion: registrygate.soliddowant.dev/v1alpha1
kind: UpstreamRegistry
metadata:
  name: quay
  namespace: registrygate
spec:
  hosts: [quay.io]
  url: https://quay.io
  tls:
    secretRef:
      name: quay-io-tls
  anonymousPulls: Challenge
EOF
kubectl -n registrygate wait --for=condition=Ready certificate/quay-io-tls upstreamregistry/quay --timeout=120s
```

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
- `url`: Where the proxy sends requests. It has a scheme (`https` or `http`) and a host, and no path. See [Plain HTTP](#plain-http).
- `aliasHosts`: Other names of the registry at `url`. See [Harbor through its Service](#harbor-through-its-service).
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

## Plain HTTP

The proxy can connect to an upstream over plain HTTP, for example to a registry Service in the cluster. Set `url` to `http://host[:port]`, and do not set `caSecretRef`. If `caSecretRef` is set, the object gets `Ready=False` with the reason `InvalidSpec`.

Warning: use plain HTTP only inside a network that you trust. The proxy sends the credential, the tokens and the images unencrypted. The object stays ready, and its status message says `plain HTTP: credentials cross the cluster network unencrypted`.

CI pods still connect to the proxy over TLS with the certificate of the `UpstreamRegistry`.

## Harbor through its Service

Harbor knows itself by its `externalURL`, for example `https://harbor.example.com`. It puts that host in its token endpoint and in its `Location` headers. If the proxy connects to Harbor through its Service (`http://harbor-core.harbor.svc`), list the `externalURL` host in `aliasHosts`. In this example, CI pods use the name `registry.example.com` for Harbor:

```yaml
apiVersion: registrygate.soliddowant.dev/v1alpha1
kind: UpstreamRegistry
metadata:
  name: harbor
  namespace: registrygate
spec:
  hosts: [registry.example.com]
  url: http://harbor-core.harbor.svc
  aliasHosts: [harbor.example.com]
  tls:
    secretRef:
      name: registry-example-com-tls
  allowedNamespaces:
    names: [ci-foo]
```

The proxy treats a token endpoint or a `Location` on an alias host as if it were on `url`. It keeps the path and the query and replaces the scheme and the host with those of `url`. The proxy never sends a request to an alias host. An entry is a host name, or a host name and a port. An entry without a port matches the default port of the scheme.

`aliasHosts` must not overlap `hosts`, because the proxy cannot stand in for a name and also treat it as another name of `url`. If an entry names a host in `hosts`, the object gets `Ready=False` with the reason `InvalidSpec`.

### Push to Harbor with a robot account that the CI pod never sees

A CI pod can push to a Harbor project with a robot account whose Secret is only in the admin namespace:

1. Create the `UpstreamRegistry` for Harbor in the admin namespace, as above.
2. For each project, create the robot account in the admin namespace. With harbor-operator, a `HarborProjectRobot` puts its Secret in the admin namespace. The `HarborProject` must admit the robot account.
3. For each project, create an [`UpstreamCredential`](credentials.md) in the admin namespace that reads that Secret. Give it the ceiling `<project>/**` for `pull`, `push` and `delete`, and admit only the namespace of the project in `allowedNamespaces`.
4. In the namespace of the project, create a policy with a credential rule for `<project>/**`.

The pods of the project send the Harbor host to the proxy like any other host. So they cannot reach the API of Harbor. If a project needs the API, do not send the Harbor host to the proxy for that project.

## Docker Hub

Docker clients use three names for Docker Hub. One `UpstreamRegistry` must have all three:

```yaml
apiVersion: registrygate.soliddowant.dev/v1alpha1
kind: UpstreamRegistry
metadata:
  name: docker-hub
  namespace: registrygate
spec:
  hosts: [registry-1.docker.io, docker.io, index.docker.io]
  url: https://registry-1.docker.io
  tokenRealmHosts: [auth.docker.io]
  tls:
    secretRef:
      name: docker-hub-tls
```

- The Docker daemon sends `docker.io/<image>` and short names like `alpine` to `registry-1.docker.io`. It reads the CA from `/etc/docker/certs.d/registry-1.docker.io/ca.crt`.
- The Docker CLI and BuildKit look up the Docker Hub credential under `https://index.docker.io/v1/`. The credential helper maps that to `index.docker.io`.
- The `hostAliases` of the CI pod and `REGISTRYGATE_HOSTS` must name all three hosts. See [CI pod setup](ci-pods.md).
- The serving certificate must be valid for all three hosts.
- The proxy does not change repository names. The daemon sends `library/<name>` for official images, so policy rules name `library/alpine`, not `alpine`.
