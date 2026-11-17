# What the status of an object means

For: people who write a `RegistryAccessPolicy`, a `RegistryMirror` or an `UpstreamCredential`. You know Kubernetes YAML.

The proxy writes the status of each object. It uses the same code to write the status and to decide about requests. Thus the status always says what the proxy does.

Each object has `status.observedGeneration` and a `Ready` condition. If `observedGeneration` is lower than `metadata.generation`, the proxy did not process your last change yet. A `RegistryAccessPolicy` also has `status.rules`, with one entry for each rule and an `Accepted` condition.

## Reasons for all kinds

| Reason | Meaning |
|---|---|
| `Ready` | The object is ready and the proxy uses it. |
| `InvalidSpec` | A field has a value that the proxy cannot use, for example an `http` URL. The message names it. |
| `NotAllowed` | The upstream, or the object that you refer to, does not admit the namespace of this object in its `allowedNamespaces`. |
| `RefNotFound` | An object that you refer to does not exist. |
| `RefNotReady` | An object that you refer to exists but is not ready. Look at its status. |
| `SecretUnavailable` | A Secret does not exist, or the proxy may not read it. For a tenant namespace, add the [Role](policies.md#use-a-mirror). |
| `SecretInvalid` | A Secret exists but lacks a key, has an empty value, or holds a certificate or a key pair that the proxy cannot parse. |
| `HostConflict` | Another `UpstreamRegistry` claims the same host. |
| `CertificateMismatch` | The serving certificate of an `UpstreamRegistry` is not valid for every host. |
| `URLNotAllowed` | The URL of a `RegistryMirror` does not match an entry of `allowedMirrorURLs` of its upstream. |
| `RulesNotAccepted` | The policy has rules that are not accepted. The message lists their names. |

While the proxy starts, an object that uses a Secret that it did not read yet has no status change. It gets its status after the Secret is read.

## Status of a rule

A rule has the condition `Accepted`. It is `True`, or `False` with one of these reasons:

| Reason | Meaning |
|---|---|
| `OutsideCeiling` | A repository pattern or an action is outside the ceiling of the credential. Remove it, or ask the owner of the credential to widen the ceiling. A rule that asks for `push` or `delete` also needs `pull` in the ceiling. |
| `NotAllowed` | The credential, the mirror or the upstream does not admit the namespace of the policy. |
| `RefNotFound` | The credential or the mirror does not exist. |
| `RefNotReady` | The credential or the mirror exists but is not ready. |

The proxy ignores a rule that is not accepted. The other rules still apply. A mirror rule whose mirror is not ready is the exception: the proxy still uses the rule, and the pull follows the fallback rule of the mirror.

Example: the credential `ghcr-pat` allows only `soliddowant/foo/**`. A rule asks for `push` on `soliddowant/bar`. The policy status is:

```yaml
status:
  observedGeneration: 1
  conditions:
    - type: Ready
      status: "False"
      reason: RulesNotAccepted
      message: "rules not accepted: push-bar"
  rules:
    - name: push-bar
      conditions:
        - type: Accepted
          status: "False"
          reason: OutsideCeiling
```

The [field reference](crd-reference.md) lists every field.

## When the proxy refuses a request

If no rule allows a push or a delete, the proxy answers `403` with the code `DENIED`. Docker shows the message, for example:

```
ServiceAccount ci-foo/builder may not push repository soliddowant/bar
```

The message names your namespace, your ServiceAccount, the repository and the action. Read the status of your policies to find out why. See [why a request is denied](policies.md#why-a-request-is-denied).

Only policies in the namespace of the pod apply. A policy in another namespace never gives access, even to a ServiceAccount with the same name.
