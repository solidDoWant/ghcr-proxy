# Write a GHCRAccessPolicy

For: people who write a `GHCRAccessPolicy` for a CI namespace, for example in a Helm chart. You know Kubernetes YAML and Docker.

A `GHCRAccessPolicy` says which ServiceAccounts in its namespace can pull, push or delete which GHCR repositories through the proxy. The proxy uses its own GitHub credential for these requests, so your pods never get one. Your admin sets the limit for your namespace (the grant), and a policy can never allow more than that.

## Write a policy

The policy lives in the namespace of the pod. It lists ServiceAccounts of that namespace and the rules for them.

```yaml
apiVersion: ghcr-proxy.soliddowant.dev/v1alpha1
kind: GHCRAccessPolicy
metadata:
  name: builder
  namespace: ci-foo
spec:
  serviceAccounts: [builder]
  rules:
    - repositories:
        - soliddowant/foo
        - soliddowant/foo/**
      actions: [pull, push]
```

- `serviceAccounts`: One to 64 names of ServiceAccounts in the same namespace. A policy cannot name a ServiceAccount in another namespace.
- `rules`: One to 32 rules. Each rule has `repositories` and `actions`.
- `repositories`: Patterns for repository names without the registry host. `*` matches any characters inside one path segment. `**` matches any number of whole segments. A pattern can use only the characters `a-z`, `0-9`, `.`, `_` and `-`.
- `actions`: `pull`, `push` or `delete`. The actions `push` and `delete` include `pull`.

The [field reference](crd-reference.md) lists every field.

## Read the status

The proxy writes `status.conditions` and `status.rejected`. Run `kubectl get gap -n ci-foo` to see the `Ready` condition and its reason. If `Ready` is not `True`, the proxy ignores part or all of the policy. [What the status means](policy-status.md) explains each reason and how to fix it.

A change to a policy applies to the next request.

## Why a push is denied

When the proxy denies a push or a delete, Docker shows `403` with the code `DENIED` and a message. The message names your namespace, ServiceAccount, repository and action. It also gives one cause:

- `no GHCRAccessPolicy in the namespace allows it`: Add a rule for this ServiceAccount, repository and action to a policy in the same namespace.
- `the namespace's grant does not cover it`: The policy asks for more than the grant. Remove the pattern or ask your admin to change the grant.

If Docker shows another error, check this list:

- `401`: The pod has no valid ServiceAccount token. The token must have the audience `ghcr-proxy`. Ask your admin to check the pod setup.
- `503`: The proxy is not ready or cannot review tokens. Try again.
- `502`: The proxy cannot get a credential from GHCR or cannot reach GHCR. Ask your admin.

Your admin can find each request in the audit log. See [the audit log](../admin/install.md#audit-log).
