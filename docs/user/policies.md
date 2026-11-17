# Write a RegistryAccessPolicy

For: people who write a `RegistryAccessPolicy` for a CI namespace, for example in a Helm chart. You know Kubernetes YAML and Docker.

A `RegistryAccessPolicy` says which ServiceAccounts in its namespace can pull, push or delete which repositories through the proxy. Each rule uses a credential or a mirror. Your admin makes the credentials. The proxy sends the credential to the registry, so your pods never get it. A rule can never allow more than the ceiling of its credential.

## Write a policy

The policy lives in the namespace of the pod. It lists the ServiceAccounts of that namespace and the rules for them. Each rule has a name that is unique in the policy, and exactly one of `credentialRef` and `mirrorRef`.

```yaml
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
      repositories:
        - soliddowant/foo
        - soliddowant/foo/**
      actions: [pull, push]
```

- `serviceAccounts`: One to 64 names of ServiceAccounts in the same namespace. A policy cannot name a ServiceAccount in another namespace.
- `rules`: One to 64 rules.
- `credentialRef`: The `UpstreamCredential` that the rule uses. Without a `namespace`, the proxy looks in the namespace of the policy. Ask your admin for the name, the namespace and the ceiling of the credential.
- `repositories`: One to 32 patterns for repository names without the registry host. `*` matches any characters inside one path segment. `**` matches any number of whole segments. A pattern can use only the characters `a-z`, `0-9`, `.`, `_` and `-`.
- `actions`: `pull`, `push` or `delete`. The actions `push` and `delete` include `pull`. GHCR does not let you delete by tag. The proxy then answers `405` with the code `UNSUPPORTED`.

The credential must admit your namespace in its `allowedNamespaces`, and so must the upstream that the credential points at. A `selector: {}` in `allowedNamespaces` admits every namespace in the cluster, not only yours. If you are an admin, use it only when you mean it.

The [field reference](crd-reference.md) lists every field.

## Use a mirror

A mirror serves pulls from a pull-through cache, for example a Harbor project for your namespace. A rule with a `mirrorRef` allows only `pull`. The mirror serves public content only. Pull private images with a credential rule.

You can create the `RegistryMirror` in your own namespace when your admin allows it. The mirror needs these things:

- The upstream that it copies must admit your namespace, and must list the URL of your mirror in its `allowedMirrorURLs`. Ask your admin.
- A Secret in your namespace with the keys `username` and `password` of the mirror (see [`RegistryMirror`](../admin/mirrors.md)).
- A Role that lets the proxy read that one Secret.

Without the Role, the mirror gets `Ready=False` with the reason `SecretUnavailable`. The proxy has no read access to Secrets in your namespace until you give it. Name the Secret in `resourceNames`, so that the proxy can read only that Secret. The proxy runs as the ServiceAccount `registrygate` in the namespace `registrygate` with the default chart values. Use the name of your install if it differs.

```yaml
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
```

The proxy retries when the Role is missing. It picks up a Role that you add later without a restart.

Then the rule of the policy names the mirror:

```yaml
    - name: pull-through
      mirrorRef:
        name: harbor
      repositories: ["**"]
      actions: [pull]
```

## Read the status

The proxy writes `status.conditions` for the policy and `status.rules` for each rule. Run `kubectl get registryaccesspolicy -n ci-foo` to see the `Ready` condition and its reason. A rule that is not accepted is ignored. The other rules of the policy still apply. [What the status means](policy-status.md) explains each reason and how to fix it.

A change to a policy applies to the next request.

## Why a request is denied

When the proxy denies a request, Docker shows `403` with the code `DENIED` and a message. The message names your namespace, ServiceAccount, repository and action. Common causes:

- No rule in a policy of your namespace allows this action on this repository for your ServiceAccount. Add a rule.
- A rule exists but is not accepted. Read the status of the policy. The usual reasons are `OutsideCeiling` (the credential does not allow that much), `NotAllowed` (the credential or the upstream does not admit your namespace) and `RefNotFound` (the credential does not exist).

If a pull is not allowed by any rule, the proxy sends it to the registry with no credential, or to the default mirror. Only public images work then.

## Ambiguous requests

Two rules are ambiguous when both match a request but name different credentials. The proxy then denies the request with `403` and the code `DENIED`. The message is `ambiguous rules:` and a list of `namespace/policy/rule` names, at most five. Change one rule so that only one credential matches the repository, or make the patterns narrower. Rules have no order, so the proxy never picks one for you.

## Other errors

- `401`: The pod has no valid ServiceAccount token. The token must have the audience `registrygate`. Ask your admin to check the pod setup.
- `503` with `registrygate is not ready`: The proxy is starting or cannot review tokens. Try again.
- `502`: The proxy cannot get a token from the registry, cannot reach it, or a mirror failed with `mirror unavailable`. Ask your admin.

Your admin can find each request in the audit log. See [the audit log](../admin/install.md#audit-log).
