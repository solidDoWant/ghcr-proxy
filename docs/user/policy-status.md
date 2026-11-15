# What the status of a policy means

For: people who write a `GHCRAccessPolicy`. You know Kubernetes YAML.

The proxy writes the status of each `GHCRAccessPolicy`. The status tells you which parts of the policy the proxy uses. The proxy also uses the same rules when it decides about a request. Thus the status and the decisions of the proxy agree.

Your admin gives each namespace a grant in the configuration of the proxy. A grant is a list of repository patterns and the actions that policies in the namespace can allow on them. A policy can never allow more than its grant. The proxy ignores the parts of a policy that are outside the grant. The other parts of the policy still apply.

## The Ready condition

The `Ready` condition has one of these reasons:

| Reason | `Ready` | Meaning |
|---|---|---|
| `NoGrant` | `False` | No grant applies to the namespace. The proxy ignores the whole policy. Its ServiceAccounts can only pull public images. |
| `InvalidPattern` | `False` | Some repository patterns are not valid patterns. The proxy ignores them. `status.rejected` lists them. |
| `OutsideGrant` | `False` | The grant does not cover some patterns or actions. The proxy ignores them. `status.rejected` lists them. |
| `Accepted` | `True` | The grant covers every repository pattern and action of the policy. |

If more than one reason applies, the condition shows the reason that is first in this table.

`status.observedGeneration` is the generation of the policy that the status describes. If it is lower than `metadata.generation`, the proxy did not process your last change yet.

## The rejected list

`status.rejected` has one entry for each repository pattern that lost one or more actions:

- `rule`: The index of the rule in `spec.rules`, starting at 0.
- `repository`: The pattern as you wrote it.
- `actions`: The actions that the proxy ignores for this pattern. `push` and `delete` include `pull`. Thus a lost `push` can also show a lost `pull`.
- `reason`: `OutsideGrant`, `InvalidPattern` or `NoGrant`, with the meanings in the table above.

Example: the grant of the namespace `ci-foo` covers `soliddowant/foo` and `soliddowant/foo/**`. A policy in `ci-foo` allows `push` on `soliddowant/bar`. The status is then:

```yaml
status:
  observedGeneration: 1
  conditions:
    - type: Ready
      status: "False"
      reason: OutsideGrant
      message: "1 of 1 repository patterns lose actions outside the namespace's grant; see status.rejected"
  rejected:
    - rule: 0
      repository: soliddowant/bar
      actions: [pull, push]
      reason: OutsideGrant
```

To fix the policy, remove the pattern or ask your admin to change the grant. The [field reference](crd-reference.md) lists every field.
