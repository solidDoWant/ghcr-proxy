# Metrics

registrygate serves Prometheus metrics at `/metrics` on the health port (`8080`). No metric has a label for a repository, namespace or ServiceAccount. Every label has a small fixed set of values, so the number of series stays small.

| Metric | Type | Labels | What it counts |
|---|---|---|---|
| `registrygate_requests_total` | counter | `action`, `decision`, `code` | Requests served. `action` is `none`, `pull`, `push`, `delete` or `unknown`. `decision` is `allowed`, `anonymous`, `denied`, `unauthenticated`, `unavailable`, `notfound` or `invalid`. `code` is the status class the client got (`2xx`, `4xx`, and so on). |
| `registrygate_upstream_request_duration_seconds` | histogram | `target` | Time until the response headers came back. `target` is `upstream`, `policy_mirror`, `anonymous_mirror` or `api`. |
| `registrygate_upstream_requests_total` | counter | `target`, `code` | Requests sent to a target. `code` is a status class, or `error` when there was no answer. |
| `registrygate_mirror_fallbacks_total` | counter | `mirror` | Pulls that went to the upstream because the `policy` or `anonymous` mirror failed. |
| `registrygate_token_reviews_total` | counter | `result`, `cache` | Token reviews. `result` is `authenticated`, `rejected` or `error`. `cache` is `hit` or `miss`. |
| `registrygate_upstream_token_fetches_total` | counter | `target`, `result` | Registry token fetches. `result` is `ok`, `rejected` or `error`. |
| `registrygate_policies` | gauge | `ready` | `GHCRAccessPolicy` objects the replica has, by whether they are inside the grants (`true` or `false`). The proxy works this out itself and does not read the status. |
| `registrygate_credential_reloads_total` | counter | `kind`, `result` | Reloads of the PAT, the TLS pair and the mirror files after start (`pat`, `tls`, `mirror`). `result` is `ok` or `error`. |

The proxy also serves a few Go runtime memory and garbage collection metrics and the process metrics.

The `api` target has no requests yet.

A rising `registrygate_credential_reloads_total{result="error"}` means a mounted file is missing or invalid. The proxy keeps the previous value and logs the error.
