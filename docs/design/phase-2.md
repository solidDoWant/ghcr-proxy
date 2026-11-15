# Phase 2 design: delete, mirrors and operations

Decides the behaviour of phase 2 on top of `docs/design/phase-1.md`. A case this page, phase 1's design and `SPEC.md` don't decide is a design question: hand it back.

## 1. Live tests and GHCR's behaviour

- `test/live` (`//go:build live`): runs the phase 1 client flows (push, pull, mount, anonymous public pull, using go-containerregistry as the client) and the delete flows below through a proxy started in-process, pointed at `https://ghcr.io` with the PAT read from the file named by `GHCR_LIVE_PAT_FILE`, owner from `GHCR_LIVE_OWNER`. Without both variables every live test skips.
- **The PAT belongs to the owner's main GitHub account.** Live tests never run in CI and agents never run them: the worker builds them and checks they skip cleanly; the operator runs `make test-live` by hand and posts the results on the issue. No workflow, secret or variable for them is ever added to the repository.
- **Blast radius.** Every repository a live run touches is `<owner>/ghcr-proxy-live-<run id>` or below it, where `<run id>` is random per run. Cleanup deletes only packages whose name starts with that run's prefix and that the run itself pushed, through the GitHub REST API. The tests never list, read, change or delete any other package, never change package visibility, and never print the PAT or tokens (failures show status codes and registry error codes only).
- First live issue: record in `docs/dev/ghcr-behaviour.md` what GHCR answers for `DELETE /v2/<name>/manifests/<digest>`, `DELETE /v2/<name>/manifests/<tag>`, `DELETE /v2/<name>/blobs/<digest>`, the scopes its token endpoint accepts for delete (`delete`, `*`, `pull,push,delete`), and `expires_in`. The rest of section 2 depends on it.

## 2. Delete

Decision table, settled by the live results:

| GHCR answer to OCI delete by digest | Proxy behaviour |
|---|---|
| `202` with a PAT token of some scope | forward with that scope; done |
| `405`/`UNSUPPORTED`/`401` with every scope | REST fallback below |

REST fallback for `DELETE /v2/<owner>/<package...>/manifests/<digest>`:
1. Owner type from `GET /users/<owner>` (`type` `User` or `Organization`), cached for an hour.
2. Package name is the rest of the path, URL-escaped (`a/b` → `a%2Fb`).
3. List versions (`GET /{users|orgs}/<owner>/packages/container/<package>/versions?per_page=100`, following pagination) and find the version whose `name` is the digest.
4. `DELETE` that version. Map: `204` → `202 Accepted` to the client; not found → `404 MANIFEST_UNKNOWN`; `403` → `502` "the proxy's credential can't delete packages".

HTTP calls to the GitHub REST API use `github.com/google/go-github` (latest major), not hand-written requests. Delete by tag and blob delete are forwarded as in phase 1 and GHCR's answer is returned.

## 3. Mirrors

```yaml
mirrors:
  policy:                     # optional
    url: https://harbor.example.com
    pathPrefix: ghcr          # mirror name = <pathPrefix>/<name>
    credentialsFile: /var/run/ghcr-proxy/mirror-policy/credentials   # "user:password", optional
    caFile: ""
    fallbackToUpstream: true
  anonymous:                  # optional, same fields; must have no upstream credentials (docs)
    ...
```

| Request | Identity / decision | Goes to |
|---|---|---|
| GET/HEAD manifest or blob | allowed pull | policy mirror if set, else upstream with PAT token |
| GET/HEAD manifest or blob | anonymous or not-allowed pull | anonymous mirror if set, else upstream anonymous |
| tags list, referrers, push, mount, delete | any | upstream as in phase 1 |

- Mirror transports use go-containerregistry's transport with the mirror's own Basic credentials, or anonymous. The PAT file is never opened by mirror code (separate type; a test asserts mirror requests never carry it).
- Fallback (when enabled) on: connection error, timeout, `5xx`, `404` for a manifest. Not on `401`/`403` (a mirror credential problem should be visible: `502`).
- Fallback for a blob only if the mirror fails before sending response headers; once a body is streaming, a failure is returned to the client as is.
- `Location` rewrite also covers the mirror's host. Blob redirects from the mirror pass through like GHCR's.
- Harbor in e2e: reuse the Harbor-on-kind setup approach from `solidDoWant/harbor-operator`'s `test/` (Harbor images from `ghcr.io/goharbor`), with a proxy-cache project whose endpoint is the in-cluster fake GHCR.

## 4. Metrics

Prometheus client (`github.com/prometheus/client_golang`). Every label has a fixed set of values. Names:

| Metric | Type | Labels |
|---|---|---|
| `ghcr_proxy_requests_total` | counter | `action` (none, pull, push, delete), `decision` (allowed, anonymous, denied, unauthenticated, unavailable), `code` (1xx..5xx) |
| `ghcr_proxy_upstream_request_duration_seconds` | histogram | `target` (upstream, policy_mirror, anonymous_mirror), `code` class |
| `ghcr_proxy_mirror_fallbacks_total` | counter | `mirror` (policy, anonymous) |
| `ghcr_proxy_token_reviews_total` | counter | `result` (authenticated, rejected, error), `cache` (hit, miss) |
| `ghcr_proxy_upstream_token_fetches_total` | counter | `result` |
| `ghcr_proxy_policies` | gauge | `ready` (true, false) |
| `ghcr_proxy_credential_reloads_total` | counter | `kind` (pat, tls, mirror), `result` |

Go collector trimmed to the runtime memory and GC metrics. A test caps the total series count at 150 with the default config.

## 5. Admission webhook

- Path `/validate-ghcraccesspolicy` on a third listener (`:9443`), TLS from a mounted Secret. Optional: `webhook.enabled` in the chart (default false).
- Certificate: the chart creates a cert-manager `Certificate` when `webhook.certManager.issuerRef` is set; otherwise the admin provides `webhook.existingSecret` and `webhook.caBundle`.
- Logic: the status computation from phase 1 (same function). Reject `CREATE`/`UPDATE` when any pattern or action would be rejected or no grant matches, with a message listing them. `failurePolicy: Fail`, `sideEffects: None`, `timeoutSeconds: 5`, `matchPolicy: Equivalent`. Status writes are not validated.
- Served by every replica; the proxy is still the enforcement point (the webhook is for fast feedback).

## 6. Chart options

- `podDisruptionBudget.enabled` (default false, `maxUnavailable: 1`).
- `networkPolicy.enabled` (default false): ingress on 8443 from namespaces matching `networkPolicy.clientNamespaceSelector`, 8080 from `networkPolicy.metricsNamespaceSelector`, 9443 from the API server (`networkPolicy.apiServerCIDRs`); egress to DNS, the API server, and 443 anywhere (GHCR's addresses aren't fixed; admins who use Cilium can replace it with an FQDN policy, documented).
- `serviceMonitor.enabled` (default false).

## 7. Docs walk-through test

`test/e2e/docs_test.go` extracts the fenced `sh` blocks marked `<!-- e2e -->` from `docs/admin/*.md` in order and runs them against a fresh kind cluster, with the fake GHCR in place of `ghcr.io` (`upstream.url` override set through one documented value). cert-manager from its OCI chart on `quay.io/jetstack`.
