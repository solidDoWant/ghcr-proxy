# Phase 3 design: registrygate

Decides the behaviour of phase 3 on top of `SPEC.md`. Phases 1 and 2's designs still decide anything this page doesn't change: request classification, authentication, glob grammar and containment, forwarding, mirror fallback, audit and metrics plumbing. A case this page, the earlier designs and `SPEC.md` don't decide is a design question: hand it back.

Phase 3 replaces the single hard-wired upstream and the config-file grants with four namespaced custom resources. Most code moves rather than being rewritten. **Nothing is migrated**: 0.2.x was never deployed, so old types, config sections and code paths are deleted, not kept beside the new ones.

## 1. Rename (first issue; everything else waits for it)

One mechanical PR, so later PRs don't conflict with it:

| Old | New |
|---|---|
| Go module `github.com/solidDoWant/ghcr-proxy` | `github.com/solidDoWant/registrygate` |
| `cmd/ghcr-proxy`, `cmd/docker-credential-ghcr-proxy` | `cmd/registrygate`, `cmd/docker-credential-registrygate` |
| images `ghcr-proxy`, `ghcr-proxy-credential-helper` | `registrygate`, `registrygate-credential-helper` |
| chart `deploy/charts/ghcr-proxy` | `deploy/charts/registrygate` |
| API group `ghcr-proxy.soliddowant.dev` | `registrygate.soliddowant.dev` (the `GHCRAccessPolicy` kind keeps working until section 2's issue replaces it) |
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
    ResponseHeader *metav1.Duration `json:"responseHeader,omitempty"` // default 30s
    Idle           *metav1.Duration `json:"idle,omitempty"`           // default 5m
}
```

Kinds (all namespaced, `+kubebuilder:resource:categories=registrygate`, status subresource, printer columns `Ready` and `Reason`):

```go
type UpstreamRegistrySpec struct {
    Hosts             []string          `json:"hosts"`             // 1..16 unique lower-case DNS names, no port, no wildcard
    URL               string            `json:"url"`               // https://host[:port], no path, query or userinfo
    CASecretRef       *LocalRef         `json:"caSecretRef,omitempty"`   // key ca.crt
    TLS               ServingTLS        `json:"tls"`               // SecretRef LocalRef: keys tls.crt, tls.key
    TokenRealmHosts   []string          `json:"tokenRealmHosts,omitempty"` // <=8 DNS names (host or host:port)
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

CEL on `AccessRule`: exactly one of `credentialRef` and `mirrorRef`; with `mirrorRef`, `actions == ['pull']`. `Action` and the glob validation are phase 1's. The `http` scheme is allowed by the CRD for `url` fields, but the proxy treats an `http` URL as invalid (`Ready=False, reason=InvalidSpec`) unless it runs with `--allow-http` (tests only; the chart doesn't expose it).

Status for every kind: `ObservedGeneration int64`, `Conditions []metav1.Condition` (`Ready`). `RegistryAccessPolicyStatus` adds:

```go
// +listType=map
// +listMapKey=name
Rules []RuleStatus `json:"rules,omitempty"` // RuleStatus{Name string; Conditions []metav1.Condition}; condition type Accepted
```

Reasons (`Ready` / `Accepted`): `Ready`, `Accepted`, `InvalidSpec`, `NotAllowed`, `RefNotFound`, `RefNotReady`, `SecretUnavailable`, `SecretInvalid`, `HostConflict`, `CertificateMismatch`, `URLNotAllowed`, `OutsideCeiling`, `RulesNotAccepted` (policy `Ready=False` when any rule isn't accepted; the message lists the rule names). `GHCRAccessPolicy` and its controller, webhook and metrics are deleted.

## 3. The model (`internal/model`, new)

One package turns the informer caches into an immutable **snapshot** that serving, the status controller and the webhook all read. No other package interprets the objects, so status always says exactly what serving does.

- **Inputs:** informers on the four kinds (all namespaces), Namespaces (for `allowedNamespaces.selector`), and the Secret store (section 4).
- **Rebuild:** any change marks the snapshot dirty; a single goroutine rebuilds it at most every 100 ms and publishes it with `atomic.Pointer`. Object counts are small (hundreds), so a full rebuild is fine. Requests read one snapshot pointer for their whole decision.
- **Readiness**, computed in this order, each step using the previous one's results:
  1. `UpstreamRegistry` (only from admin namespaces; others are skipped and not given status): spec valid; serving Secret has a parseable key pair whose leaf verifies for every host (`x509.Certificate.VerifyHostname`); CA Secret valid if set; no host claimed by another `UpstreamRegistry` (both get `HostConflict`, naming the other).
  2. `RegistryMirror`: upstream exists and admits the mirror's namespace and is ready; URL matches an `allowedMirrorURLs` prefix (same scheme and host, and the prefix's path is a prefix of the mirror's path on a `/` boundary); Secrets valid.
  3. `UpstreamCredential`: upstream exists, admits, is ready; Secret has non-empty username and password.
  4. `RegistryAccessPolicy` rules: the referenced credential or mirror exists (`RefNotFound`), admits the policy's namespace (`NotAllowed`), and is ready (`RefNotReady`); a credential rule is inside the ceiling (`OutsideCeiling`, using phase 1's `glob.Contains` per action, with phase 1's implied-action rule if it applies); `RefNotReady` and `NotAllowed` rules are re-evaluated on every rebuild.
  5. `defaultMirrorRef`: must resolve to a ready mirror of that same upstream that admits the upstream's namespace; otherwise the upstream stays ready and pulls go upstream anonymously, and the upstream's `Ready` message says the default mirror isn't usable.
- **Indexes in the snapshot:** host → upstream (and its `tls.Certificate`); `(namespace, serviceAccount)` → accepted rules, each with its resolved upstream and its credential or mirror.
- **Admission check:** a namespace is admitted when it is the object's own, is in `names`, or its labels match `selector`. A Namespace that isn't in the cache isn't admitted.

## 4. Secrets (`internal/secrets`, new)

- **Admin namespaces** (`--admin-namespaces`, default the proxy's own): one namespace-scoped Secret informer each.
- **Every other referenced Secret:** one watch per Secret, a `cache.ListWatch` with field selector `metadata.name=<name>` in its namespace, started when the first object references it and stopped when the last reference goes. A `403` (no Role) marks it `SecretUnavailable` and retries with backoff (1 s doubling to 5 min) so adding the Role later works without a restart.
- The store hands the model `{data, resourceVersion}` or an error. Values never go into logs, status messages, events or errors; types holding them implement `String`, `GoString` and `LogValue` as redacted, as `internal/mirror.Credentials` does today.
- **Required RBAC** (chart): ClusterRole: `tokenreviews` create; the four kinds get/list/watch; their `/status` update and patch; `namespaces` get/list/watch; leases in its own namespace. Role in each admin namespace: Secrets get/list/watch. **Amends `SPEC.md` section 5:** add `namespaces` get/list/watch (needed for `allowedNamespaces.selector`).
- `internal/filewatch` and `internal/upstream/pat.go` are deleted; the serving certificate comes from Secrets now.

## 5. TLS by server name

`tls.Config.GetCertificate` looks up the lower-cased `ClientHelloInfo.ServerName` in the current snapshot. No match, or an empty server name: return an error (handshake fails). The snapshot keeps the parsed `tls.Certificate` per upstream, rebuilt when its Secret's `resourceVersion` changes; a renewed certificate applies to new connections. There is no certificate file flag any more.

## 6. Requests

### 6.1 Path canonicalization

Done on the raw request target (`r.RequestURI`'s path) before classification:

1. A path containing `%`: `400` (registry paths never need percent-encoding: names, references, digests and session tokens use only `[A-Za-z0-9._:+=~/-]`).
2. A path with an empty segment (`//`), or a `.` or `..` segment: `400`.
3. First segment exactly `v2`: classify (phase 1 route table). First segment equal to `v2` ignoring case but not exactly `v2`: `400`. Anything else: `404`.

Responses are registry JSON errors (`400` uses code `NAME_INVALID` for name-shaped problems, else `UNSUPPORTED`). Nothing goes upstream. Go's `http.ServeMux` must not be in front of this (it cleans paths and redirects); the handler sees the raw target.

### 6.2 Upload sessions

- `RouteUploadSession` becomes `/v2/<name>/blobs/uploads/<token>` only; the singular `upload` form and `uploadSegment` are deleted.
- **Issuing:** when an upload start or session response has a `Location`, the forwarder resolves it against the upstream URL. If its scheme and host aren't the upstream's, the client gets `502 UNAVAILABLE` "upstream returned an upload location on another host". If its path isn't under `/v2/<name>/blobs/` for the request's `<name>`, same `502`. Otherwise the client gets `Location: /v2/<name>/blobs/uploads/<token>` where `<token>` is the unpadded base64url encoding of the upstream path plus `?` and raw query (if any).
- **Using:** the token is decoded (invalid base64, longer than 2048 bytes decoded, or containing `#`: `404 BLOB_UPLOAD_UNKNOWN`); its path must again be under `/v2/<name>/blobs/` for the URL's own `<name>`, with no `.`/`..` or empty segments; otherwise `404 BLOB_UPLOAD_UNKNOWN`, nothing sent. The client's own query on a session request (`digest=` on the final `PUT`) is merged onto the decoded query; a key present in both is a `400`.
- The token is not signed. It can only name a path under the same repository's blobs on the upstream origin, and the upstream token is scoped to that repository.

### 6.3 Resolution (`internal/authz`, rewritten on the snapshot)

`Decide(snapshot, host, identity, request) Decision`, exactly `SPEC.md`'s "Resolution". Outcomes:

| Outcome | Forwarding |
|---|---|
| `Credential{upstream, credential}` | upstream, token from that credential |
| `Mirror{upstream, mirror}` | manifests and blobs (`GET` only; blob `HEAD` stays upstream) to the mirror; tag list and referrers to the upstream anonymously |
| `Anonymous{upstream, defaultMirror?}` | as `Mirror` with the default mirror if usable, else upstream anonymously |
| `Denied{message}` | `403 DENIED` |
| `Ambiguous{rules}` | `403 DENIED`, message `ambiguous rules: <ns>/<policy>/<rule>, …` (sorted, at most 5 listed) |
| `Challenge` | `401` with the Basic challenge |

- The version check `GET /v2/` is answered by the proxy itself: anonymous gets the `401` Basic challenge (so Docker sends its credential), an authenticated caller gets `200` with `Docker-Distribution-API-Version: registry/2.0`. Nothing goes upstream.
- A host with no ready upstream after the TLS handshake (it went away mid-connection): plain `404`, nothing sent.
- `mount`/`from`: the `pull` on `from` must resolve to `Credential` with the same credential (UID) as the push; otherwise drop both parameters (phase 1 behaviour).

### 6.4 Forwarding and upstream auth (`internal/upstream`, `internal/registry`)

- One `upstream.Client` per ready `UpstreamRegistry`, keyed by UID and rebuilt when its URL, CA or timeouts change: an `http.Transport` (no redirects followed, the CA if set), and a `Tokens` cache.
- `Tokens.Token(ctx, cred, scopes)`: `cred` is `{key: credential UID + Secret resourceVersion, username, password}` or anonymous. Ping `/v2/` (the challenge is cached per upstream for 5 minutes and re-pinged after a `401`): no challenge → no auth header; `Basic` → send `Authorization: Basic` with the credential (anonymous: none); `Bearer` → token exchange through go-containerregistry's transport with the realm check against the `url` host plus `tokenRealmHosts`. Tokens cached per `(cred key, sorted scopes)` until `expires_in` (60 s when absent or 0), dropped on a `401`. A credential's cache entries go when its key changes.
- `registry.Forwarder` takes the `Decision` and the upstream client instead of the fixed `Upstream`, `PolicyMirror` and `AnonymousMirror` fields. The REST delete (`restdelete.go`, `APIConfig`, go-github) is deleted; `DELETE` routes forward like any credential request with scope `repository:<name>:delete`.
- Rewrites: a `Location` on the upstream's (or mirror's) scheme and host is rewritten to `https://<client host>` (the TLS server name), path and query kept, except upload locations (section 6.2).

### 6.5 Mirrors (`internal/mirror`)

`mirror.Client` is built from a ready `RegistryMirror` and its Secrets instead of config files (`files.go` is deleted), cached by UID plus the `resourceVersion`s it used. Its behaviour, fallback rules and timeouts are phase 2's. `Send` path: `<url path>/v2/<pathPrefix>/<name>/…`.

## 7. Status controller (`internal/controller`)

Leader-elected. One reconciler per kind, each triggered by its own kind and by snapshot rebuilds (a channel source that enqueues every object whose computed status changed). It writes the snapshot's computed status with a status patch only when it differs (conditions compared without `lastTransitionTime`; `lastTransitionTime` moves only when status changes). `observedGeneration` is the generation the snapshot used.

## 8. Admission webhook (`internal/webhook`)

Validating, served by the proxy when `--webhook` is on (chart value, as phase 2). Two paths:

- `UpstreamRegistry` create/update: denied outside the admin namespaces.
- `RegistryAccessPolicy` create/update: each credential rule whose credential exists in the snapshot is checked against its ceiling; a rule outside it is denied with the rule name and the patterns. References that don't resolve are allowed (status reports them).

CEL covers the structural rules; the webhook doesn't repeat them. `failurePolicy: Ignore` (status still reports the problem), as phase 2.

## 9. Credential helper (`internal/credhelper`)

- `REGISTRYGATE_HOSTS`: comma-separated hosts, compared lower-cased after stripping scheme, path and a trailing `:443`. Unset or empty: every `get` answers "credentials not found".
- `configure`: reads `$DOCKER_CONFIG/config.json` (default `$HOME/.docker/config.json`) as a `map[string]json.RawMessage`, sets `credHelpers[<host>] = "registrygate"` for every listed host (keeping other `credHelpers` entries), and writes it back atomically (temp file plus rename, mode `0600`, directory created `0700`). A file that isn't a JSON object: exit 1 with a message, file untouched. Exit 1 with no hosts listed.

## 10. Config file, chart and docs

- The config file keeps only server settings: listen addresses, authentication (audience, review cache TTL), audit, webhook, `adminNamespaces`. `upstream`, `grants` and `mirrors` sections are deleted; an old file with them fails to load with a message pointing at the custom resources.
- Chart: CRDs, the RBAC of section 4, values for the config above. No upstream, credential or mirror values: admins create the objects themselves (the docs show them). The chart's NetworkPolicy option keeps an egress list value for upstream and mirror hosts.
- `docs/admin/`: install; one CA and Certificate per upstream (cert-manager, name constraints); `UpstreamRegistry`; shared credentials with ceilings; mirrors and the public-content-only warning; a CI pod (hostAliases, `certs.d`, helper, `configure`, token); projects that don't intercept a host. `docs/user/`: policies, tenant mirrors and the Role for their Secret, reading status, denied and ambiguous requests. CRD references regenerated.

## 11. Tests

- **`test/fakeregistry`** (renamed and generalized from `test/fakeghcr`; `cmd/fakeregistry` for e2e): `Options` gain `Auth` (`Bearer` with token endpoint as today, `Basic`, `None`), `UploadPath` (`"uploads"` default, `"upload"` for GHCR), `Delete` (`Supported` or `Unsupported` → `405 UNSUPPORTED`). The GitHub REST endpoints are deleted. The GHCR-like configuration is `Bearer` + `upload` + `Unsupported`.
- **Unit:** canonicalization table; upload token round trip and every refusal; resolution table (candidates, precedence, ambiguity, mounts, anonymous, no rule); readiness table for each kind; certificate selection; helper `configure` merges.
- **Integration (envtest):** the model and status from real objects, Secret watches including the `resourceNames` Role case (envtest with RBAC on: the proxy runs as a ServiceAccount-equivalent user), rotation.
- **e2e (kind):** in-cluster upstream A (GHCR-like fake) and upstream B (`Basic` auth, `uploads`, `Supported` delete), Harbor as today with two proxy-cache projects for upstream A (`ci-a-cache`, `ci-b-cache`) and robots created through Harbor's API in test setup, namespaces `ci-a` and `ci-b`, each CI pod set up per the admin docs. Phase 2's e2e tests are ported to the new objects, not kept beside them.
- **Live:** `test/live` builds its objects (one `UpstreamRegistry` for `ghcr.io`, one `UpstreamCredential` from `GHCR_LIVE_PAT_FILE` with ceiling `<owner>/registrygate-live-<run id>/**` and the bare name) in envtest; the REST cleanup client stays in the test (it's test code, not product code). AC-3-35 expects GHCR's `405` for delete.

## 12. Issue order

1. Rename (section 1). Blocks everything.
2. API types and CRDs (section 2); delete `GHCRAccessPolicy`. Blocks 3–9.
3. Secret store (section 4).
4. Model and snapshot (section 3). Needs 2, 3. `model:opus`.
5. Status controller (section 7). Needs 4.
6. TLS by server name (section 5). Needs 4.
7. Path canonicalization and upload tokens (sections 6.1, 6.2). Needs 1. `model:opus`.
8. Resolution (section 6.3). Needs 4. `model:opus`.
9. Upstream clients, forwarding and delete; mirrors from objects (sections 6.4, 6.5). Needs 8. `model:opus`.
10. Webhook (section 8). Needs 4.
11. Credential helper (section 9). Needs 1.
12. Fake registry generalization (section 11). Needs 1.
13. Config file and chart (section 10). Needs 9.
14. e2e port and new scenarios. Needs 9, 11, 12, 13.
15. Admin and user docs, docs walk-through test. Needs 14.
16. Live test port (operator runs it). Needs 9.
17. Metrics and audit fields. Needs 9.
