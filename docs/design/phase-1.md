# Phase 1 design: push and pull through the proxy

Decides the behaviour of phase 1. `SPEC.md` gives the model; this page fixes the details workers need. A case this page and `SPEC.md` don't decide is a design question: hand it back.

## 1. Packages

| Package | Job |
|---|---|
| `api/v1alpha1` | `GHCRAccessPolicy` types, CEL validation. |
| `internal/config` | Loads and validates the YAML config file (section 3). Compiles grant regexes once. Invalid config: the process exits non-zero at start with every error listed. |
| `internal/registry` | `Classify(*http.Request) (Request, error)`: the route table of section 4. `Forwarder`: the `httputil.ReverseProxy` with the upstream transports, `Location` rewrite and error mapping. Registry JSON errors (`{"errors":[{"code","message","detail"}]}`) built with `encoding/json`. |
| `internal/authn` | `Reviewer.Review(ctx, token) (Identity, error)` over `TokenReview`, with the 30-second cache (section 5). |
| `internal/authz` | `Glob` (parse, match, `Contains`), `Grants.For(namespace) []ExpandedGrant`, `Index` built from the informer (policies by namespace), `Authorizer.Decide(Identity, Request) Decision` (section 6). |
| `internal/controller` | `GHCRAccessPolicyReconciler`: computes and writes status. Leader-elected. |
| `internal/server` | TLS listener with certificate reload, the health/metrics listener, wiring. |
| `internal/audit` | `Logger.Log(Entry)`, one JSON line per request via `log/slog`'s JSON handler. |
| `internal/credhelper` | The helper's `get`/`store`/`erase`/`list`, protocol from `github.com/docker/docker-credential-helpers/credentials` (use its `Serve` and types; don't re-implement the protocol). |
| `test/fakeghcr` | The GHCR test double (section 9). `cmd/fakeghcr` wraps it as a binary for e2e only. |

## 2. Types

```go
// GHCRAccessPolicy (namespaced)
type GHCRAccessPolicySpec struct {
    // ServiceAccounts in this namespace this policy applies to. At least one.
    ServiceAccounts []string     `json:"serviceAccounts"`  // DNS-1123 subdomain names; CEL: 1..64 items, unique
    Rules           []AccessRule `json:"rules"`            // CEL: 1..32 items
}
type AccessRule struct {
    Repositories []string `json:"repositories"` // globs (section 6.1); CEL: 1..32 items, each 1..255 chars, lower-case
    Actions      []Action `json:"actions"`      // enum pull|push|delete; CEL: 1..3 unique items
}
type GHCRAccessPolicyStatus struct {
    ObservedGeneration int64              `json:"observedGeneration,omitempty"`
    Conditions         []metav1.Condition `json:"conditions,omitempty"`  // Ready
    Rejected           []RejectedRule     `json:"rejected,omitempty"`
}
type RejectedRule struct {
    Rule         int32    `json:"rule"`                   // index into spec.rules
    Repositories []string `json:"repositories,omitempty"` // patterns outside the grant
    Actions      []Action `json:"actions,omitempty"`      // actions the grant doesn't allow at all
}
```

Short name `gap`. Printer columns: Ready, Reason, Age. A pattern that fails glob parsing is rejected by CEL where possible (character set and `**` only as a whole segment) and otherwise by the controller as `Ready=False, reason=InvalidPattern`, ignored for authorization.

`Ready` reasons: `Accepted` (True), `OutsideGrant` (False, some patterns or actions dropped), `NoGrant` (False, no grant matches the namespace; nothing applies), `InvalidPattern` (False).

## 3. Config file

`--config` (default `/etc/ghcr-proxy/config.yaml`), rendered by the chart from values. Read at start only; the chart rolls the Deployment when it changes (checksum annotation).

```yaml
upstream:
  url: https://ghcr.io            # tests point this at the fake
  patFile: /var/run/ghcr-proxy/pat/token   # re-read when it changes (section 7)
  caFile: ""                      # extra CA for the upstream, tests only
  timeout: 30m                    # whole request; uploads of several GB must fit
authentication:
  audience: ghcr-proxy
  cacheTTL: 30s                   # max 5m
grants:
  - namespaces: "ci-(?P<repo>[a-z0-9-]+)"
    repositories:
      - soliddowant/${repo}
      - soliddowant/${repo}/**
    actions: [pull, push, delete]
tls:
  certFile: /var/run/ghcr-proxy/tls/tls.crt
  keyFile: /var/run/ghcr-proxy/tls/tls.key
listen:
  https: ":8443"
  health: ":8080"                 # /healthz, /readyz, /metrics
audit:
  enabled: true
```

Grant rules:
- `namespaces` is RE2, anchored (`^(?:…)$` added by the loader).
- `${name}` in a repository pattern must be a named capture of that grant's regex; the loader rejects unknown names. Expanded values are lower-cased. A capture that expands to a value containing `/` or glob characters makes that grant not apply to the namespace (never widens it).
- A namespace may match several grants; their expanded patterns and actions are combined per pattern.
- `push` and `delete` imply `pull`, in grants and in policies, because Docker checks blobs with `HEAD` before pushing.

## 4. Request classification

`Classify` matches only these routes (method, then path after `/v2/`). `<name>` is one or more `/`-separated components matching the distribution reference grammar's path-component regex; it is lower-cased. `<ref>` is a tag or digest, `<digest>` an OCI digest; both validated with `github.com/opencontainers/go-digest` and the reference grammar from `github.com/distribution/reference`.

| Method | Path | Action |
|---|---|---|
| GET, HEAD | `/v2/` | `none` |
| GET, HEAD | `/v2/<name>/manifests/<ref>` | pull |
| GET, HEAD | `/v2/<name>/blobs/<digest>` | pull |
| GET | `/v2/<name>/tags/list` | pull |
| GET | `/v2/<name>/referrers/<digest>` | pull |
| POST | `/v2/<name>/blobs/uploads/` | push; with `mount` and `from` query: push on `<name>` + pull on `from` |
| GET, PATCH, PUT, DELETE | `/v2/<name>/blobs/uploads/<session>` | push (`DELETE` here cancels an upload, still push) |
| PUT | `/v2/<name>/manifests/<ref>` | push |
| DELETE | `/v2/<name>/manifests/<ref>`, `/v2/<name>/blobs/<digest>` | delete |
| anything else | | `404 NAME_UNKNOWN`-style `NOT_FOUND`, nothing upstream |

Ambiguity: names may contain `manifests`, `blobs` or `tags` as components. Match the route suffix from the right (`/manifests/<ref>`, `/blobs/uploads/<session>`, `/blobs/uploads/`, `/blobs/<digest>`, `/tags/list`, `/referrers/<digest>`); the remainder is `<name>`. The route `.../blobs/uploads/<session>` wins over `.../blobs/<digest>` because `uploads` is not a valid digest.

## 5. Authentication

| `Authorization` header | Result |
|---|---|
| absent | anonymous identity |
| `Basic` with a password that passes `TokenReview` for the audience, user `system:serviceaccount:<ns>:<sa>` | that identity (pod from `authentication.kubernetes.io/pod-name` extra, if present) |
| `Basic` failing review, review not authenticated, audience missing from the result, user not a ServiceAccount | `401 UNAUTHORIZED` with the Basic challenge |
| `Basic` and the API server unreachable / review errors | `503` with `UNAVAILABLE`-style error, `Retry-After: 5`; never anonymous |
| `Bearer` or any other scheme | `401 UNAUTHORIZED` |

`GET /v2/` with no header returns `401` + challenge (Docker needs that to send credentials). `GET /v2/` with a valid credential returns `200 {}` without contacting the upstream. Every other anonymous request proceeds as anonymous (section 6).

Cache: key SHA-256 of the token; value identity or failure; TTL `cacheTTL`, but never beyond the token's own `exp` (parsed unverified from the JWT payload only to cap the TTL; if it can't be parsed, use `cacheTTL`). At most 10,000 entries, LRU. Concurrent reviews of the same token are collapsed (`golang.org/x/sync/singleflight`).

## 6. Authorization

### 6.1 Globs

A pattern is `/`-separated segments. A segment is either `**` (zero or more whole segments) or a run of literal characters `[a-z0-9._-]` and `*` (any characters within one segment). Use `github.com/bmatcuk/doublestar/v4`'s matching semantics for `Match`; containment is ours:

`Contains(outer, inner)` is true only if every name matching `inner` matches `outer`. Decide it conservatively on segments: literal ⊆ literal if equal; literal ⊆ `*`-segment if it matches; a `*`-segment ⊆ `*`-segment only if `outer` segment is exactly `*` or the two are equal; `inner` `**` ⊆ only an `outer` `**`; `outer` `**` absorbs any run of inner segments. Return false whenever unsure. Table-test both directions, including `**` at start, middle and end.

### 6.2 Decision table

`P(ns, sa, name, action)`: some policy in `ns` lists `sa` and has a rule whose pattern matches `name` with `action` (or an action that implies it), counting only the parts of the policy inside the grant (the controller's computation, recomputed locally by `Index`; authorization never reads status).

| Identity | Action | `P` | Outcome |
|---|---|---|---|
| any | none (`GET /v2/`) | | see section 5 |
| ServiceAccount | pull | true | forward with a PAT token scoped `repository:<name>:pull` |
| ServiceAccount | pull | false | forward anonymously |
| anonymous | pull | | forward anonymously |
| ServiceAccount | push | true | forward with a PAT token scoped `repository:<name>:pull,push` |
| ServiceAccount | push | false | `403 DENIED` |
| anonymous | push or delete | | `401 UNAUTHORIZED` + challenge (lets Docker retry with credentials) |
| ServiceAccount | mount, `P(push on name)` true, `P(pull on from)` true | | forward with token for both scopes |
| ServiceAccount | mount, push true, pull on `from` false | | strip `mount` and `from`, forward as a plain upload start |
| ServiceAccount | mount, push false | | `403 DENIED` |
| ServiceAccount | delete | true | forward with a PAT token scoped `repository:<name>:delete` (phase 2 settles GHCR's behaviour) |
| ServiceAccount | delete | false | `403 DENIED` |
| any | any | informer not synced | `503`, not ready |

`403` messages name the namespace, ServiceAccount, repository and action, and say whether no policy matched or the grant excluded it. They never list other policies.

Policy changes apply to the next request; in-flight requests finish with the decision they started with.

### 6.3 Status controller

For each policy: expand the namespace's grants (section 3). For each rule, keep the patterns contained in some grant pattern whose grant allows each of the rule's actions; record the rest in `status.rejected`. Set `Ready` as in section 2. Write status only when it changes. Re-reconcile all policies when the proxy starts (grants come from config, so a config change takes effect through the Deployment roll). The same function computes the effective policy for `Index`, so status and enforcement can't disagree.

## 7. Upstream credentials and forwarding

- The PAT file is read at start and whenever it changes (`fsnotify` on the directory, to catch Kubernetes' symlink swap, plus a 60-second re-stat as a backstop). A changed PAT drops all cached upstream transports. A missing or empty PAT file makes the replica not ready.
- Upstream auth uses `transport.NewWithContext(ctx, registry, authn.FromConfig(Basic user="ghcr-proxy", password=PAT), base, scopes)` from go-containerregistry. Transports are cached per sorted scope set (map with a 5-minute idle expiry); the library refreshes its token on `401`. Anonymous forwarding uses `authn.Anonymous` the same way.
- The proxy, not the client, follows nothing: it returns upstream redirects as they are (after the rewrite rule), and never follows a redirect itself.
- Hop-by-hop headers are removed as `httputil.ReverseProxy` does. The client's `Authorization` header is always removed before forwarding. `Host` is set to the upstream host.
- Responses: status, headers (minus `WWW-Authenticate`) and body as the upstream sent them. An upstream `401` on a PAT-backed request: drop that transport, retry once with a new one if the request body is replayable (no body, or `GetBody` available), else return `502`. Second `401`: `502` with message "upstream rejected the proxy's credential".
- Upstream connection errors and timeouts: `502` with a registry error body.

### 7.1 Location rewrite

If `Location` is absolute and its scheme+host equal the upstream's, replace scheme+host with `https://` + the `Host` the client sent. Relative `Location` values pass unchanged. Any other host passes unchanged.

## 8. TLS, health, readiness

- Certificate: loaded with `tls.LoadX509KeyPair`, served through `tls.Config.GetCertificate`, reloaded on file change (same watcher as the PAT). Minimum TLS 1.2. A failed reload keeps the old pair and logs an error.
- `/healthz`: process alive. `/readyz`: informer synced, PAT loaded, certificate loaded. `/metrics`: Go and process collectors only in phase 1.
- Leader election: `coordination.k8s.io` lease `ghcr-proxy-status` in the proxy's namespace; only the status controller is leader-gated.

## 9. Fake GHCR

`test/fakeghcr.New(t, opts) *Fake` starts two `httptest` TLS servers, **registry** and **storage**, sharing one CA the test also hands to clients:
- Registry: go-containerregistry's `registry.New()` handler wrapped by:
  - auth: no or wrong bearer token → `401` with `WWW-Authenticate: Bearer realm="<registry>/token",service="ghcr.io",scope="repository:<name>:<actions>"`;
  - `/token`: Basic with the configured PAT grants any scope; anonymous grants `pull` on repositories marked public; returns `{"token":…,"expires_in":300}`; tokens are opaque random strings mapped to granted scopes; requests outside a token's scopes get `401`/`403` like GHCR;
  - upload `Location` headers made absolute (`https://<registry host>/v2/...`);
  - `GET` blob → `307` to `https://<storage host>/<digest>?sig=<random>`, served by the storage server.
- `Fake` exposes the PAT, `MakePublic(name)`, `Push(name, image)` helpers, and a request log for assertions (method, path, which credential: PAT token, anonymous token or none).

The fake isn't a GHCR clone: don't add behaviour no scenario needs.

## 10. Chart

`deploy/charts/ghcr-proxy`: Deployment (default 2 replicas, `topologySpreadConstraints` not set: cluster defaults apply), Service (`https` 443 → 8443, `metrics` 8080), ServiceAccount, ClusterRole/Binding (section 5 of `SPEC.md`), Role/Binding for the lease, ConfigMap with the rendered config, CRDs from `config/crd/bases`. Values: `image`, `replicas`, `resources` (memory request = limit, no CPU limit), `pat.existingSecret` + `pat.key`, `tls.existingSecret` (type `kubernetes.io/tls`), `upstream.*`, `authentication.*`, `grants`, `audit.enabled`, `podSecurityContext`/`securityContext` (non-root, read-only root filesystem, no privilege escalation, all capabilities dropped), `serviceMonitor.enabled` (default false). The chart never creates the PAT or TLS Secrets.

## 11. CI pod setup (admin guide, and the e2e pod)

`docs/admin/ci-pods.md` shows, and the e2e test uses, exactly this:
- `hostAliases`: `ghcr.io` → the proxy Service's ClusterIP (the admin pins `spec.clusterIP` in the chart values, `service.clusterIP`, so it is stable).
- A projected volume `ghcr-proxy-token` with a `serviceAccountToken` source (`audience: ghcr-proxy`, `expirationSeconds: 3600`, `path: token`) mounted at `/var/run/secrets/ghcr-proxy` in the container that runs the Docker CLI.
- An image volume from `ghcr.io/soliddowant/ghcr-proxy-credential-helper` mounted at `/opt/ghcr-proxy/bin`, and `PATH` extended with it in that container.
- `~/.docker/config.json` with `{"credHelpers":{"ghcr.io":"ghcr-proxy"}}`, written by the step's setup.
- The CA at `/etc/docker/certs.d/ghcr.io/ca.crt` in the **dind** container (the daemon makes the registry connections).
- A `GHCRAccessPolicy` for the pod's ServiceAccount.

## 12. Errors

| Situation | Status | Registry error code |
|---|---|---|
| Unknown route, `_catalog` | 404 | `NOT_FOUND` (body only; nothing upstream) |
| Bad credential | 401 | `UNAUTHORIZED` |
| Denied by policy or grant | 403 | `DENIED` |
| TokenReview unavailable, informer not synced | 503 | `UNAVAILABLE` |
| Upstream rejects the PAT twice | 502 | `UNAVAILABLE` |
| Upstream unreachable or timed out | 502 | `UNAVAILABLE` |

`UNAVAILABLE` is not in the distribution spec's list; clients only show the message, which is enough.
