# registrygate specification

registrygate is an authenticating, transparent reverse proxy for container registries. It runs in a Kubernetes cluster and stands in for registries such as `ghcr.io` and `quay.io` inside CI pods. Pods authenticate with their own Kubernetes ServiceAccount token. Namespaced custom resources declare which upstream registries the proxy stands in for, which credentials a namespace may push with, which pull-through mirror (for example a Harbor proxy-cache project) serves its pulls, and what each ServiceAccount may do. Pods never see an upstream or mirror credential.

The project was called registrygate up to 0.2.x and served only `ghcr.io`. Phases 1 and 2 below were built under that name. Phase 3 generalizes it (issue #98) and renames it.

This file is the authoritative reference. Each phase is done when its checklist in `docs/acceptance/` passes (see "Phases").

## 1. Purpose and reference behaviour

**The owner's goal.** The owner runs self-hosted CI: Tekton, one `ci-<repository>` namespace per repository, builds with the Docker CLI against a `dockerd` dind sidecar in a kata VM. They want:
- every image pull a build makes, by `dockerd` and by BuildKit, from any configured registry, to go through a Harbor proxy-cache project that belongs to **that CI project**, so storage quotas, retention and cost are tracked per project;
- pushes (and deletes) authenticated with the pod's ServiceAccount token, so builds never hold a registry credential;
- all of this with no change to Dockerfiles, pipeline scripts or image references.

`dockerd` can't do this by itself: its `registry-mirrors` covers Docker Hub only and can't add Harbor's project path, and with the containerd image store it ignores `hosts.toml` mirrors. BuildKit mirrors cover only builds and hold one credential per host. A proxy that intercepts the registry hosts and holds the credentials itself covers both (issue #98 has the proof of concept).

**How it is used.** CI pods resolve each intercepted registry host (`ghcr.io`, `quay.io`, …) to the proxy's Service (pod `hostAliases`). `dockerd` trusts the proxy's certificate for each host through `/etc/docker/certs.d/<host>/ca.crt`. The Docker CLI gets its credential from `docker-credential-registrygate`, a static credential helper shipped as its own image and mounted as an image volume. The helper returns the pod's projected ServiceAccount token. Builds keep using plain `ghcr.io/<owner>/<image>` and `quay.io/<org>/<image>` references.

A project that needs a registry's own API (for example Harbor's `/api/v2.0/` in harbor-operator's e2e tests) doesn't intercept that host: its pods reach the registry directly with their own credentials, and network policy decides who may. The proxy serves only the registry API.

**Reference behaviour.** Where this spec doesn't decide a behaviour, decide it in this order:
1. What the upstream registry itself does for the same request, so a client can't tell the proxy apart from the upstream except for what policy denies. The proxy follows the OCI Distribution Specification v1.1 and the Docker Registry HTTP API v2.
2. What well-behaved registry front ends do (Harbor's proxy, distribution's proxy mode, Zot): error bodies in the registry JSON error format, standard status codes.
3. The simplest behaviour that keeps the principles.

Record the choice on the issue.

**Users:**
- **Admins** install the proxy with Helm, create `UpstreamRegistry` objects with their serving certificates, shared `UpstreamCredential` and `RegistryMirror` objects, and set up CI pods.
- **Chart authors** (the owner's CI chart, `cluster/charts/ci/project` in the owner's infra repo) create each project's `RegistryAccessPolicy`, its own `RegistryMirror` objects and the Role that lets the proxy read their Secrets.
- **The owner** is both.

## 2. Principles

Non-negotiable. Tests enforce them, and only the owner can change them.

1. **Every credential goes only to its own endpoint.** An `UpstreamCredential`'s Secret is sent only to its upstream's registry and token endpoint. A `RegistryMirror`'s Secret is sent only to that mirror. No credential is ever logged, returned to a client, or put in status, events, metrics or errors.
2. **A caller gets no more than its policy, and a policy no more than what it references allows.** A request uses a credential or a mirror only when a `RegistryAccessPolicy` in the caller's own namespace names the caller's ServiceAccount, has a rule that matches the repository and action, and that rule is inside the referenced credential's ceiling and admitted by every referenced object's `allowedNamespaces`. Everything else is forwarded anonymously (pulls) or denied.
3. **Fail closed.** If the proxy can't decide (token review unavailable, objects not yet synced, a Secret missing or invalid, rules that disagree) it denies, or forwards a pull anonymously. It never guesses which credential or mirror to use.
4. **Transparent to Docker.** A Docker client set up as in "How it is used" can push, pull and delete exactly as against the upstream, with no changes to image references, Dockerfiles or commands.
5. **Done means the owner's checklist passes.** Nobody raises the bar beyond the checklist and principles 1–4.

## 3. Target environment

- Kubernetes 1.33 or later with the `ImageVolume` feature on (needed only for mounting the credential helper). The `TokenReview` API must be reachable from the proxy.
- Clients: Docker CLI and daemon 25 or later, including BuildKit through `docker build` and `docker buildx`. Other OCI clients (crane, oras, skopeo) should work but aren't tested.
- Upstreams: any registry that implements the Distribution API with anonymous access, Basic auth, or the Docker token flow (Bearer challenge, token endpoint taking Basic credentials). Tested: `ghcr.io` (live, by hand), a fake GHCR, a second fake registry, and Harbor 2.15. `quay.io` and `mirror.gcr.io` are expected to work; Docker Hub isn't tested.
- Mirrors: any registry that serves an upstream's content under a path prefix, for example a Harbor proxy-cache project. **Public content only:** a mirror must reach its upstream anonymously (see "Security model").

## 4. Architecture

One Go binary, `registrygate`, plus the credential helper binary `docker-credential-registrygate`.

The proxy runs several stateless replicas behind a Service. Each replica:

1. **Serves TLS** on port 8443, choosing the certificate by the TLS server name (SNI) from the `UpstreamRegistry` that claims that host. Serves `/healthz`, `/readyz` and `/metrics` on a separate plain-HTTP port.
2. **Authenticates** each request (see "Authentication").
3. **Resolves** the request to a rule, credential or mirror from in-memory informer caches of the four kinds and their Secrets (see "Resolution").
4. **Forwards** it to the upstream or a mirror (see "Forwarding") and rewrites what the client must see (see "Rewrites").
5. **Writes one audit log line** per request (see "Audit").

A leader-elected controller in the same binary writes status for all four kinds. Serving doesn't need the leader: every replica decides from its own caches. A replica isn't ready until its caches have synced.

### Custom resources

API group `registrygate.soliddowant.dev`, version `v1alpha1`. All kinds are namespaced and in the category `registrygate` (`kubectl get registrygate -A`).

**References and admission** follow harbor-operator's pattern. Every reference is `{namespace, name}`, with `namespace` defaulting to the referring object's own. Every object that others point at has `allowedNamespaces`:

```yaml
allowedNamespaces:
  names:                 # explicit list
    - ci-foo
  selector:              # metav1.LabelSelector on Namespace labels
    matchLabels:
      registrygate.soliddowant.dev/tenant: "true"
```

A namespace is admitted when it is the object's own namespace, is in `names`, or matches `selector`. A reference that isn't admitted is treated as absent and reported as `NotAllowed`. Every Secret reference is to a Secret in the referring object's own namespace.

| Kind | Who writes it, where | Spec |
|---|---|---|
| `UpstreamRegistry` | admin, **admin namespaces only** | `hosts` (DNS names the proxy answers for), `url` (where requests go), `caSecretRef` (optional, to trust the upstream), `tls.secretRef` (serving certificate: `tls.crt`, `tls.key`; its SANs must cover every host), `tokenRealmHosts` (optional; hosts allowed as the token endpoint besides the `url` host), `anonymousPulls` (`Challenge`, the default, or `Forward`; see "Resolution"), `defaultMirrorRef` (optional), `allowedMirrorURLs` (URL prefixes a mirror of this upstream may use), `timeouts`, `allowedNamespaces` (who may point credentials, mirrors and policies at it) |
| `UpstreamCredential` | admin or tenant, any namespace | `upstreamRef`, `secretRef` (`name`; keys `username` and `password` by default, or `usernameKey` / `passwordKey`), `ceiling` (list of `{repositories, actions}`: the most any policy may do with it), `allowedNamespaces` |
| `RegistryMirror` | admin or tenant, any namespace | `upstreamRef`, `url`, `pathPrefix`, `secretRef` (optional, same key rules), `caSecretRef` (optional), `fallbackToUpstream` (default `true`), `timeouts`, `allowedNamespaces` |
| `RegistryAccessPolicy` | tenant, its own namespace | `serviceAccounts` (in this namespace only), `rules` (below) |

**Admin namespaces** are set by the proxy flag `--admin-namespaces` (default: the proxy's own namespace). The proxy reads `UpstreamRegistry` objects only from these, because an `UpstreamRegistry` decides which hosts the proxy impersonates for everyone. The admission webhook rejects one created elsewhere.

**Hosts.** Each host belongs to at most one `UpstreamRegistry`. When two claim the same host, both are `Ready=False, reason=HostConflict` for it and the host isn't served. A TLS handshake for a server name that no ready `UpstreamRegistry` claims fails; there is no default certificate.

**Rules.** `rules` is a list keyed by `name` (`+listType=map`; names unique within the policy). Each rule has exactly one of:
- `credentialRef` with `repositories` and `actions` (any of `pull`, `push`, `delete`): requests go to the credential's upstream with that credential;
- `mirrorRef` with `repositories` and `actions: [pull]`: pulls go to that mirror with the mirror's own credential.

Repository patterns are globs: `*` matches within one path segment, `**` across segments. A rule's upstream is the one its credential or mirror points at. A credential rule is **accepted** only if every repository pattern for every action is contained in the credential's ceiling for that action (the existing containment check; when in doubt, not contained). Rules have no order.

**Status.** Every kind has `observedGeneration` and a `Ready` condition with a reason (`Ready`, `InvalidSpec`, `NotAllowed`, `RefNotFound`, `RefNotReady`, `SecretUnavailable`, `SecretInvalid`, `HostConflict`, `CertificateMismatch`, `URLNotAllowed`, `RulesNotAccepted`). `RegistryAccessPolicy` status also has `rules[]`, keyed by `name`, each with conditions: `Accepted` (`True`, or `False` with `OutsideCeiling`, `NotAllowed`, `RefNotFound`, `RefNotReady`). An unaccepted rule is ignored; the rest of the policy still applies.

### Secrets

- The proxy watches every Secret that an object references, and applies a change without a restart: cached upstream tokens for that credential are dropped (the token cache key includes the Secret's `resourceVersion`), mirror clients are rebuilt, serving certificates and CAs are reloaded.
- **Admin namespaces:** the chart gives the proxy `get`, `list` and `watch` on Secrets there.
- **Tenant namespaces:** the tenant (or its chart) creates a Role with `resourceNames: [<secret>]` and verbs `get`, `list`, `watch`, bound to the proxy's ServiceAccount. The proxy watches each such Secret by name (field selector `metadata.name`). It never needs cluster-wide read access to Secrets.
- A Secret that is missing, unreadable or lacks its keys makes the referring object `Ready=False`, and requests that need it fail closed (see "Resolution").

### Authentication

- The proxy answers unauthenticated `/v2/` requests with `401` and `WWW-Authenticate: Basic realm="registrygate"`, so Docker sends HTTP Basic credentials. The username is ignored; the password is a Kubernetes ServiceAccount token.
- Tokens are checked with the `TokenReview` API, with `audiences: [<configured audience>]` (default `registrygate`). The audience must be in the result. Definite results are cached for at most 30 seconds, keyed by a SHA-256 of the token; the cache never stores the token.
- The identity is the ServiceAccount's namespace and name, plus the bound pod name if the review reports one (audit only).
- A request with no `Authorization` header is **anonymous**. A Basic credential that fails review gets `401 UNAUTHORIZED`; it is never downgraded to anonymous. A `Bearer` credential is rejected with `401`.

### Requests

**Paths.** Before anything else, a request whose path isn't in canonical form gets `400` and nothing is sent anywhere: `.` or `..` segments, empty segments, percent-encoded `/` or unreserved characters, or a first segment that is `v2` in another case or encoding. Requests outside `/v2` get `404`.

**Actions.** The proxy derives the action from the request:

| Request | Action |
|---|---|
| `GET`/`HEAD` on `/v2/<name>/manifests/<ref>`, `/blobs/<digest>`, `/tags/list`, `/referrers/<digest>` | `pull` on `<name>` |
| `POST /v2/<name>/blobs/uploads/`, requests on an upload session (below), `PUT` on `/v2/<name>/manifests/<ref>` | `push` on `<name>` |
| `POST /v2/<name>/blobs/uploads/?mount=<digest>&from=<other>` | `push` on `<name>` **and** `pull` on `<other>` |
| `DELETE` on `/v2/<name>/manifests/<ref>` or `/blobs/<digest>` | `delete` on `<name>` |
| `GET /v2/` | none (version check) |
| anything else (`/v2/_catalog`, unknown paths) | `404` |

Repository names are parsed with the distribution reference grammar and compared lower-cased.

**Upload sessions.** The upload `Location` an upstream returns is opaque and differs between registries (GHCR uses `/v2/<name>/blobs/upload/<session>`). The proxy gives the client its own session URL, `/v2/<name>/blobs/uploads/<token>`, where `<token>` encodes the upstream's `Location` path and query and is signed with HMAC-SHA-256 over the upstream, `<name>` and that path, with a key from a Secret all replicas share. When a request comes in on such a URL, the proxy checks the signature and refuses it (`404 BLOB_UPLOAD_UNKNOWN`) unless it matches and the upstream path stays under `/v2/<name>/blobs/` for the same `<name>`. The session is checked against policy as `push` on `<name>`.

### Resolution

For a request to host `H`, repository `R` and action `A`:

1. The upstream `U` is the ready `UpstreamRegistry` that claims `H`.
2. If the caller is a ServiceAccount, the **candidates** are the accepted rules, in policies in the caller's namespace that name its ServiceAccount, whose upstream is `U`, whose repositories match `R`, and whose actions include `A`. A credential rule is a candidate only if its credential is ready. A mirror rule whose mirror isn't ready is still a candidate, and that mirror counts as failed (see "Mirrors"), so a tenant's `fallbackToUpstream: false` is kept.
3. **Precedence:** for a pull, credential rules win over mirror rules (private content needs the credential; mirrors are for public content).
4. **Ambiguity:** if the remaining candidates name more than one distinct credential, or more than one distinct mirror, the request is denied: `403 DENIED` with a message naming the policies and rules. Candidates that name the same credential or mirror simply combine.
5. **One candidate target:**
   - credential: forwarded to `U` with that credential (pulls too: credentialed pulls never go to a mirror);
   - mirror: a pull of a manifest or blob goes to the mirror; a tag list or referrers request goes to `U` anonymously.
6. **No candidate:**
   - an anonymous request (no `Authorization`) to an upstream with `anonymousPulls: Challenge` (the default): `401` with the Basic challenge, for pulls too. BuildKit and containerd try anonymously first, so this is what makes them send their ServiceAccount token, and with it lets per-namespace mirrors see their pulls;
   - pull (authenticated, or anonymous with `anonymousPulls: Forward`): to `U`'s `defaultMirrorRef` if it is set and ready, else to `U` anonymously;
   - push or delete by a ServiceAccount: `403 DENIED` naming the namespace, ServiceAccount, repository and action; nothing is sent;
   - push or delete by an anonymous caller: `401` with the Basic challenge, so Docker retries with its credential.
7. **Mounts** use the credential chosen for `push` on `<name>`. If `pull` on `<other>` doesn't resolve to that same credential, or the parameters are malformed or repeated, `mount` and `from` are dropped and the upstream answers with an ordinary upload session.

A credential or mirror that becomes not ready while serving (Secret removed, admission withdrawn) stops being used on the next request.

### Forwarding

- **Upstream auth.** The proxy pings the upstream's `/v2/` and follows its challenge: none, Basic (sends the credential), or Bearer (gets a token from the token endpoint, with the credential as Basic auth, or anonymously). It uses `github.com/google/go-containerregistry/pkg/v1/remote/transport` only for the ping and token exchange, sends tokens with its own round tripper (never widening scopes or resending), and caches one token per (credential, Secret `resourceVersion`, scope set) until it expires, or 60 seconds when the endpoint gives no lifetime. A token endpoint on a host other than the `url` host or `tokenRealmHosts` is refused (`502`).
- **Anonymous pulls** use the same flow with no credential. When the upstream refuses one, the client gets the proxy's own `401` Basic challenge, so clients that try anonymously first (BuildKit, containerd) retry with their credential.
- **Upstream `401`** on a request with a credential is retried once with a fresh token when the request can be replayed, then returned as `502` with a clear error: the credential is wrong or lacks a scope.
- **Delete** is the OCI `DELETE` on manifests (by digest, or by tag where the registry supports it) and blobs, forwarded with the credential. The upstream's answer, including `405 UNSUPPORTED` (GHCR answers that to every delete), is passed through. There are no registry-specific delete paths.
- The upstream path and query are rebuilt from what the proxy parsed and resolved; nothing from the client's URL is forwarded verbatim.
- Request and response bodies are streamed, never buffered whole. Uploads of several GB must work.

### Mirrors

- A mirror pull goes to `<mirror url>/v2/<pathPrefix>/<R>/…` with the mirror's own credential (the same challenge handling as upstreams).
- A `RegistryMirror` whose `url` doesn't start with one of its upstream's `allowedMirrorURLs` is `Ready=False, reason=URLNotAllowed` and is never contacted. An empty list allows no mirrors.
- When a mirror fails (connection error, no response headers within its timeout, `5xx`, or `404`; a blob only before response headers) and `fallbackToUpstream` is true, the pull goes to `U` anonymously; otherwise a `404` is returned as is and any other failure gives `502 "mirror unavailable"`. A `401` or `403` from the mirror is a mirror credential problem: `502 "mirror unavailable"`, never a fallback.
- A blob `HEAD` always goes to `U` anonymously.

### Rewrites

- `Location` headers that point at the upstream or mirror host are rewritten to the host the client used. Upload sessions are rewritten as above.
- `Location` headers that point anywhere else (blob storage, signed URLs) are passed through untouched, so blob downloads go straight to storage.
- `WWW-Authenticate` challenges from the upstream or a mirror are never passed to the client.

### Audit

One JSON log line per request to stdout: time, client IP, host, upstream, namespace, ServiceAccount, pod, repository, action, decision (`allowed`, `mirror`, `anonymous`, `denied`, `ambiguous`, `unauthenticated`, `unavailable`, `notfound`, `invalid`), the policy and rule, the credential or mirror used, upstream or mirror status, bytes, duration, and for manifest pushes and deletes the digest. Never any header values, credentials or tokens.

### Metrics

Request counts by upstream, action, decision and status class; upstream and mirror latency histograms; token review results and cache hits; mirror fallbacks. The only object-name label is `upstream` (`<namespace>/<name>` of the `UpstreamRegistry`; admins create few). No repository, namespace, ServiceAccount, credential or mirror labels.

### Admission webhook

Optional, served by the proxy (`--webhook`). It rejects:
- an `UpstreamRegistry` outside the admin namespaces;
- a rule with both or neither of `credentialRef` and `mirrorRef`, or a mirror rule with an action other than `pull`;
- a credential rule outside its credential's ceiling (message names the patterns).

References that don't exist yet are allowed (they may be created later) and show in status.

### Credential helper

`docker-credential-registrygate` implements the Docker credential helper protocol, plus one subcommand:
- `get`: for a server whose host is in `REGISTRYGATE_HOSTS` (comma-separated; with or without scheme or path), reads the token file (`REGISTRYGATE_TOKEN_FILE`, default `/var/run/secrets/registrygate/token`) fresh on every call and prints `{"ServerURL":…,"Username":"registrygate","Secret":<token>}`. Any other server: the protocol's "credentials not found" answer.
- `store`, `erase`: succeed and do nothing. `list`: `{}`.
- `configure`: merges `"credHelpers": {"<host>": "registrygate"}` for each host in `REGISTRYGATE_HOSTS` into `$DOCKER_CONFIG/config.json` (default `~/.docker`), creating it if needed and keeping every other key. `DOCKER_CONFIG` must be writable (buildx keeps state there), so it is never a ConfigMap mount.

It is a static Go binary (`CGO_ENABLED=0`) shipped alone in the image `ghcr.io/soliddowant/registrygate-credential-helper` (`FROM scratch`, binary at `/docker-credential-registrygate`), for amd64 and arm64.

## 5. Security model

- **RBAC:** create `TokenReview`; get, list and watch the four kinds and Namespaces (for `allowedNamespaces.selector`) cluster-wide; update their status; Secrets as in "Secrets"; leases for leader election. Nothing else: no pods, no cluster-wide Secrets.
- **Mirrors are public-content only.** A Harbor proxy-cache project fetches from its upstream with its registry endpoint's credential. If that credential can read private images, anyone holding the project's robot can read them through Harbor directly, bypassing the proxy's policy. So a mirror's Harbor registry endpoint must have **no upstream credential**. The proxy can't check this; the admin docs say so prominently. Private content is pulled with an `UpstreamCredential`, which never uses a mirror.
- **Tenant-written objects** can only affect their own namespace's requests: a tenant's credential or mirror is used only by policies in namespaces it admits, and its Secret goes only to its own endpoint. A tenant mirror's URL is limited by the upstream's `allowedMirrorURLs`. The proxy's NetworkPolicy should also limit its egress to the upstreams and mirrors.
- **ServiceAccount tokens** for the proxy use their own audience (`registrygate`) and a short lifetime (the docs recommend 1–6 hours).
- **Serving CAs** are the admin's. The docs require one CA per `UpstreamRegistry`, name-constrained to exactly its hosts, trusted only through Docker's per-registry `certs.d` directory, never the system trust store.
- **Shared credentials** (for example one GHCR PAT behind many `UpstreamCredential` objects, each with its own ceiling) share one upstream identity and its rate limits. The audit log is how the owner tells who did what.

## 6. Won't do

- Caching or storing image data in the proxy (mirrors do that).
- Pulling private content through a mirror.
- Passing through anything outside the registry API (`/v2`): a project that needs a registry's own API doesn't intercept that host.
- Registry-specific APIs (the GitHub packages REST API, Harbor's API): only the standard registry protocol.
- Identifying callers by anything but their ServiceAccount token (no source-IP identity).
- Cluster-scoped kinds, deny rules, rule ordering, or policies that name ServiceAccounts in other namespaces.
- Minting or managing upstream credentials, robot accounts or PATs; issuing or rotating TLS certificates (the admin uses cert-manager).
- Node-level (containerd/CRI) image pulls.
- `/v2/_catalog`.
- Migrating `GHCRAccessPolicy` objects or the 0.2.x config file: 0.2.x was never deployed.
- Hand-written cryptography or registry protocol code where a maintained library exists (see `CLAUDE.md`).
- Formal verification, or fuzz or simulation runs that define done.

## 7. Phases

Each phase ends with a release an admin can deploy. The owner deploys it in their own time; deploying never blocks a phase.

1. **Done (0.1.x, as registrygate).** Push and pull through the proxy for `ghcr.io`.
2. **Done (0.2.0, as registrygate).** Delete, mirrors and operations.
3. **registrygate** (`docs/acceptance/phase-3.md`). The rename, the four custom resources, several upstreams by SNI, upstream auth by challenge, credentials with ceilings, per-namespace mirrors with tenant-owned Secrets, Secret watching, generic upload sessions and delete, path canonicalization, the credential helper's host list and `configure`, status, webhook, metrics, audit, chart and docs. Release 0.3.0: images `ghcr.io/soliddowant/registrygate:<version>` and `ghcr.io/soliddowant/registrygate-credential-helper:<version>`, chart `oci://ghcr.io/soliddowant/charts/registrygate`.

Each phase's design (`docs/design/phase-N.md`) is reviewed before its issues are scheduled.

## 8. Testing

- **Unit:** Go `testing` + `testify`, table-driven. Path canonicalization and classification, glob matching and containment, resolution (candidates, precedence, ambiguity), upload session tokens, rewrites and the credential helper are unit-tested.
- **Fake registries** (`test/fakeregistry`): in-process registries built from `github.com/google/go-containerregistry/pkg/registry`, with options for the behaviours that matter: Bearer challenge with a token endpoint that checks credentials and enforces scopes, or Basic, or anonymous; absolute upload `Location`s with a configurable path shape (GHCR's singular `blobs/upload/`); `307` redirects for blobs to a separate storage host; public and private repositories; `DELETE` supported or answered `405`. The fake GHCR is one configuration of it. They are test doubles, not product code.
- **Integration** (`//go:build integration`): envtest plus the proxy and fake registries in-process; real `TokenReview` from envtest's API server.
- **e2e** (`//go:build e2e`): kind with `ImageVolume` on, the proxy from the chart with a locally built image, two fake registries and Harbor in the cluster, and pods set up exactly as the admin guide says, running real `docker build`, `docker push` and `docker pull`.
- **Live** (`//go:build live`): the same client flows against the real `ghcr.io`. **Never in CI.** Only the operator runs `make test-live` by hand, with `GHCR_LIVE_PAT_FILE`; agents never get the token. The tests touch only packages they create.
- **Acceptance:** each checklist scenario lists its tests on a `Tests:` line. `make acceptance` prints passing scenarios out of the total.
- Tests never depend on wall-clock speed. Wait on conditions with deadlines. Timing-sensitive tests pass `-count=50 -race`.

## 9. Repository layout

```
cmd/registrygate/                     proxy main
cmd/docker-credential-registrygate/   credential helper main
api/v1alpha1/                          the four kinds (controller-gen)
internal/server/                       TLS listener and SNI, path canonicalization, health and metrics endpoints
internal/authn/                        TokenReview with cache
internal/authz/                        object index, admission, rule acceptance, resolution, glob matching and containment
internal/registry/                     request classification, forwarding, upload sessions, rewrites, upstream transports
internal/mirror/                       mirror clients and fallback
internal/secrets/                      Secret watching
internal/controller/                   status controllers
internal/webhook/                      admission webhook
internal/audit/, internal/metrics/
internal/credhelper/                   credential helper logic
config/crd/bases/, config/rbac/        generated
deploy/charts/registrygate            Helm chart
test/fakeregistry/, test/e2e/,         fake registries, e2e harness and fixtures,
test/live/                             live tests against ghcr.io
docs/                                  user/ admin/ dev/ agents/ design/ acceptance/
scripts/                               dev and agent scripts
```

## 10. Documentation plan

| Audience | Knows | Needs | Pages |
|---|---|---|---|
| Chart authors (users) | Kubernetes YAML, Helm, Docker | How to write a `RegistryAccessPolicy`, set up a project's own mirror and the Role for its Secret, read status, and why a request is denied or ambiguous | `docs/user/` |
| Admins | Kubernetes, Helm, Flux, cert-manager, Tekton, Harbor | Install, `UpstreamRegistry` and its certificate and CA, shared credentials with ceilings, mirrors (public content only), setting up a CI pod (hostAliases, certs.d, helper, `configure`, token), projects that don't intercept, security limits, troubleshooting | `docs/admin/` (CRD and chart value references generated) |
| Developers | Go, controller-runtime, the registry API | Architecture, packages, the fake registries, test tiers, releasing | `docs/dev/` |
| Agents | This repo | Playbooks | `docs/agents/`, `.claude/`, `CLAUDE.md` |

## 11. Open questions

1. Do `quay.io` and `mirror.gcr.io` answer the anonymous token flow as expected, and where do they redirect blobs? Checked by hand by the operator during phase 3; recorded in `docs/dev/`.
