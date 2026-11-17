# Mirrors

For: admins who set up pull-through caches, such as a Harbor project for each CI namespace. You know Kubernetes and Harbor. You already made an [upstream registry](upstreams.md).

A `RegistryMirror` object points at a registry that keeps copies of the images of an upstream. A policy rule with a `mirrorRef` sends the pulls of a ServiceAccount to the mirror. The proxy sends the credential of the mirror, and never a credential of the upstream.

## Mirrors serve public content only

A mirror must have no upstream credential that can read private images.

A Harbor proxy-cache project fetches images from its upstream with the credential of its registry endpoint. If that credential can read private images, anyone who holds the robot account of the project can read them through Harbor. Harbor then skips the policy of the proxy. So create the registry endpoint of the project with no upstream credential. The proxy cannot check this for you.

To pull private images, use an [`UpstreamCredential`](credentials.md). A credential never uses a mirror.

## Allow the mirror URLs on the upstream

Add the URL prefixes that mirrors of this upstream can use to the `UpstreamRegistry`:

<!-- e2e -->
```sh
kubectl -n registrygate patch upstreamregistry ghcr --type merge \
  --patch '{"spec":{"allowedMirrorURLs":["https://harbor.example.com/"]}}'
```

A mirror URL matches a prefix when the scheme and host are equal and the path of the prefix is a prefix of the path of the mirror on a `/` boundary. A mirror with another URL gets `Ready=False` with the reason `URLNotAllowed`, and the proxy never connects to it.

A mirror can use plain HTTP only if you allow it here. An `http://` mirror URL matches only an `http://` prefix, and an `https://` prefix matches only an `https://` mirror URL. For Harbor through its Service, allow for example `http://harbor-core.harbor.svc/`.

Warning: use plain HTTP only inside a network that you trust. The proxy sends the credential of the mirror and its tokens unencrypted. The mirror stays ready, and its status message says `plain HTTP: credentials cross the cluster network unencrypted`.

## Create a RegistryMirror

An admin creates the object, or the tenant creates it in its own namespace. This example makes a mirror for the namespace `ci-foo`. The Harbor project `ci-foo-ghcr` caches `ghcr.io`. The Secret `harbor-ci-foo` holds the keys `username` and `password` of a Harbor robot account for that project. This example reads them from the variables `HARBOR_USER` and `HARBOR_PASSWORD`.

The proxy has no read access to Secrets in the namespace `ci-foo` yet. Make the Secret, and a Role that lets the proxy read that one Secret. Use the namespace and the ServiceAccount name of your install if they differ:

<!-- e2e -->
```sh
kubectl -n ci-foo create secret generic harbor-ci-foo \
  --from-literal=username="${HARBOR_USER}" \
  --from-literal=password="${HARBOR_PASSWORD}"
kubectl apply -f - <<'EOF'
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: registrygate-mirror-secret
  namespace: ci-foo
rules:
  - apiGroups: [""]
    resources: [secrets]
    resourceNames: [harbor-ci-foo]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: registrygate-mirror-secret
  namespace: ci-foo
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: registrygate-mirror-secret
subjects:
  - kind: ServiceAccount
    name: registrygate
    namespace: registrygate
EOF
```

Then make the mirror:

<!-- e2e -->
```sh
kubectl apply -f - <<'EOF'
apiVersion: registrygate.soliddowant.dev/v1alpha1
kind: RegistryMirror
metadata:
  name: harbor
  namespace: ci-foo
spec:
  upstreamRef:
    name: ghcr
    namespace: registrygate
  url: https://harbor.example.com
  pathPrefix: ci-foo-ghcr
  secretRef:
    name: harbor-ci-foo
EOF
```

- `upstreamRef`: The `UpstreamRegistry` that the mirror copies. It must admit the namespace of the mirror in its `allowedNamespaces`.
- `url`: The scheme, host and port of the registry of the mirror. It has no query and no user information.
- `pathPrefix`: The path under which the mirror serves the upstream. With `ci-foo-ghcr`, the image `soliddowant/foo` is `ci-foo-ghcr/soliddowant/foo` on the mirror.
- `secretRef`: A Secret in the namespace of the mirror with the credential of the mirror. Leave it out if the mirror needs none.
- `caSecretRef`: A Secret with the key `ca.crt`, for a mirror with a private CA.
- `tokenRealmHosts`: Hosts that can serve as the token endpoint of the mirror, besides the host of `url`. They must use the scheme of `url`. The proxy refuses any other token endpoint, and the mirror then fails.
- `aliasHosts`: Other names of the mirror, for example the `externalURL` host of Harbor when `url` is its Service `http://harbor-core.harbor.svc`. The proxy treats a token endpoint or a `Location` on an alias host as if it were on `url`, and never sends a request to the alias host. See [Harbor through its Service](upstreams.md#harbor-through-its-service).
- `fallbackToUpstream`: If `true` (the default), a pull goes to the upstream with no credential when the mirror fails.
- `timeouts`: The `responseHeader` timeout also decides when the mirror counts as failed.

The proxy never sends the credential of an upstream to a mirror. It never sends the credential of a mirror to an upstream.

The proxy reads the Secrets in the admin namespaces. For a Secret in a tenant namespace, the tenant creates a Role that names that one Secret. See [tenant mirrors](../user/policies.md#use-a-mirror).

## Send pulls to the mirror

Add a rule with a `mirrorRef` to the policy of the namespace. This policy keeps the push rule and lets the ServiceAccount pull `soliddowant/public` from the mirror:

<!-- e2e -->
```sh
kubectl apply -f - <<'EOF'
apiVersion: registrygate.soliddowant.dev/v1alpha1
kind: RegistryAccessPolicy
metadata:
  name: builder
  namespace: ci-foo
spec:
  serviceAccounts: [builder]
  rules:
    - name: push-foo
      credentialRef:
        name: ghcr-pat
        namespace: registrygate
      repositories: [soliddowant/foo]
      actions: [pull, push]
    - name: pull-public
      mirrorRef:
        name: harbor
      repositories: [soliddowant/public]
      actions: [pull]
EOF
```

Wait until the mirror and the policy are ready:

<!-- e2e -->
```sh
kubectl -n ci-foo wait --for=condition=Ready registrymirror/harbor registryaccesspolicy/builder --timeout=120s
```

A pull in a pod of that ServiceAccount, for example the pod from [CI pods](ci-pods.md), now comes through the mirror:

<!-- e2e -->
```sh
kubectl -n ci-foo exec build -c docker -- docker pull ghcr.io/soliddowant/public:latest
```

## Default mirror

Set `defaultMirrorRef` on the `UpstreamRegistry` to a mirror of that same upstream. The proxy then sends pulls that no rule decides to that mirror. These are the pulls of a caller with no matching rule, and anonymous pulls when `anonymousPulls` is `Forward`. The mirror must be ready and must admit the namespace of the upstream. If it is not usable, the upstream stays ready and these pulls go to the upstream with no credential.

## What goes to the mirror

- Manifest pulls (`GET` and `HEAD`) and blob pulls (`GET`) go to the mirror.
- Blob checks (`HEAD` on a blob), tag lists and referrers go to the upstream with no credential. `docker push` uses blob checks to decide which layers to upload, and a mirror can keep a blob that the upstream no longer has.
- A mirror fails when the proxy cannot connect to it, when its response headers do not arrive in time, or when it answers `5xx` or `404`. With `fallbackToUpstream: true`, the pull then goes to the upstream with no credential. With `false`, the client gets `502` with the message `mirror unavailable`.
- If a mirror answers `401` or `403` to a rule, the client gets `502` with the message `mirror unavailable`. The pull does not go to the upstream, because you must fix the credential of the mirror.
- If the mirror of a rule is not ready, that rule still applies. The pull follows the fallback rule above. It never moves to the default mirror.
- A pull that the proxy serves with no rule (the default mirror, or the upstream) can end in `401`, `403` or `404`. The client then gets the Basic challenge, so a private base image can retry with its token.
