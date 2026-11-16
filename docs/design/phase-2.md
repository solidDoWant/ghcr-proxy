# Phase 2 design: delete, mirrors and operations

Decides the behaviour of phase 2 on top of `docs/design/phase-1.md`. A case this page, phase 1's design and `SPEC.md` don't decide is a design question: hand it back.

The phase 2 design review (#63) is answered in this text.

## 1. Live tests and GHCR's behaviour

- `test/live` (`//go:build live`): runs client flows (push, pull, mount, anonymous public pull, using go-containerregistry as the client) and the delete flows below through a proxy started in-process with envtest, pointed at `https://ghcr.io` (and `https://api.github.com/` for REST), with the PAT read from the file named by `GHCR_LIVE_PAT_FILE` and the owner from `GHCR_LIVE_OWNER`. Every live test skips when either variable is unset **or** the envtest assets (`KUBEBUILDER_ASSETS`) are missing.
- **The PAT belongs to the owner's main GitHub account.** Live tests never run in CI and agents never run them: the worker builds them and checks they skip cleanly; the operator runs `make test-live` by hand and posts the results on the issue. No workflow, secret or variable for them is ever added to the repository.
- **Blast radius.**
  - The in-process proxy's **only** grant is `<owner>/ghcr-proxy-live-<run id>` and `<owner>/ghcr-proxy-live-<run id>/**`, with `<run id>` random per run. So even a bug in the REST fallback can't act outside the run's own packages.
  - Cleanup deletes only packages under that prefix that the run itself pushed, through the GitHub REST API.
  - The anonymous public pull uses one fixed, already-public image, read-only: `ghcr.io/soliddowant/ghcr-proxy:0.1.0`. No other package is listed, read, changed or deleted, and visibility is never changed.
  - Tests never print the PAT or tokens; failures show status codes and registry error codes only.
  - Packages left by a killed run are removed by hand by the operator, never by a later run.
- **First live issue:** record in `docs/dev/ghcr-behaviour.md` what GHCR answers for `DELETE /v2/<name>/manifests/<digest>`, `DELETE /v2/<name>/manifests/<tag>` and `DELETE /v2/<name>/blobs/<digest>`; which scopes its token endpoint accepts for delete (`delete`, `*`, `pull,push,delete`); and `expires_in`. The operator then amends section 2 with the result (a design PR). Done on 2026-11-16.

## 2. Delete

**The path was chosen once, from the operator's live run on 2026-11-16 (`docs/dev/ghcr-behaviour.md`), not at runtime.** GHCR answers `405 UNSUPPORTED` to every registry delete endpoint, whatever the token's scope:

| Route | Path | Upstream token scope |
|---|---|---|
| `DELETE /v2/<name>/manifests/<digest>` | `REST` (below). No registry `DELETE` is ever sent to GHCR for it. | none (the REST call uses the PAT directly) |
| `DELETE /v2/<name>/manifests/<tag>` | registry, GHCR's answer passed through (today `405 UNSUPPORTED`) | `repository:<name>:delete` |
| `DELETE /v2/<name>/blobs/<digest>` | registry, GHCR's answer passed through (today `405 UNSUPPORTED`) | `repository:<name>:delete` |

For the registry paths, phase 1's rules apply unchanged: an upstream `401` is retried once, then answered `502 "upstream rejected the proxy's credential"`. A refusal (`405`, or `UNSUPPORTED`) is passed through to the client. A `401` is never a reason to switch to REST. Deleting by tag stays unsupported, as `SPEC.md`'s "Won't do" list allows.

**REST path** for `DELETE /v2/<owner>/<package...>/manifests/<digest>`:

1. **Name check.** A `<name>` with fewer than two segments gets `404 NAME_UNKNOWN`, with no REST call.
2. **Owner type.** Try the `orgs` versions listing first. A `404` there means the owner is a user, so retry with `users`. The result is cached for an hour (successes only; errors are never cached). There is no separate `GET /users/<owner>` call.
3. **Package name.** Pass the **raw** package name (`a/b`) to go-github. go-github escapes it itself for the list calls and for the org delete. go-github's `UsersService.PackageDeleteVersion` does **not** escape it, so for user owners build the delete with `client.NewRequest("DELETE", "users/"+owner+"/packages/container/"+url.PathEscape(pkg)+"/versions/"+strconv.FormatInt(id, 10), nil)`. A fake-GHCR test asserts the exact escaped path for a two-segment package on all four calls (user and org, list and delete).
4. **Find the version.** List versions (`per_page=100`, paginating with go-github's `Response.NextPage`) and find the version whose `name` equals the digest exactly.
5. **Delete and map the answer:**

| GitHub answer | Client gets |
|---|---|
| `204` | `202 Accepted` |
| version not found, package `404` | `404 MANIFEST_UNKNOWN` |
| `400` or `422` (for example "cannot delete the last version") | `405 UNSUPPORTED` "GHCR refused to delete this version" |
| `403` or `429` with `X-RateLimit-Remaining: 0` or `Retry-After` | `503 UNAVAILABLE` with `Retry-After` |
| other `403` | `502 UNAVAILABLE` "the proxy's credential can't delete packages" |
| `401` | `502 UNAVAILABLE` "upstream rejected the proxy's credential" |
| `5xx`, timeout, connection error | `502 UNAVAILABLE` "upstream unavailable" |

**REST client setup:**
- Config `upstream.apiURL` (default `https://api.github.com/`; chart value `upstream.apiURL`), using `upstream.caFile` if set.
- REST calls use `upstream.apiTimeout` (default `30s`).
- go-github (`github.com/google/go-github`, latest major) gets an `http.Client` whose `CheckRedirect` returns `http.ErrUseLastResponse`. The PAT goes only to the `apiURL` host.
- The audit line's `upstreamStatus` is the DELETE call's status.
- The fake GHCR implements the `users` and `orgs` versions list and delete endpoints, with a PAT check, for these tests.

## 3. Mirrors

```yaml
mirrors:
  policy:                     # optional
    url: https://harbor.example.com
    pathPrefix: ghcr          # mirror name = <pathPrefix>/<name>
    credentialsFile: /var/run/ghcr-proxy/mirror-policy/credentials   # "user:password", optional
    caFile: ""
    fallbackToUpstream: true
    responseHeaderTimeout: 10s
  anonymous:                  # optional, same fields; must have no upstream credentials (docs)
    ...
```

**Routing:**

| Request | Identity / decision | Goes to |
|---|---|---|
| `GET` manifest or blob, `HEAD` manifest | allowed pull | policy mirror if set, else upstream with a PAT-backed token |
| `GET` manifest or blob, `HEAD` manifest | anonymous or not-allowed pull | anonymous mirror if set, else upstream anonymous |
| `HEAD` blob, tags list, referrers, push, mount, delete | any | upstream, as in phase 1 |

`HEAD` on a blob always goes upstream, because `docker push` uses it to decide whether to upload a layer. A mirror can still hold a blob after GHCR has dropped it.

**Mirror tokens and transport:**
- Each mirror uses the phase 1 token machinery (`transport.Ping` plus `transport.Exchange`) with its own token cache, keyed by mirror.
- The realm must have the mirror's own scheme and host.
- The scope is `repository:<pathPrefix>/<name>:pull`.
- Tokens are sent by the same "set the bearer token and do nothing else" round tripper as upstream tokens. `transport.NewWithContext` is never used.
- Mirror credentials come from that mirror's `credentialsFile` only. The config loader rejects a `credentialsFile` that resolves to the same file as `upstream.patFile`.

**Mirror files** (`credentialsFile`, `caFile`) follow the PAT rule:
- They are read at start and on change, with the same watcher.
- A missing or invalid file at start makes that mirror unavailable. Its requests then take the fallback rule; they never go to the other mirror.
- A failed reload keeps the previous value.

**Answers:**
- Mirror answers go through the same response handling as upstream answers: `modifyResponse`, the `1xx` filter, removing `WWW-Authenticate`, and the fixed error messages.
- **Fallback** (when `fallbackToUpstream` is true) happens on a connection error, on a `responseHeaderTimeout` timeout (time to response headers only), on `5xx`, and on `404` for manifests **and** blobs (blobs are content-addressed, so falling back is always safe). It doesn't happen on `401` or `403` from the **policy** mirror: that is a mirror credential problem and should be visible as `502 "mirror unavailable"`.
- A blob falls back only if the mirror fails before sending response headers. Once a body is streaming, a failure is returned to the client as is.
- **Anonymous and not-allowed pulls:** phase 1's section 7.3 mapping applies to the **final** answer, whichever target served it. A `401`, `403` or `404` from the anonymous mirror (after any fallback) becomes the proxy's Basic challenge, so BuildKit and private `FROM` keep working. Only the policy mirror's `401`/`403` gives `502`.
- **`Location` from a mirror's host:** strip `/v2/<pathPrefix>/` back to `/v2/` and rewrite scheme and host as phase 1 does. A mirror-host path that doesn't start with `/v2/<pathPrefix>/` gives `502 "mirror unavailable"` and the fallback rule. A mirror-host URL never reaches the client. Redirects to other hosts (storage) pass through.
- The whole request is still bounded by `upstream.timeout`.

**Harbor in e2e:** reuse the Harbor-on-kind approach from `solidDoWant/harbor-operator`'s `test/` (Harbor images from `ghcr.io/goharbor`), with a proxy-cache project whose endpoint is the in-cluster fake GHCR.

## 4. Metrics

Prometheus client (`github.com/prometheus/client_golang`). Every label has a fixed set of values:

| Metric | Type | Labels |
|---|---|---|
| `ghcr_proxy_requests_total` | counter | `action` (none, pull, push, delete, unknown), `decision` (allowed, anonymous, denied, unauthenticated, unavailable, notfound, invalid), `code` (1xx..5xx) |
| `ghcr_proxy_upstream_request_duration_seconds` | histogram | `target` (upstream, policy_mirror, anonymous_mirror, api); buckets 0.05, 0.1, 0.25, 0.5, 1, 2.5, 10, 60 |
| `ghcr_proxy_upstream_requests_total` | counter | `target`, `code` (1xx..5xx, or `error` when there was no answer) |
| `ghcr_proxy_mirror_fallbacks_total` | counter | `mirror` (policy, anonymous) |
| `ghcr_proxy_token_reviews_total` | counter | `result` (authenticated, rejected, error), `cache` (hit, miss) |
| `ghcr_proxy_upstream_token_fetches_total` | counter | `target`, `result` (ok, rejected, error) |
| `ghcr_proxy_policies` | gauge | `ready` (true, false), from the local `Effective` result, not from status |
| `ghcr_proxy_credential_reloads_total` | counter | `kind` (pat, tls, mirror), `result` (ok, error) |

- The Go collector is trimmed to the runtime memory and GC metrics.
- A test runs the full integration flow set with both mirrors configured, then asserts that the series count is at most 250.

## 5. Admission webhook

- **Server:** controller-runtime's `webhook.NewServer` with an `admission.CustomValidator` (registered with `WithValidator`), on its own listener (`:9443`). Its `CertWatcher` reloads the certificate when the files change. Optional: `webhook.enabled` in the chart (default false).
- **Own Service:** the webhook has its own Service with `publishNotReadyAddresses: true`, and its handler needs only the config and the webhook certificate, not the PAT and not the informer. So a missing PAT or an unsynced cache never blocks policy writes cluster-wide.
- **Certificate:**
  - When `webhook.certManager.issuerRef` is set, the chart creates a cert-manager `Certificate` and annotates the `ValidatingWebhookConfiguration` with `cert-manager.io/inject-ca-from: <ns>/<certificate>`, which needs cainjector (documented).
  - Otherwise the admin provides `webhook.existingSecret` and `webhook.caBundle`.
- **Logic:**
  - The phase 1 status computation (`Effective`, the same function) decides.
  - Reject `CREATE`, and `UPDATE` that changes `spec`, when any pattern or action would be rejected or no grant matches, with a message listing them.
  - Admit an `UPDATE` whose `oldObject.spec` equals `object.spec` (`equality.Semantic.DeepEqual`), or whose object has a `deletionTimestamp`. Label, annotation and finalizer changes then still work after a grant is narrowed.
- **Registration:** `failurePolicy: Fail`, `sideEffects: None`, `timeoutSeconds: 5`, `matchPolicy: Equivalent`, `admissionReviewVersions: ["v1"]`. The rule covers resource `ghcraccesspolicies` only (not `/status`), operations `CREATE` and `UPDATE`.
- Every replica serves it. The proxy is still the enforcement point; the webhook is for fast feedback.

## 6. Chart options

- `podDisruptionBudget.enabled` (default false, `maxUnavailable: 1`). The value docs note that with `replicas: 1` this allows full disruption.
- `networkPolicy.enabled` (default false). NetworkPolicy ports match the destination **endpoint** port after Service translation (documented).
  - **Ingress:**
    - `8443` from namespaces matching `networkPolicy.clientNamespaceSelector`;
    - `8080` from `networkPolicy.metricsNamespaceSelector`;
    - `9443` from `networkPolicy.apiServerCIDRs`.
  - **Egress:**
    - DNS;
    - `networkPolicy.apiServerCIDRs` on `networkPolicy.apiServerPorts` (default `[443, 6443]`);
    - `443` anywhere (GHCR's addresses aren't fixed; admins who use Cilium can replace it with an FQDN policy, documented);
    - `networkPolicy.extraEgress`: raw rules for in-cluster upstreams and mirrors. e2e uses it for the fake GHCR and Harbor.
  - **e2e:** kind's default CNI (kindnet) enforces NetworkPolicy on the kind version the harness pins. The e2e test for AC-2-12 also checks that a pod in a namespace outside `clientNamespaceSelector` can't connect to `8443`.
- `serviceMonitor.enabled` (default false).

## 7. Docs walk-through test

- `test/e2e/docs_test.go` extracts the fenced `sh` blocks marked `<!-- e2e -->` from `docs/admin/*.md`, in order, and runs them against a fresh kind cluster. cert-manager comes from its OCI chart on `quay.io/jetstack`.
- **The harness adds only these things outside the docs:**
  - the fake GHCR installed in the cluster;
  - a values overlay with `upstream.url` and `upstream.ca.existingSecret` pointing at the fake;
  - the fake's PAT value as the PAT Secret's content.
- The walk-through ends with a `docker push` only, so it needs no storage host alias.
- This e2e test is part of the single e2e CI job (the owner chose not to split e2e).
