# Phase 3 design: registrygate

Decides the behaviour of phase 3 on top of `SPEC.md`. Phases 1 and 2's designs still decide anything this page doesn't change: request classification, authentication, glob grammar and containment, forwarding, mirror fallback, audit and metrics plumbing. A case this page, the earlier designs and `SPEC.md` don't decide is a design question: hand it back.

The phase 3 design review (#102) is answered in this text.

Phase 3 replaces the single hard-wired upstream and the config-file grants with four namespaced custom resources. Most code moves rather than being rewritten. **Nothing is migrated**: 0.2.x was never deployed, so old types, config sections and code paths are deleted, not kept beside the new ones.

## 1. Rename (first issue; everything else waits for it)

One mechanical PR, so later PRs don't conflict with it:

| Old | New |
|---|---|
| Go module `github.com/solidDoWant/ghcr-proxy` | `github.com/solidDoWant/registrygate` |
| `cmd/ghcr-proxy`, `cmd/docker-credential-ghcr-proxy` | `cmd/registrygate`, `cmd/docker-credential-registrygate` |
| images `ghcr-proxy`, `ghcr-proxy-credential-helper` | `registrygate`, `registrygate-credential-helper` |
| chart `deploy/charts/ghcr-proxy` | `deploy/charts/registrygate` |
| API group `ghcr-proxy.soliddowant.dev` | `registrygate.soliddowant.dev` (the `GHCRAccessPolicy` kind keeps working until issue 13 of section 12 removes it) |
| metric prefix `ghcr_proxy_` | `registrygate_` |
| default audience `ghcr-proxy`, Basic realm `ghcr-proxy` | `registrygate` |
| helper env `GHCR_PROXY_TOKEN_FILE`, default `/var/run/secrets/ghcr-proxy/token`, username `ghcr-proxy` | `REGISTRYGATE_TOKEN_FILE`, `/var/run/secrets/registrygate/token`, `registrygate` |
| `test/fakeghcr`, `cmd/fakeghcr` | unchanged in this PR (section 11 generalizes them) |

Docs, CI, Renovate rules, the Makefile and `CLAUDE.md` references follow. `CLAUDE.md` is the owner's file, so the worker lists the lines that need the new name in its PR description and the operator edits them. The live tests' package prefix becomes `registrygate-live-<run id>`.

## 2. API types (`api/v1alpha1`)

Shared types:

```go
type LocalRef struct {        // a Secret in the referring object's namespace
    Name string `json:"name"` // DNS-1123 subdomain
}
type ObjectRef struct {
    Namespace string `json:"namespace,omitempty"` // defaults to the referring object's namespace
    Name      string `json:"name"`
}
type CredentialsSecretRef struct {
    Name        string `json:"name"`
    UsernameKey string `json:"usernameKey,omitempty"` // default "username"
    PasswordKey string `json:"passwordKey,omitempty"` // default "password"
}
type AllowedNamespaces struct {
    Names    []string              `json:"names,omitempty"`    // CEL: <=256 unique
    Selector *metav1.LabelSelector `json:"selector,omitempty"`
}
type Timeouts struct {
    // From the request being sent to the response headers arriving. Default 30s.
    ResponseHeader *metav1.Duration `json:"responseHeader,omitempty"`
    // Ping plus token exchange, together. Default 10s.
    Token *metav1.Duration `json:"token,omitempty"`
}
// There is no whole-request limit: uploads of several GB must fit. Body
// reads and writes are bounded by the server's idle timeout (phase 1).
// A mirror's ResponseHeader timeout also decides its fallback (phase 2).
```

Kinds (all namespaced, `+kubebuilder:resource:categories=registrygate`, status subresource, printer columns `Ready` and `Reason`):

```go
type UpstreamRegistrySpec struct {
    Hosts             []string          `json:"hosts"`             // 1..16 unique lower-case DNS names, no port, no wildcard
    URL               string            `json:"url"`               // https://host[:port], no path, query or userinfo
    CASecretRef       *LocalRef         `json:"caSecretRef,omitempty"`   // key ca.crt
    TLS               ServingTLS        `json:"tls"`               // SecretRef LocalRef: keys tls.crt, tls.key
    TokenRealmHosts   []string          `json:"tokenRealmHosts,omitempty"` // <=8 DNS names (host or host:port)
    AnonymousPulls    string            `json:"anonymousPulls,omitempty"`  // Challenge (default) | Forward; section 6.3
    DefaultMirrorRef  *ObjectRef        `json:"defaultMirrorRef,omitempty"`
    AllowedMirrorURLs []string          `json:"allowedMirrorURLs,omitempty"` // <=32 https URL prefixes
    Timeouts          Timeouts          `json:"timeouts,omitempty"`
    AllowedNamespaces AllowedNamespaces `json:"allowedNamespaces,omitempty"`
}
type UpstreamCredentialSpec struct {
    UpstreamRef       ObjectRef            `json:"upstreamRef"`
    SecretRef         CredentialsSecretRef `json:"secretRef"`
    Ceiling           []AccessGrant        `json:"ceiling"`          // 1..32; AccessGrant{Repositories []string; Actions []Action}
    AllowedNamespaces AllowedNamespaces    `json:"allowedNamespaces,omitempty"`
}
type RegistryMirrorSpec struct {
    UpstreamRef        ObjectRef             `json:"upstreamRef"`
    URL                string                `json:"url"`          // https URL, no query or userinfo
    PathPrefix         string                `json:"pathPrefix"`   // 1..255, repository-name grammar
    SecretRef          *CredentialsSecretRef `json:"secretRef,omitempty"`
    CASecretRef        *LocalRef             `json:"caSecretRef,omitempty"`
    FallbackToUpstream *bool                 `json:"fallbackToUpstream,omitempty"` // default true
    Timeouts           Timeouts              `json:"timeouts,omitempty"`
    AllowedNamespaces  AllowedNamespaces     `json:"allowedNamespaces,omitempty"`
}
type RegistryAccessPolicySpec struct {
    ServiceAccounts []string     `json:"serviceAccounts"` // 1..64 unique DNS-1123 subdomain names
    // +listType=map
    // +listMapKey=name
    Rules []AccessRule `json:"rules"` // 1..64
}
type AccessRule struct {
    Name          string     `json:"name"`                    // DNS-1123 label
    CredentialRef *ObjectRef `json:"credentialRef,omitempty"`
    MirrorRef     *ObjectRef `json:"mirrorRef,omitempty"`
    Repositories  []string   `json:"repositories"`            // 1..32 globs, as phase 1
    Actions       []Action   `json:"actions"`                 // 1..3 unique of pull|push|delete
}
```

CEL on `AccessRule`: exactly one of `credentialRef` and `mirrorRef`; with `mirrorRef`, `actions == ['pull']`. The worker checks that the CRD still installs within the API server's CEL cost budget with 64 rules of 32 globs; if it doesn't, lower the rule limit to 32 and say so on the issue. The paths of `RegistryMirror.url` and of `allowedMirrorURLs` entries must not contain `%`, `.` or `..` segments, or empty segments (CEL, and checked again by the model). `ObjectRef.namespace` can't be defaulted by the API server (defaulting can't read the object's own namespace); the model treats an empty namespace as the referring object's. `Action` and the glob validation are phase 1's. The `http` scheme is allowed by the CRD for `url` fields, but the proxy treats an `http` URL as invalid (`Ready=False, reason=InvalidSpec`) unless it runs with `--allow-http` (tests only; the chart doesn't expose it).

Status for every kind: `ObservedGeneration int64`, `Conditions []metav1.Condition` (`Ready`). `RegistryAccessPolicyStatus` adds:

```go
// +listType=map
// +listMapKey=name
Rules []RuleStatus `json:"rules,omitempty"` // RuleStatus{Name string; Conditions []metav1.Condition}; condition type Accepted
```

Reasons (`Ready` / `Accepted`): `Ready`, `Accepted`, `InvalidSpec`, `NotAllowed`, `RefNotFound`, `RefNotReady`, `SecretUnavailable`, `SecretInvalid`, `HostConflict`, `CertificateMismatch`, `URLNotAllowed`, `OutsideCeiling`, `RulesNotAccepted` (policy `Ready=False` when any rule isn't accepted; the message lists the rule names). Mapping: a Secret that doesn't exist, or that the proxy may not read (`403`), is `SecretUnavailable`; a Secret that exists but lacks a key, has an empty value, or holds a key pair or CA that doesn't parse is `SecretInvalid`. `SPEC.md` lists the same reasons. `GHCRAccessPolicy` and its controller, webhook and metrics are deleted in the issue that replaces their last user (section 12), not when the new types land.

## 3. The model (`internal/model`, new)

One package turns the informer caches into an immutable **snapshot** that serving, the status controller and the webhook all read. No other package interprets the objects, so status always says exactly what serving does.

- **Inputs:** informers on the four kinds (all namespaces), Namespaces (for `allowedNamespaces.selector`), and the Secret store (section 4).
- **Rebuild:** any change marks the snapshot dirty; a single goroutine rebuilds it at most every 100 ms and publishes it with `atomic.Pointer`. Object counts are small (hundreds), so a full rebuild is fine. Requests read one snapshot pointer for their whole decision.
- **Readiness**, computed in this order, each step using the previous one's results:
  1. `UpstreamRegistry` (only from admin namespaces; others are skipped and not given status): spec valid; serving Secret has a parseable key pair whose leaf verifies for every host (`x509.Certificate.VerifyHostname`); CA Secret valid if set; no host claimed by another `UpstreamRegistry` whose spec is valid (both get `HostConflict`, naming the other; an object with an invalid spec claims no hosts).
  2. `RegistryMirror`: upstream exists and admits the mirror's namespace and is ready; URL matches an `allowedMirrorURLs` prefix (same scheme and host, and the prefix's path is a prefix of the mirror's path on a `/` boundary); Secrets valid.
  3. `UpstreamCredential`: upstream exists, admits, is ready; Secret has non-empty username and password.
  4. `RegistryAccessPolicy` rules: the referenced credential or mirror exists (`RefNotFound`), admits the policy's namespace (`NotAllowed`), and is ready (`RefNotReady`); **the rule's upstream also admits the policy's namespace** (`NotAllowed`); a credential rule is inside the ceiling (`OutsideCeiling`, using phase 1's `glob.Contains` per action). **`push` and `delete` imply `pull`**, as in phase 1: for the ceiling check (a rule with `push` needs `pull` in the ceiling too) and for candidates (a `push` rule is a candidate for pulls of its repositories). Admission and readiness are re-evaluated on every rebuild.
     - A mirror rule whose mirror exists and admits but isn't ready stays in the index, marked not ready, so resolution can treat it as a failed mirror (section 6.3). It is reported `Accepted=False, reason=RefNotReady`.
  5. `defaultMirrorRef`: must resolve to a ready mirror of that same upstream that admits the upstream's namespace; otherwise the upstream stays ready and pulls go upstream anonymously, and the upstream's `Ready` message says the default mirror isn't usable.
- **Indexes in the snapshot:** host → upstream (and its `tls.Certificate`); `(namespace, serviceAccount)` → accepted rules, each with its resolved upstream and its credential or mirror.
- **Pending Secrets (H2):** an object whose Secret is still `pending` (section 4) has no readiness yet. The snapshot marks it pending; resolution answers requests that need it with `503 UNAVAILABLE` "registrygate is not ready", and the status controller writes nothing for it.
- **Replica readiness:** `/readyz` is `200`, and authenticated requests and webhook decisions are served, only when all five informers have synced, a snapshot has been built, and no Secret the snapshot references is pending. Before that, requests get `503 UNAVAILABLE` "registrygate is not ready" (replacing phase 1's "ghcr-proxy is not ready"). Once ready, a pending Secret affects only the objects that use it.
- **Admission check:** a namespace is admitted when it is the object's own, is in `names`, or its labels match `selector`. A Namespace that isn't in the cache isn't admitted. `selector: {}` admits every namespace (the user docs say so).

## 4. Secrets (`internal/secrets`, new)

- **Admin namespaces** (`--admin-namespaces`, default the proxy's own): one namespace-scoped Secret informer each.
- **Every other referenced Secret:** one watch per Secret, a `cache.ListWatch` with field selector `metadata.name=<name>` in its namespace, started when the first object references it and stopped when the last reference goes. A `403` (no Role) marks it `SecretUnavailable` and retries with backoff (1 s doubling to 5 min) so adding the Role later works without a restart.
- Each Secret is in one of four states: `pending` (watch started, first list not returned yet), `present` (`{data, resourceVersion}`), `notFound`, `forbidden`. The store hands the model the state; `notFound` and `forbidden` both map to `SecretUnavailable`. Values never go into logs, status messages, events or errors; types holding them implement `String`, `GoString` and `LogValue` as redacted, as `internal/mirror.Credentials` does today.
- **Required RBAC** (chart): ClusterRole: `tokenreviews` create; the four kinds get/list/watch; their `/status` update and patch; `namespaces` get/list/watch; leases in its own namespace. Role in each admin namespace: Secrets get/list/watch. **Amends `SPEC.md` section 5:** add `namespaces` get/list/watch (needed for `allowedNamespaces.selector`).
- `internal/filewatch` and `internal/upstream/pat.go` are deleted by issue 9 of section 12; the serving certificate comes from Secrets now.

## 5. TLS by server name

`tls.Config.GetCertificate` looks up the lower-cased `ClientHelloInfo.ServerName` in the current snapshot. No match, or an empty server name: return an error (handshake fails). The snapshot keeps the parsed `tls.Certificate` per upstream, rebuilt when its Secret's `resourceVersion` changes; a renewed certificate applies to new connections. There is no certificate file flag any more.

## 6. Requests

### 6.1 Path canonicalization

Done on the raw request target (`r.RequestURI`'s path) before classification:

1. A path containing `%`: `400` (registry paths never need percent-encoding: names, references, digests and session tokens use only `[A-Za-z0-9._:+=~/-]`).
2. A path with an empty segment (`//`), or a `.` or `..` segment: `400`.
3. First segment exactly `v2`: classify (phase 1 route table). First segment equal to `v2` ignoring case but not exactly `v2`: `400`. Anything else: `404`.

A single trailing `/` is not an empty segment for rule 2 (`/v2/` and `/v2/<name>/blobs/uploads/` need it); the route table decides whether a trailing `/` is allowed. The path length limit is phase 1's 2048 bytes, except 4096 for the upload session route (its token is long).

**The deciding host** is the TLS server name. The `Host` header must name one of the same `UpstreamRegistry`'s hosts (with or without `:443`); otherwise `400`, nothing sent.

Responses are registry JSON errors (`400` uses code `NAME_INVALID` for name-shaped problems, else `UNSUPPORTED`). Nothing goes upstream. Go's `http.ServeMux` must not be in front of this (it cleans paths and redirects); the handler sees the raw target.

### 6.2 Upload sessions

- `RouteUploadSession` becomes `/v2/<name>/blobs/uploads/<token>` only. The singular `upload` form, `uploadSegment` and phase 1's `<session>` grammar (`sessionPattern`) are deleted; the token grammar below replaces it.
- **Signing key (C1).** Tokens are signed with HMAC-SHA-256 (`crypto/hmac`, `crypto/sha256`; compare with `hmac.Equal`). The key is 32 random bytes in key `key` of a Secret in the proxy's namespace, named by `--upload-signing-secret`. The chart creates it (`randAlphaNum` kept across upgrades with `lookup`) unless the admin names an existing one. Every replica reads it through the Secret store (admin namespace), so it can be rotated; rotating it fails uploads in flight, which clients retry (the admin docs say so). Until it is loaded the replica isn't ready.
- **Issuing.** Only a `202 Accepted` answer to an upload start or an upload session request is tokenized. A `201 Created` `Location` (mount success, final `PUT`) gets the ordinary host rewrite of section 6.4 (M5). The forwarder resolves the `Location` against the upstream URL. If its scheme and host aren't the upstream's, or its path doesn't start with `/v2/<name>/blobs/` for the request's `<name>`, the client gets `502 UNAVAILABLE` "upstream returned an unusable upload location". Otherwise:
  - `suffix` is the path after `/v2/<name>/blobs/`, plus `?` and the raw query if there is one;
  - `payload` is `base64url(suffix)` (unpadded);
  - `mac` is `base64url(HMAC(key, upstream UID + "\n" + <name> + "\n" + suffix))` (unpadded, 43 characters);
  - the client gets `Location: /v2/<name>/blobs/uploads/<payload>.<mac>`.
- **Grammar.** `<token>` is `[A-Za-z0-9_-]{1,2800}\.[A-Za-z0-9_-]{43}`; anything else is `404 BLOB_UPLOAD_UNKNOWN`.
- **Using.** Recompute the MAC with the upstream UID of the request's own host and the URL's own `<name>`; a mismatch is `404 BLOB_UPLOAD_UNKNOWN` and nothing is sent. Then decode `suffix`. It must not contain `#`, `.`/`..` or empty segments (else `404`). The upstream URL is `/v2/<name>/blobs/` + suffix. The MAC binds the token to the repository and upstream it was issued for, so a forged suffix (for example one that reaches `/v2/a/blobs/x/manifests/latest`) can't be used; the structural checks are a second layer.
- **Client query.** On a session request the client may send only `digest`, and only on `PUT`; any other key, or `digest` also present in the decoded query, is `400` (L5). It is added to the decoded query.
- The token is opaque to clients and never logged in full (audit shows the repository only).

### 6.3 Resolution (`internal/authz`, rewritten on the snapshot)

`Decide(snapshot, host, identity, request) Decision`, exactly `SPEC.md`'s "Resolution". Outcomes:

| Outcome | Forwarding |
|---|---|
| `Credential{upstream, credential}` | upstream, token from that credential |
| `Mirror{upstream, mirror}` | manifests (`GET` and `HEAD`) and blobs (`GET`) to the mirror; blob `HEAD`, tag list and referrers to the upstream anonymously (phase 2's split, H3) |
| `Anonymous{upstream, defaultMirror?}` | as `Mirror` with the default mirror if usable, else upstream anonymously |
| `Denied{message}` | `403 DENIED` |
| `Ambiguous{rules}` | `403 DENIED`, message `ambiguous rules: <ns>/<policy>/<rule>, …` (sorted, at most 5 listed) |
| `Challenge` | `401` with the Basic challenge |
| `NotReady` | `503 UNAVAILABLE` "registrygate is not ready" (a needed object is pending, section 3) |

**Anonymous requests and `anonymousPulls` (C2).** BuildKit and the containerd image store try every pull anonymously first and call the credential helper only after a `401`. If the proxy served those anonymous pulls, it would never learn the caller, and per-namespace mirrors would never be used. So each `UpstreamRegistry` has `anonymousPulls`:

- `Challenge` (default): **every** request with no `Authorization` header gets `Challenge`. Clients retry with their ServiceAccount token, and resolution then runs as an authenticated caller. Pods that intercept the host but have no helper can't pull from it; the admin docs say so.
- `Forward`: anonymous pulls resolve to `Anonymous{defaultMirror}` as in `SPEC.md` (for admins who intercept hosts in pods without the helper). Per-namespace mirrors then only see clients that authenticate first.

For an **authenticated** caller with no candidate, a pull is `Anonymous{defaultMirror}` in both modes. e2e sets `Challenge` on upstreams A and B; AC-3-24's anonymous case is tested with `Forward`. Amends `SPEC.md` (field and "Resolution" step 6).

**Rules:**

- `H` is the TLS server name (section 6.1).
- **A mirror rule whose mirror isn't ready** (M1) is still a candidate. It resolves to `Mirror` with a mirror that counts as failed: the pull goes to the upstream anonymously if `fallbackToUpstream`, else `502 "mirror unavailable"`. It never silently moves to the default mirror. A credential rule whose credential isn't ready isn't a candidate.
- **Final answers for anonymous outcomes** (M2): phase 1's section 7.3 mapping applies to the final answer of an `Anonymous` outcome, whichever target served it, including the default mirror: a `401`, `403` or `404` becomes the proxy's Basic challenge, so a private `FROM` still gets a challenge and retries with its token. A `401`/`403` from a mirror gives `502` only for `Mirror` outcomes.
- The version check `GET /v2/` is answered by the proxy itself: anonymous gets the `401` Basic challenge (so Docker sends its credential), an authenticated caller gets `200` with `Docker-Distribution-API-Version: registry/2.0`. Nothing goes upstream.
- A host with no ready upstream after the TLS handshake (it went away mid-connection): plain `404`, nothing sent.
- `mount`/`from`: the `pull` on `from` must resolve to `Credential` with the same credential (UID) as the push; otherwise drop both parameters (phase 1 behaviour).

### 6.4 Forwarding and upstream auth (`internal/upstream`, `internal/registry`)

- One `upstream.Client` per ready `UpstreamRegistry`, keyed by UID, and rebuilt when the object's `generation` or its CA Secret's `resourceVersion` changes (M6): an `http.Transport` (no redirects followed, the CA if set), the realm allow-list, the timeouts, and a `Tokens` cache.
- `Tokens.Token(ctx, cred, scopes)`: `cred` is `{key: credential UID + Secret resourceVersion, username, password}` or anonymous.
  - Ping `/v2/`; the challenge is cached per upstream for 5 minutes. No challenge → no auth header; `Basic` → send `Authorization: Basic` with the credential (anonymous: none); `Bearer` → token exchange through go-containerregistry's transport with the realm check against the `url` host plus `tokenRealmHosts`.
  - **Per-request challenges (M8):** when an upstream answers a request `401` with a `WWW-Authenticate` challenge, that challenge (realm-checked) replaces the cached ping result for the upstream, and the request is retried once (if replayable) with it. So an upstream whose `/v2/` is open but whose repositories challenge still works.
  - Tokens are cached per `(cred key, sorted scopes)` until 30 seconds before `expires_in` (phase 1's margin, L3; 60 s when absent or 0), and dropped on a `401`. A credential's cache entries go when its key changes.
  - Ping plus token exchange are bounded by `timeouts.token`; the request itself by `timeouts.responseHeader` until headers arrive.
- `registry.Forwarder` takes the `Decision` and the upstream client instead of the fixed `Upstream`, `PolicyMirror` and `AnonymousMirror` fields. The REST delete (`restdelete.go`, `APIConfig`, go-github) is deleted; `DELETE` routes forward like any credential request with scope `repository:<name>:delete`.
- **Rewrites.**
  - Upstream `Location` on the upstream's scheme and host: rewritten to `https://<server name>`, path and query kept, except tokenized upload locations (section 6.2).
  - Mirror `Location` on the mirror's scheme and host (M4): `<url path>/v2/<pathPrefix>/` is stripped back to `/v2/` and the host rewritten as above; a path outside that prefix is a failed mirror (`502`, or fallback), as phase 2.
  - Anything else is passed through untouched.

### 6.5 Mirrors (`internal/mirror`)

`mirror.Client` is built from a ready `RegistryMirror` and its Secrets instead of config files (`files.go` is deleted). It is cached by mirror UID and rebuilt when the mirror's `generation`, its upstream's `allowedMirrorURLs`, or a Secret `resourceVersion` it used changes (M6). Its behaviour, fallback rules and response-header timeout are phase 2's. `Send` path: `<url path>/v2/<pathPrefix>/<name>/…`.

## 7. Status controller (`internal/controller`)

Leader-elected. One reconciler per kind, each triggered by its own kind and by snapshot rebuilds (a channel source that enqueues every object whose computed status changed). It writes the snapshot's computed status with a status patch only when it differs (conditions compared without `lastTransitionTime`; `lastTransitionTime` moves only when status changes). `observedGeneration` is the generation the snapshot used. It writes nothing for an object the snapshot marks pending (section 3), so a restart or failover doesn't flap status while Secret watches start.

## 8. Admission webhook (`internal/webhook`)

Validating, served by the proxy when `--webhook` is on (chart value, as phase 2). Two paths:

- `UpstreamRegistry` create/update: denied outside the admin namespaces.
- `RegistryAccessPolicy` create/update: each credential rule whose credential exists in the snapshot is checked against its ceiling; a rule outside it is denied with the rule name and the patterns. References that don't resolve are allowed (status reports them).

Both paths keep phase 2's exemption (M7): an `UPDATE` whose `spec` is unchanged, or of an object with a `deletionTimestamp`, is always admitted, so labels and finalizers can change after an admin narrows a ceiling. CEL covers the structural rules; the webhook doesn't repeat them. `failurePolicy: Fail`, as phase 2; the webhook is only registered when the replica serving it is ready.

## 9. Credential helper (`internal/credhelper`)

- `REGISTRYGATE_HOSTS`: comma-separated hosts, compared lower-cased after stripping scheme, path and a trailing `:443`. Unset or empty: every `get` answers "credentials not found".
- `configure` (L5): the `credHelpers` keys it writes are the normalized hosts (no scheme, path or `:443`). An existing `credHelpers` that isn't a JSON object: exit 1, file untouched. It reads `$DOCKER_CONFIG/config.json` (default `$HOME/.docker/config.json`) as a `map[string]json.RawMessage`, sets `credHelpers[<host>] = "registrygate"` for every listed host (keeping other `credHelpers` entries), and writes it back atomically (temp file plus rename, mode `0600`, directory created `0700`). A file that isn't a JSON object: exit 1 with a message, file untouched. Exit 1 with no hosts listed.

## 10. Config file, chart and docs

- The config file keeps only server settings: listen addresses, authentication (audience, review cache TTL), audit, webhook, `adminNamespaces`. `upstream`, `grants` and `mirrors` sections are deleted; an old file with them fails to load with a message pointing at the custom resources.
- Chart: CRDs, the RBAC of section 4, values for the config above. No upstream, credential or mirror values: admins create the objects themselves (the docs show them). The chart's NetworkPolicy option keeps an egress list value for upstream and mirror hosts.
- `docs/admin/`: install; one CA and Certificate per upstream (cert-manager, name constraints); `UpstreamRegistry`; shared credentials with ceilings; mirrors and the public-content-only warning; a CI pod (hostAliases, `certs.d`, helper, `configure`, token); projects that don't intercept a host. `docs/user/`: policies, tenant mirrors and the Role for their Secret, reading status, denied and ambiguous requests, and that `selector: {}` admits every namespace. CRD references regenerated. The admin docs turn on cert-manager's `NameConstraints` feature gate, which name-constrained CAs need.
- The chart also creates the upload signing Secret (section 6.2) unless `uploadSigning.existingSecret` is set.

### 10.1 Metrics (M9)

Phase 2's table with these changes; the prefix is `registrygate_`.

| Metric | Labels |
|---|---|
| `registrygate_requests_total` | `upstream` (`<namespace>/<name>`, or `none` before a host resolves), `action`, `decision` (allowed, mirror, anonymous, denied, ambiguous, unauthenticated, unavailable, notfound, invalid), `code` |
| `registrygate_upstream_request_duration_seconds` | `upstream`, `target` (`upstream`, `mirror`); phase 2's buckets |
| `registrygate_upstream_requests_total` | `upstream`, `target`, `code` |
| `registrygate_mirror_fallbacks_total` | `upstream` |
| `registrygate_token_reviews_total` | unchanged |
| `registrygate_upstream_token_fetches_total` | `upstream`, `target`, `result` |
| `registrygate_objects` | gauge: `kind` (the four kinds), `ready` (true, false, pending), from the snapshot |
| `registrygate_secret_updates_total` | `result` (ok, invalid, unavailable) |

`ghcr_proxy_policies` and `credential_reloads_total` are gone. No other object-name labels.

### 10.2 Audit fields (M9)

Phase 1's `Entry`, plus: `host` (server name), `upstream`, `policy` and `credential` and `mirror` as `<namespace>/<name>` (empty when not used), `rule` (the rule name). For `ambiguous`, `rule` lists the rule names joined by `,`. Never a token, Secret value or upload token.

### 10.3 Docs walk-through harness (AC-3-34, M9)

The walk-through test runs only commands from `docs/admin/`, except that the harness may: install the two fake registries and Harbor, and trust their test CA in the proxy (`caSecretRef`) and in Harbor; create Harbor's registry endpoints, proxy-cache projects and robots through Harbor's API; and substitute the fakes' in-cluster URLs for `ghcr.io`/`quay.io` in `UpstreamRegistry.url`. Everything else, including cert-manager with the `NameConstraints` gate, comes from the docs.

## 11. Tests

- **`test/fakeregistry`** (renamed and generalized from `test/fakeghcr`; `cmd/fakeregistry` for e2e): `Options` gain `Auth` (`Bearer` with token endpoint as today, `Basic`, `None`), `UploadPath` (`"uploads"` default, `"upload"` for GHCR), `Delete` (`Supported` or `Unsupported` → `405 UNSUPPORTED`). The GitHub REST endpoints are deleted. The GHCR-like configuration is `Bearer` + `upload` + `Unsupported`. With every `Auth`, repositories marked public are served to anonymous requests; a private one gets `401` with that mode's challenge (H5).
- **Unit:** canonicalization table; upload token round trip and every refusal; resolution table (candidates, precedence, ambiguity, mounts, anonymous, no rule); readiness table for each kind; certificate selection; helper `configure` merges.
- **Integration (envtest):** the model and status from real objects, Secret watches including the `resourceNames` Role case (envtest with RBAC on: the proxy runs as a ServiceAccount-equivalent user), rotation.
- **e2e (kind):**
  - in-cluster upstream A (GHCR-like fake) and upstream B (`Basic` auth, `uploads`, `Supported` delete), both with `anonymousPulls: Challenge`;
  - Harbor as today, with a registry endpoint for each upstream (no credential), and proxy-cache projects `ci-a-a`, `ci-a-b`, `ci-b-a` and `ci-b-b` (`<namespace>-<upstream>`), robots created through Harbor's API in test setup (H5);
  - namespaces `ci-a` and `ci-b`, each with a `RegistryMirror` per upstream and the `resourceNames` Role for its robot Secret, and a CI pod set up per the admin docs;
  - phase 2's e2e tests are ported to the new objects, then deleted (section 12).
- **Live:** `test/live` builds its objects (one `UpstreamRegistry` for `ghcr.io`, one `UpstreamCredential` from `GHCR_LIVE_PAT_FILE` with ceiling `<owner>/registrygate-live-<run id>/**` and the bare name) in envtest; the REST cleanup client stays in the test (it's test code, not product code). AC-3-35 expects GHCR's `405` for delete.

## 12. Issue order

The order keeps `master` building and green after every issue (H6). New code lands beside the old; the old code is deleted by the issue that replaces its last user.

1. **Rename** (section 1). Blocks everything.
2. **API types and CRDs** (section 2). Adds the four kinds **beside** `GHCRAccessPolicy`; deletes nothing. Needs 1.
3. **Secret store** (section 4). Needs 2.
4. **Model and snapshot** (section 3), including pending and replica readiness. Needs 2, 3. `model:opus`.
5. **Status controller** (section 7) for the new kinds; deletes the `GHCRAccessPolicy` controller. Needs 4.
6. **TLS by server name** (section 5), as a `GetCertificate` on the snapshot; not wired into `main` yet. Needs 4.
7. **Path canonicalization and the upload token codec** (sections 6.1, 6.2): canonicalization in the handler, the token codec and validation as a pure package taking a key. Needs 1. `model:opus`.
8. **Resolution** (section 6.3) on the snapshot. Needs 4. `model:opus`.
9. **Forwarding and wiring** (sections 6.4, 6.5, readiness): upstream clients, challenge auth, delete, mirrors from objects, issuing and using upload tokens in the forwarder, and **wiring everything into `cmd/registrygate`**: the model, the Secret store, TLS selection, `Decide`, the forwarder, the signing key and the readiness gate. Deletes the old index, `Effective`, grants, the PAT holder, `filewatch`, config-file mirrors and the REST delete, and ports or deletes their tests. Needs 5, 6, 7, 8, 12. `model:opus`.
10. **Webhook** (section 8) for the new kinds; deletes the old validator. Needs 4.
11. **Credential helper** (section 9). Needs 1.
12. **Fake registry generalization** (section 11). Needs 1.
13. **Config file and chart** (section 10): deletes the old config sections, `GHCRAccessPolicy` CRD and Go type, and adds the signing Secret. Needs 9, 10.
14. **e2e port and new scenarios**; deletes the phase 2 e2e tests. Needs 11, 12, 13.
15. **Docs and the walk-through test.** Needs 14.
16. **Live test port** (operator runs it). Needs 9.
17. **Metrics and audit fields** (sections 10.1, 10.2). Needs 9.
