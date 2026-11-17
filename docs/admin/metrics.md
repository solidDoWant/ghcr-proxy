# Metrics

registrygate serves Prometheus metrics at `/metrics` on the health port (`8080`). The only label that names an object is `upstream`. No metric has a label for a repository, namespace or ServiceAccount. Every label has a small fixed set of values, so the number of series stays small.

| Metric | Type | Labels | What it counts |
|---|---|---|---|
| `registrygate_requests_total` | counter | `upstream`, `action`, `decision`, `code` | Requests served. `upstream` is `namespace/name` of the `UpstreamRegistry`, or `none` before a host resolves. `action` is `none`, `pull`, `push`, `delete` or `unknown`. `decision` is `allowed`, `mirror`, `anonymous`, `denied`, `ambiguous`, `unauthenticated`, `unavailable`, `notfound` or `invalid`. `code` is the status class the client got (`2xx`, `4xx`, and so on). |
| `registrygate_upstream_request_duration_seconds` | histogram | `upstream`, `target` | Time until the response headers came back. `target` is `upstream` or `mirror`. |
| `registrygate_upstream_requests_total` | counter | `upstream`, `target`, `code` | Requests sent to a target. `code` is a status class, or `error` when there was no answer. |
| `registrygate_mirror_fallbacks_total` | counter | `upstream` | Pulls that went to the upstream because a mirror failed. |
| `registrygate_token_reviews_total` | counter | `result`, `cache` | Token reviews. `result` is `authenticated`, `rejected` or `error`. `cache` is `hit` or `miss`. |
| `registrygate_upstream_token_fetches_total` | counter | `upstream`, `target`, `result` | Registry token fetches. `result` is `ok`, `rejected` or `error`. |
| `registrygate_objects` | gauge | `kind`, `ready` | Custom resources the replica has, by kind and by `ready` (`true`, `false` or `pending`). |
| `registrygate_secret_updates_total` | counter | `result` | Secret updates the proxy saw. `result` is `ok` or `unavailable` (the Secret is gone or not readable). |

The proxy also serves a few Go runtime memory and garbage collection metrics and the process metrics.
