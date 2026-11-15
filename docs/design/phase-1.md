# Phase 1 design: push and pull through the proxy

Decides the behaviour of phase 1. `SPEC.md` gives the model; this page fixes the details workers need. A case this page and `SPEC.md` don't decide is a design question: hand it back.

The phase 1 design review (#3) is answered in this text. Where a section says "must", a test covers it.

## 1. Packages

| Package | Job |
|---|---|
| `api/v1alpha1` | `GHCRAccessPolicy` types, CEL validation. |
| `internal/config` | Loads and validates the YAML config file (section 3). Compiles grant regexes once. Invalid config: the process exits non-zero at start with every error listed. |
| `internal/glob` | The repository glob grammar, `Match` and `Contains` (section 6.1). Used by config validation, the CRD controller and the authorizer. |
| `internal/registry` | `Classify(*http.Request) (Request, error)`: the route table of section 4. `Request.UpstreamURL()` rebuilds the upstream path and query from the classified fields. `Forwarder`: `httputil.ReverseProxy` (with `Rewrite`) plus the token-setting round tripper and error mapping (section 7). Registry JSON errors (`{"errors":[{"code","message","detail"}]}`) built with `encoding/json`. |
| `internal/upstream` | Upstream token handling (section 7.1): ping, token exchange, the token cache, the PAT holder. The only package that ever reads the PAT. |
| `internal/authn` | `Reviewer.Review(ctx, token) (Identity, error)` over `TokenReview`, with the cache (section 5). |
| `internal/authz` | `Grants.For(namespace) []ExpandedGrant`, `Effective(policy, grants) EffectivePolicy` (section 6.3), the informer-backed `Index`, `Authorizer.Decide(Identity, Request) Decision` (section 6.2). |
| `internal/controller` | `GHCRAccessPolicyReconciler`: computes and writes status. Leader-elected. |
| `internal/server` | TLS listener with certificate reload, the health and metrics listener, request pipeline (section 6.2, "Order"), graceful shutdown. |
| `internal/audit` | `Entry` and `Logger.Log(Entry)`, one JSON line per request via `log/slog`'s JSON handler (section 10). |
| `internal/credhelper` | The helper's `get`, `store`, `erase` and `list`, protocol from `github.com/docker/docker-credential-helpers/credentials` (use its `Serve` and types; don't re-implement the protocol). |
| `test/fakeghcr` | The GHCR test double (section 9). `cmd/fakeghcr` wraps it as a binary and image for e2e only. |

## 2. Types

```go
// GHCRAccessPolicy (namespaced)
type GHCRAccessPolicySpec struct {
    // ServiceAccounts in this namespace this policy applies to. At least one.
    ServiceAccounts []string     `json:"serviceAccounts"`  // DNS-1123 subdomain names; CEL: 1..64 items, unique
    Rules           []AccessRule `json:"rules"`            // CEL: 1..32 items
}
type AccessRule struct {
    Repositories []string `json:"repositories"` // globs (section 6.1); CEL: 1..32 items, each 1..255 chars, the glob grammar
    Actions      []Action `json:"actions"`      // enum pull|push|delete; CEL: 1..3 unique items
}
type GHCRAccessPolicyStatus struct {
    ObservedGeneration int64              `json:"observedGeneration,omitempty"`
    Conditions         []metav1.Condition `json:"conditions,omitempty"`  // Ready
    Rejected           []RejectedRule     `json:"rejected,omitempty"`
}
// One entry per (rule, pattern) that lost at least one action.
type RejectedRule struct {
    Rule       int32    `json:"rule"`       // index into spec.rules
    Repository string   `json:"repository"` // the pattern as written
    Actions    []Action `json:"actions"`    // the actions dropped for it (after implication)
    Reason     string   `json:"reason"`     // OutsideGrant | InvalidPattern | NoGrant
}
```

Short name `gap`. Printer columns: Ready, Reason, Age. CEL rejects patterns outside the glob grammar (section 6.1) where it can; the controller rejects the rest as `InvalidPattern`.

`Ready` reasons, highest precedence first: `NoGrant` (False: no grant applies to the namespace; every (rule, pattern) is listed with all its actions), `InvalidPattern` (False), `OutsideGrant` (False: some actions dropped), `Accepted` (True). Messages are fixed text plus counts, rule indexes and patterns, for example `2 of 5 repository patterns lose actions outside the namespace's grant; see status.rejected`. They never contain anything else.

Status is written with `Status().Update` (the RBAC grants only `update` on the status subresource). Conditions are set with `meta.SetStatusCondition`, and status is written only when it differs from the current value, so reconciles converge.

## 3. Config file

`--config` (default `/etc/ghcr-proxy/config.yaml`), rendered by the chart from values. Read at start only; the chart rolls the Deployment when it changes (checksum annotation).

```yaml
upstream:
  url: https://ghcr.io            # tests point this at the fake
  tokenHosts: []                  # extra hosts allowed as token realms; default: only the upstream host
  patFile: /var/run/ghcr-proxy/pat/token   # the chart mounts the Secret as a directory (never subPath)
  caFile: ""                      # extra CA for the upstream, tests only
  timeout: 30m                    # whole upstream request; uploads of several GB must fit
  tokenTimeout: 10s               # token exchange
authentication:
  audience: ghcr-proxy
  cacheTTL: 30s                   # 1s..30s; the loader rejects larger values (SPEC.md)
  reviewTimeout: 5s
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
server:
  readHeaderTimeout: 10s
  idleTimeout: 120s
  maxHeaderBytes: 65536
  shutdownGracePeriod: 10m        # in-flight requests may finish this long after SIGTERM
audit:
  enabled: true
```

Grant rules:
- `namespaces` is RE2, anchored (`^(?:…)$` added by the loader).
- `${name}` in a repository pattern must be a named capture of that grant's regex; the loader rejects unknown names.
- Each grant pattern must satisfy the glob grammar (section 6.1) with `${…}` treated as one literal run; the loader rejects anything else at start. After expansion for a namespace, the pattern is checked again. The grant **does not apply** to that namespace if a capture is empty, did not participate in the match, or expands to a value outside `[a-z0-9._-]+`, or if the expanded pattern fails the grammar. A grant that doesn't apply never widens anything.
- Expanded values are lower-cased.
- A namespace may match several grants; each applying grant contributes its expanded patterns with its actions.
- `push` and `delete` imply `pull`, in grants and in policies, because Docker checks blobs with `HEAD` before pushing.

## 4. Request classification

`Classify` works only on `r.URL.EscapedPath()` and the raw query. It never uses `http.ServeMux` for `/v2/` (ServeMux answers `301` to unclean paths).

**Rejected outright with `404 NOT_FOUND`, nothing upstream:** any `%` in the path; an empty, `.` or `..` segment (including `//` and a trailing `/`, except the route `/v2/` and `/v2/<name>/blobs/uploads/`); a path longer than 2048 bytes; upper-case letters in `<name>` (the reference grammar forbids them, and GHCR names are lower-case; returning `404` is what GHCR returns for a name it doesn't have).

`<name>` is one or more `/`-separated components, each matching the path-component regex of `github.com/distribution/reference`, whole name at most 255 characters. `<ref>` is a tag (reference tag grammar) or a digest; `<digest>` is validated with `github.com/opencontainers/go-digest`. `<session>` is exactly one path segment matching `[A-Za-z0-9._=~+-]{1,256}`.

| Method | Path | Action |
|---|---|---|
| GET, HEAD | `/v2/` | `none` |
| GET, HEAD | `/v2/<name>/manifests/<ref>` | pull |
| GET, HEAD | `/v2/<name>/blobs/<digest>` | pull |
| GET, HEAD | `/v2/<name>/tags/list` | pull |
| GET, HEAD | `/v2/<name>/referrers/<digest>` | pull |
| POST | `/v2/<name>/blobs/uploads/` | push; with a valid mount (below): push on `<name>` + pull on `from` |
| GET, PATCH, PUT, DELETE | `/v2/<name>/blobs/uploads/<session>` | push (`DELETE` here cancels an upload, still push) |
| PUT | `/v2/<name>/manifests/<ref>` | push |
| DELETE | `/v2/<name>/manifests/<ref>`, `/v2/<name>/blobs/<digest>` | delete |
| anything else, including `/v2/_catalog` | | `404 NOT_FOUND`, nothing upstream |

Route matching runs from the right: the last segments must be one of `manifests/<ref>`, `blobs/uploads/<session>`, `blobs/uploads/` (trailing slash), `blobs/<digest>`, `tags/list`, `referrers/<digest>`; the rest is `<name>`. Because `<session>` and `<ref>` are single segments and `uploads` is not a digest, the split is unique.

**Query parameters.** The forwarded query is rebuilt from the classified request, never copied:
- `POST .../blobs/uploads/`: only `digest`, `mount` and `from` are kept. A **valid mount** is exactly one `mount` (a valid digest) and exactly one `from` (a valid `<name>`, lower-case). Any other combination (repeated keys, `mount` without `from`, `from` without `mount`, an invalid value, `from` equal to `<name>`) drops both `mount` and `from` and the request is a plain upload start. `digest` (monolithic upload) is kept only if it appears once and is valid, else the request is `400 DIGEST_INVALID` with nothing upstream.
- `PUT .../blobs/uploads/<session>`: `digest` required once and valid; all other parameters, including the upstream's upload state parameters (`_state` and similar), are kept as sent, because the upload session URL is the upstream's own. The path segments are still rebuilt.
- `GET .../tags/list`: only `n` and `last`. `GET .../referrers/<digest>`: only `artifactType`.
- Every other route: no query.

`Request.UpstreamURL()` builds the path from the classified fields: `/v2/` + `<name>` (lower-case) + the route + `<ref>`/`<digest>`/`<session>`. The decision is made on exactly the name and query the upstream receives.

## 5. Authentication

| `Authorization` header | Result |
|---|---|
| absent | anonymous identity |
| one `Basic` header, non-empty password, password passes `TokenReview` for the audience, user parses with `k8s.io/apiserver/pkg/authentication/serviceaccount.SplitUsername` | that identity (pod from the `authentication.kubernetes.io/pod-name` extra, if present) |
| `Basic` with bad base64, empty password, more than one `Authorization` header, review not authenticated, audience missing from the result, user not a ServiceAccount | `401 UNAUTHORIZED` with the Basic challenge, no TokenReview for the malformed cases |
| `Basic` and the review errors or times out | `503 UNAVAILABLE`, `Retry-After: 5`; never anonymous |
| `Bearer` or any other scheme | `401 UNAUTHORIZED` |

The challenge is always `WWW-Authenticate: Basic realm="ghcr-proxy"`.

Cache:
- Key: SHA-256 of the token. Value: the identity, or "rejected". **Only definite answers are cached** (authenticated, or not authenticated). Errors and timeouts are never cached.
- TTL `cacheTTL` (max 30s), but never beyond the token's own `exp`, read with `github.com/golang-jwt/jwt/v5`'s `Parser.ParseUnverified` only to cap the TTL; if it can't be parsed, use `cacheTTL`.
- At most 10,000 entries, LRU (`github.com/hashicorp/golang-lru/v2`). Accepted risk: unique garbage tokens each cost one review and can evict entries; the Kubernetes API server's own rate limits and the review timeout bound the damage.
- Concurrent reviews of the same token are collapsed with `golang.org/x/sync/singleflight`. The shared call runs on a context detached from any client request, bounded by `reviewTimeout`.
- The proxy's rest config sets QPS 50 and burst 100.

## 6. Authorization

### 6.1 Globs

Grammar: a pattern is one or more segments separated by `/`, no empty segments. A segment is either exactly `**`, or a non-empty run of `[a-z0-9._-]` and `*`, where no two `*` are adjacent. Nothing else (`?`, `[`, `{`, `\`, upper case) is allowed. The same grammar applies to policy patterns, grant patterns and the CEL rule.

`Match(pattern, name)`: `*` matches any run (possibly empty) of `[a-z0-9._-]` within one segment; `**` matches zero or more whole segments. Use `github.com/bmatcuk/doublestar/v4`'s `Match`; the grammar keeps us inside the subset where its semantics are the ones stated here, and a test checks agreement on the grammar.

`Contains(outer, inner)` is true only if every name matching `inner` matches `outer`. Recursive over segment lists `o` and `i`:

```
C(o, i):
  if o empty: return i empty
  if o[0] == "**":
      # ** absorbs zero or more inner segments, then the rest of outer must contain the rest of inner
      for k in 0..len(i): if C(o[1:], i[k:]) return true
      return false
  if i empty: return false
  if i[0] == "**": return false            # only an outer ** can absorb an inner **
  if segContains(o[0], i[0]): return C(o[1:], i[1:])
  return false

segContains(os, is):                       # every segment matching `is` matches `os`
  if is has no '*': return segMatch(os, is)
  if os == "*": return true
  if os has no '*': return false
  return os == is                           # conservative
```

When the algorithm can't prove containment it returns false. Required tests: the examples in AC-1-17, `Contains("a/**/b", "a/**") == false`, `Contains("**/b", "**") == false`, `Contains("**", "a/**/b") == true`, and a generated check: for random pattern pairs over a small alphabet with `Contains` true, every name in an enumerated name set that matches inner also matches outer.

### 6.2 Decision table

`P(ns, sa, name, action)`: the effective policy (section 6.3) of some policy in `ns` that lists `sa` allows `action` (after implication) on `name`. Authorization never reads status.

**Order.** For each request: (1) classify (unknown route: `404`); (2) authenticate (section 5); (3) readiness: for an authenticated request, or a request that would need the PAT, the replica must be ready (informer synced, PAT loaded) or the answer is `503 UNAVAILABLE` with `Retry-After: 5` (this includes authenticated `GET /v2/`); (4) decide with the table below. Anonymous `GET /v2/`, anonymous pulls and unknown routes don't need readiness.

| Identity | Action | `P` | Outcome |
|---|---|---|---|
| anonymous | none (`GET /v2/`) | | `401` + Basic challenge |
| ServiceAccount | none | | `200 {}`, nothing upstream |
| ServiceAccount | pull | true | forward with a PAT-backed token for `repository:<name>:pull` |
| ServiceAccount | pull | false | forward with an anonymous token (section 7.3 for the answer) |
| anonymous | pull | | forward with an anonymous token (section 7.3 for the answer) |
| ServiceAccount | push | true | forward with a PAT-backed token for `repository:<name>:pull,push` |
| ServiceAccount | push | false | `403 DENIED`, nothing upstream |
| anonymous | push or delete | | `401` + Basic challenge, nothing upstream (Docker retries with credentials) |
| ServiceAccount | valid mount, push on `name` true, pull on `from` true | | forward with a PAT-backed token for `repository:<name>:pull,push` and `repository:<from>:pull` |
| ServiceAccount | valid mount, push true, pull on `from` false | | drop `mount` and `from`, forward as a plain upload start |
| ServiceAccount | mount, push false | | `403 DENIED`, nothing upstream |
| ServiceAccount | delete | true | forward with a PAT-backed token for `repository:<name>:delete` (phase 2 settles GHCR's behaviour) |
| ServiceAccount | delete | false | `403 DENIED`, nothing upstream |

`403` messages name the namespace, ServiceAccount, repository and action, and say whether no policy matched or the grant excluded it. They never list other policies.

Policy changes apply to the next request; in-flight requests finish with the decision they started with. **Staleness:** once synced, a replica enforces the last policies its informer has seen. During an API server outage, revoked policies keep working until the watch recovers. This is accepted (principle 3 is about not deciding; a synced cache has decided), and the admin docs say so.

**How AC-1-04 and AC-1-05 are read** (operator for the owner): "nothing reached the upstream" means no push-class request (`POST`, `PATCH`, `PUT`, `DELETE`) and no PAT-backed request for that repository reached the upstream. Anonymous pull-class checks (Docker's `HEAD` on blobs and manifests before a push) may reach it.

### 6.3 Effective policy and status

One function, `Effective(policy, grants)`, used by both the status controller and `Index`:

1. Expand the grants that apply to the policy's namespace (section 3). If none apply: `NoGrant`; the effective policy is empty.
2. For each rule, each pattern, each action (after implication): keep the (pattern, action) pair if some applying grant has a pattern that `Contains` it and that grant allows the action. A pattern that fails the grammar keeps no actions (`InvalidPattern`).
3. Every (rule, pattern) that lost actions becomes one `RejectedRule` with those actions and its reason.

The status controller writes `Ready` and `rejected` from this. It re-reconciles all policies when the proxy starts. Status and enforcement agree on each replica. While a config change rolls out, replicas may run different grants, and status reflects the leader's config; it settles when the roll finishes. This is accepted and the admin docs say so.

Leader election: lease `ghcr-proxy-status` in the namespace from the `POD_NAMESPACE` environment variable (downward API), controller-runtime defaults for timings, `LeaderElectionReleaseOnCancel: true`. The event recorder is not used (no `events` permission).

## 7. Upstream credentials and forwarding

### 7.1 Tokens

- The PAT file is read at start and whenever it changes (`fsnotify` on the directory, to catch Kubernetes' symlink swap, plus a 60-second re-stat as a backstop). Surrounding whitespace is trimmed. A missing, unreadable or empty file at start means "PAT not loaded" (replica not ready). A failed, unreadable or empty **reload** keeps the previous PAT and logs an error (same rule as TLS). Each successful change increments a PAT generation.
- go-containerregistry is used only for the token protocol, never as the forwarding transport: `transport.Ping` against the upstream at start and after each PAT change (cached challenge), then `transport.Exchange` (or the library's equivalent exported token-fetch function) per scope set, with `authn.FromConfig(Basic user="ghcr-proxy", password=PAT)` or `authn.Anonymous`. If the library exposes no such function, hand back `HANDOFF: question`; don't use `transport.NewWithContext` as the forwarding transport, because it widens scopes from upstream challenges and resends requests itself.
- **Realm check:** the token realm from the challenge must have the scheme and host of `upstream.url` or one in `upstream.tokenHosts`. Otherwise no credential is sent; the request gets `502 UNAVAILABLE` "upstream token endpoint not allowed", and an error is logged.
- **Token cache:** key `(kind, PAT generation, sorted scopes)` where `kind` is `pat` or `anonymous`. A token is reused until `expires_in` minus 30 seconds (60 seconds total lifetime if absent). At most 4,096 entries, LRU. A PAT-backed token is never used for an anonymous or not-allowed request, and anonymous tokens are only ever fetched with `authn.Anonymous`.
- The forwarding round tripper sets `Authorization: Bearer <token>` on the rebuilt request and does nothing else: it never widens scopes, never resends on its own, never follows redirects.

### 7.2 Forwarding

- `httputil.ReverseProxy` with `Rewrite` (not `Director`): the outbound URL is `Request.UpstreamURL()`; `Host` is the upstream host; the client's `Authorization`, `Cookie`, `Proxy-*`, `Forwarded` and `X-Forwarded-*` headers are removed; nothing is added that reveals the client.
- Bodies stream. The only body the proxy buffers is a manifest `PUT`, up to 4 MiB (larger: `400 MANIFEST_INVALID` "manifest too large", nothing upstream), so it can be replayed.
- **Upstream `401` on a PAT-backed request:** drop that cached token, fetch a new one and retry once, if the request has no body or is a buffered manifest `PUT`. Otherwise, or if the retry also gets `401`: `502 UNAVAILABLE` "upstream rejected the proxy's credential".
- Upstream connection errors and timeouts: `502 UNAVAILABLE` "upstream unavailable".

### 7.3 Answers to the client

- Status, headers and body are passed through as the upstream sent them, except:
  - `WWW-Authenticate` from the upstream is always removed.
  - **Anonymous identity, pull, upstream answers `401`, `403` or `404`, or the anonymous token exchange is refused:** the proxy answers `401 UNAUTHORIZED` with its Basic challenge and a registry error body. BuildKit and the containerd resolver send the first request with no credentials and only call the credential helper after a challenge, so this is how private `FROM` and private pulls work. Callers with no credentials see "unauthorized" for missing images too, as on GHCR.
  - **ServiceAccount whose pull isn't allowed, upstream answers `401` or `403`, or the anonymous token exchange is refused:** `401 UNAUTHORIZED` with the Basic challenge (Docker then fails as it would on GHCR without access, AC-1-05).
- Every client-facing message is a fixed string from this page. Library errors (which carry upstream URLs and bodies) go to the process log only, never into responses or audit lines.

### 7.4 Location rewrite

If `Location` is absolute and its scheme and host equal the upstream's (host compared case-insensitively, default port removed on both sides), replace scheme and host with `https://` and the `Host` the client sent. Relative values pass unchanged. Any other host passes unchanged. `Link` headers are not rewritten. Integration tests send `Host: ghcr.io` to the test listener so they can check AC-1-22 literally.

## 8. TLS, health, readiness, shutdown

- Certificate: loaded with `tls.LoadX509KeyPair`, served through `tls.Config.GetCertificate`, reloaded on file change (same watcher as the PAT). Minimum TLS 1.2. A failed reload keeps the old pair and logs an error.
- `http.Server`: `ReadHeaderTimeout`, `IdleTimeout` and `MaxHeaderBytes` from config; no `ReadTimeout` or `WriteTimeout` (multi-GB uploads).
- `/healthz`: process alive. `/readyz`: informer synced, PAT loaded, certificate loaded. `/metrics`: Go and process collectors only in phase 1.
- SIGTERM: `/readyz` fails at once, the listener keeps serving for 5 seconds (endpoint removal), then `Shutdown` with `shutdownGracePeriod`. The chart sets `terminationGracePeriodSeconds` to `shutdownGracePeriod` + 30s.

## 9. Fake GHCR

`test/fakeghcr.New(t, opts) *Fake` starts two `httptest` TLS servers, **registry** and **storage**, sharing one CA the test also hands to clients:
- Registry: go-containerregistry's `registry.New(registry.WithBlobHandler(...))` with a **disk-backed blob handler** under `t.TempDir()` that streams (so a 2 GiB layer doesn't sit in memory), wrapped by:
  - auth: no or wrong bearer token → `401` with `WWW-Authenticate: Bearer realm="<registry>/token",service="ghcr.io",scope="repository:<name>:<actions>"`;
  - `/token`: Basic with the configured PAT grants any scope; anonymous grants `pull` on repositories marked public and answers `401` for anything else; returns `{"token":…,"expires_in":300}` (configurable, so tests can make tokens expire); tokens are opaque random strings mapped to granted scopes; requests outside a token's scopes get `401` with a challenge, like GHCR;
  - an anonymous request for a private repository gets `401` with a Bearer challenge;
  - upload `Location` headers made absolute (`https://<registry host>/v2/...`);
  - `GET` blob → `307` to `https://<storage host>/<digest>?sig=<random>`, served by the storage server.
- `Fake` exposes the PAT, `MakePublic(name)`, `Push(name, image)` helpers, `SetRealm(url)` (to test the realm check), and a request log for assertions (method, path, query keys, credential kind: PAT-backed token, anonymous token, none).

In e2e, the fake runs in the cluster as one Pod with two Services: `fakeghcr` (registry, the proxy's `upstream.url`) and `fakeghcr-storage`. Its certificate covers both Service DNS names and also `pkg-containers.githubusercontent.com`, and the storage redirect uses that name. The CI pod gets a `hostAliases` entry for that name pointing at `fakeghcr-storage`, and the same CA in dind's `certs.d/pkg-containers.githubusercontent.com/`. The test asserts the proxy's memory with `process_resident_memory_bytes` from its `/metrics` against the chart's memory limit (default `256Mi`).

The fake isn't a GHCR clone: don't add behaviour no scenario needs.

## 10. Audit

```go
type Entry struct {
    Time       time.Time `json:"time"`
    ClientIP   string    `json:"clientIP"`
    Namespace  string    `json:"namespace,omitempty"`
    SA         string    `json:"serviceAccount,omitempty"`
    Pod        string    `json:"pod,omitempty"`
    Method     string    `json:"method"`
    Path       string    `json:"path"`             // escaped path, never the query
    Repository string    `json:"repository,omitempty"`
    From       string    `json:"from,omitempty"`   // mount source, when forwarded
    Action     string    `json:"action"`           // none, pull, push, delete, unknown
    Decision   string    `json:"decision"`         // allowed, anonymous, denied, unauthenticated, unavailable, notfound, invalid
    Policy     string    `json:"policy,omitempty"` // the allowing policy's name
    Status     int       `json:"status"`           // what the client got
    Upstream   int       `json:"upstreamStatus,omitempty"`
    Bytes      int64     `json:"bytes"`
    DurationMS int64     `json:"durationMs"`
    Digest     string    `json:"digest,omitempty"` // manifest push/delete: Docker-Content-Digest from the upstream, else <ref> when it is a digest
}
```

Every request gets exactly one entry, including `404`, `401` and `503` answers. No header values, tokens or query strings.

## 11. Chart

`deploy/charts/ghcr-proxy`: Deployment (default 2 replicas, no `topologySpreadConstraints`: cluster defaults apply), Service (`https` 443 → 8443, `metrics` 8080), ServiceAccount, ClusterRole/Binding (`SPEC.md` section 5), Role/Binding for the lease, ConfigMap with the rendered config, CRDs from `config/crd/bases`. The PAT and TLS Secrets are mounted as directories (never `subPath`, which breaks file-change detection). `POD_NAMESPACE` from the downward API. Values: `image`, `replicas`, `resources` (memory request = limit, default `256Mi`; no CPU limit), `pat.existingSecret` + `pat.key`, `tls.existingSecret` (type `kubernetes.io/tls`), `service.clusterIP`, `upstream.*`, `authentication.*`, `server.*`, `grants`, `audit.enabled`, `podSecurityContext`/`securityContext` (non-root, read-only root filesystem, no privilege escalation, all capabilities dropped), `serviceMonitor.enabled` (default false). The chart never creates the PAT or TLS Secrets.

## 12. CI pod setup (admin guide, and the e2e pod)

`docs/admin/ci-pods.md` shows, and the e2e test uses, exactly this:
- `hostAliases`: `ghcr.io` → the proxy Service's ClusterIP (pinned with `service.clusterIP`).
- A projected volume `ghcr-proxy-token` with a `serviceAccountToken` source (`audience: ghcr-proxy`, `expirationSeconds: 3600`, `path: token`) mounted at `/var/run/secrets/ghcr-proxy` in the container that runs the Docker CLI.
- An image volume from `ghcr.io/soliddowant/ghcr-proxy-credential-helper` mounted at `/opt/ghcr-proxy/bin`, and `PATH` extended with it in that container.
- `~/.docker/config.json` with `{"credHelpers":{"ghcr.io":"ghcr-proxy"}}`, written by the step's setup.
- The CA at `/etc/docker/certs.d/ghcr.io/ca.crt` in the **dind** container (the daemon makes the registry connections).
- A `GHCRAccessPolicy` for the pod's ServiceAccount.

The e2e pod adds only the fake's storage host (section 9).

## 13. Credential helper details

- `get` input: `ghcr.io`, `https://ghcr.io`, `ghcr.io/…` or `https://ghcr.io/…` (any path) → answer; anything else → `credentials.NewErrCredentialsNotFound()`.
- Token file missing, unreadable or empty (after trimming whitespace): exit 1 with "ghcr-proxy credential helper: token file <path> is missing or empty"; never print the file's contents.

## 14. Errors

| Situation | Status | Registry error code | Message |
|---|---|---|---|
| Unknown route, `_catalog`, rejected path | 404 | `NOT_FOUND` | "not found" |
| Bad monolithic-upload digest | 400 | `DIGEST_INVALID` | "invalid digest" |
| Manifest over 4 MiB | 400 | `MANIFEST_INVALID` | "manifest too large" |
| Bad credential, anonymous push or delete, refused anonymous pull (section 7.3) | 401 | `UNAUTHORIZED` | "authentication required" |
| Denied by policy or grant | 403 | `DENIED` | section 6.2 |
| TokenReview error or timeout, replica not ready, PAT not loaded | 503 | `UNAVAILABLE` | "ghcr-proxy is not ready" / "token review unavailable" |
| PAT token exchange refused (`401`/`403`), or upstream `401` twice | 502 | `UNAVAILABLE` | "upstream rejected the proxy's credential" |
| Token realm not allowed | 502 | `UNAVAILABLE` | "upstream token endpoint not allowed" |
| Token endpoint or upstream unreachable, timed out, or `5xx` from the token endpoint | 502 | `UNAVAILABLE` | "upstream unavailable" |

`UNAVAILABLE` is not in the distribution spec's list; clients only show the message, which is enough.
