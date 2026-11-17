# Shared credentials

For: admins who give CI namespaces access to private repositories. You know Kubernetes. You already made an [upstream registry](upstreams.md).

An `UpstreamCredential` holds one login for an upstream, for example a GitHub personal access token (PAT) for `ghcr.io`. It also holds a ceiling: the most that any policy can do with that login. A caller never sees the login. The proxy sends it only to the upstream.

## Create the PAT Secret

GitHub has no API that makes GHCR credentials, so you make the PAT by hand.

1. Create a GitHub machine-user account that is only for this proxy.
2. Give the machine user access to the packages that CI must push to.
3. Create a classic PAT for that account with the scopes `read:packages`, `write:packages` and `delete:packages`.
4. Store the PAT in a Secret in the namespace of the `UpstreamCredential`. The Secret needs the keys `username` and `password`. This example reads the PAT from the file `pat.txt` and the user name from the variable `GITHUB_USER`.

<!-- e2e -->
```sh
kubectl -n registrygate create secret generic ghcr-pat \
  --from-literal=username="${GITHUB_USER}" \
  --from-file=password=./pat.txt
```

To use other key names, set `usernameKey` and `passwordKey` in `secretRef`.

The proxy never logs the PAT, never returns it to a client and never sends it anywhere except the upstream. It reads the Secret again when it changes.

## Create the UpstreamCredential

The `ceiling` is a list of repository patterns with the actions that are allowed on them. A pattern can use `*` inside one path segment and `**` for whole segments. The actions are `pull`, `push` and `delete`. The actions `push` and `delete` include `pull`, so a ceiling with `push` also needs `pull` for the same repositories. List `pull` with `push`, as in the example.

This credential lets policies in `ci-foo` use the repositories `soliddowant/foo` and everything below it:

<!-- e2e -->
```sh
kubectl apply -f - <<'EOF'
apiVersion: registrygate.soliddowant.dev/v1alpha1
kind: UpstreamCredential
metadata:
  name: ghcr-pat
  namespace: registrygate
spec:
  upstreamRef:
    name: ghcr
  secretRef:
    name: ghcr-pat
  ceiling:
    - repositories:
        - soliddowant/foo
        - soliddowant/foo/**
      actions: [pull, push, delete]
  allowedNamespaces:
    names: [ci-foo]
EOF
```

- `upstreamRef`: The `UpstreamRegistry`. With no `namespace`, the proxy looks in the namespace of the credential. The upstream must admit the namespace of the credential in its `allowedNamespaces`.
- `ceiling`: One to 32 entries. A policy rule that asks for more is not accepted.
- `allowedNamespaces`: The namespaces whose policies can use this credential. Without it, only policies in the namespace of the credential can use it. A `selector` of `{}` admits every namespace, so use it with care.

To cover many namespaces with one credential, use a label selector, and give the credential a ceiling that fits all of them. To give each namespace its own repositories, make one credential for each namespace.

A policy in a namespace uses a credential only if the credential admits the namespace and the upstream admits it too. All callers of one credential share one upstream identity and its rate limits. The [audit log](install.md#audit-log) shows which ServiceAccount made each request.

Make sure that the credential is ready:

<!-- e2e -->
```sh
kubectl -n registrygate wait --for=condition=Ready upstreamcredential/ghcr-pat --timeout=120s
```

## Delete

A delete is a normal `DELETE` request that the proxy sends with the credential. GHCR does not let you delete by tag, and the proxy passes on its answer. [GHCR behaviour](../dev/ghcr-behaviour.md) records what GHCR answers.
