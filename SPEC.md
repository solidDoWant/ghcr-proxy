# ghcr-proxy specification

ghcr-proxy is an authenticating reverse proxy for `ghcr.io`. It runs in a Kubernetes cluster, holds one GitHub classic personal access token (PAT), and lets CI pods push, pull and delete GHCR images without ever seeing that token. Pods authenticate with their own Kubernetes ServiceAccount token. What each ServiceAccount may do is declared in `GHCRAccessPolicy` custom resources, capped per namespace by the proxy's own configuration.

This file is the authoritative reference. Each phase is done when its checklist in `docs/acceptance/` passes (see "Phases").

## 1. Purpose and reference behaviour

**The owner's goal.** GitHub has no API for minting GHCR credentials: GHCR still needs a classic PAT, and classic PATs can't be created, scoped or expired by a program. The owner runs self-hosted CI (Tekton, one `ci-<repository>` namespace per repository, builds with the Docker CLI against a dind sidecar in a kata VM). They want each repository's builds to get short-lived credentials that reach only that repository's images, with no PAT handed out.

**How it is used.** CI pods are set up to resolve `ghcr.io` to the proxy (pod `hostAliases`) and to trust the proxy's certificate for `ghcr.io` only (Docker's `/etc/docker/certs.d/ghcr.io/ca.crt`). The cluster admin issues that certificate from a private CA limited to `ghcr.io` by name constraints; ghcr-proxy only serves it. The Docker CLI gets its credential from `docker-credential-ghcr-proxy`, a static credential helper shipped as its own image and mounted into CI pods as an image volume. The helper returns the pod's projected ServiceAccount token. Builds keep using plain `ghcr.io/<owner>/<image>` references: nothing in a Dockerfile or pipeline changes.

**Reference behaviour.** Where this spec doesn't decide a behaviour, decide it in this order:
1. What `ghcr.io` itself does for the same request, so a client can't tell the proxy apart from GHCR except for what policy denies. The proxy follows the OCI Distribution Specification v1.1 and the Docker Registry HTTP API v2 as GHCR implements them.
2. What well-behaved registry front ends do (Harbor's proxy, distribution's proxy mode, Zot): error bodies in the registry JSON error format, standard status codes.
3. The simplest behaviour that keeps the principles.

Record the choice on the issue.

**Users:**
- **Admins** install the proxy with Helm, give it the PAT, the TLS certificate and the namespace grants, and set up CI pods (hostAliases, CA, helper, projected token).
- **Chart authors** (the owner's CI chart, `cluster/charts/ci/project` in the owner's infra repo) create a `GHCRAccessPolicy` in each CI namespace.
- **The owner** is both.

## 2. Principles

Non-negotiable. Tests enforce them, and only the owner can change them.

1. **The PAT never leaves the proxy.** It is read from a Secret and sent only to `ghcr.io`'s token endpoint (or the configured upstream). It is never logged, never returned to a client, never put in status, events, metrics or errors, and never sent to a mirror.
2. **A caller gets no more than its policy, and a policy no more than its grant.** A request uses the PAT only when a `GHCRAccessPolicy` in the caller's own namespace names the caller's ServiceAccount and allows that action on that repository, and the proxy's grant for that namespace covers it. Everything else is either forwarded with no credential (pulls) or denied.
3. **Fail closed.** If the proxy can't decide (token review unavailable, policies not yet synced, config invalid) it denies or forwards anonymously; it never falls back to the PAT.
4. **Transparent to Docker.** A Docker client set up as in "How it is used" can push, pull and delete exactly as against `ghcr.io`, with no changes to image references, Dockerfiles or commands.
5. **Done means the owner's checklist passes.** Nobody raises the bar beyond the checklist and principles 1–4.

## 3. Target environment

- Kubernetes 1.33 or later with the `ImageVolume` feature on (needed only for mounting the credential helper). The `TokenReview` API must be reachable from the proxy.
- Clients: Docker CLI and daemon 25 or later (including BuildKit through `docker build` and `docker buildx`). Other OCI clients (crane, oras, skopeo, containerd) should work but aren't tested.
- Upstream: `ghcr.io`. The upstream URL is configurable so tests can use a fake.
- Optional pull mirrors: any registry that serves GHCR's content under a path prefix, for example a Harbor proxy-cache project for `ghcr.io` (phase 2).

## 4. Architecture

One Go binary, `ghcr-proxy`, plus the credential helper binary `docker-credential-ghcr-proxy`.

The proxy runs several stateless replicas behind a Service. Each replica:

1. **Serves TLS** on port 8443 with the certificate and key from a mounted Secret (`tls.crt`, `tls.key`), reloaded when the files change. Also serves `/healthz`, `/readyz` and `/metrics` on a separate plain-HTTP port.
2. **Authenticates** each request (see "Authentication").
3. **Authorizes** it against an in-memory view of every `GHCRAccessPolicy`, kept by a controller-runtime informer, and the static grants from its config file (see "Authorization").
4. **Forwards** it upstream with the right credential (see "Forwarding"), and rewrites what the client must see (see "Rewrites").
5. **Writes one audit log line** per request (see "Audit").

A leader-elected controller in the same binary writes `GHCRAccessPolicy` status (which rules are inside the grant). Serving doesn't need the leader: every replica authorizes from its own informer cache. A replica isn't ready until its cache has synced.

### Authentication

- The proxy answers unauthenticated `/v2/` requests with `401` and `WWW-Authenticate: Basic realm="ghcr-proxy"`, so Docker sends HTTP Basic credentials on every request. The username is ignored; the password is a Kubernetes ServiceAccount token.
- Tokens are checked with the `TokenReview` API, with `audiences: [<configured audience>]` (default `ghcr-proxy`). The audience must be in the review's result. This also rejects tokens of deleted pods and ServiceAccounts. Definite results (authenticated or rejected, never errors) are cached for at most 30 seconds, keyed by a SHA-256 of the token; the cache never stores the token itself.
- The identity is the ServiceAccount's namespace and name (`system:serviceaccount:<ns>:<name>`), plus the bound pod name if the review reports one (audit only).
- A request with no `Authorization` header is **anonymous**. A request with a Basic credential that fails review gets `401 UNAUTHORIZED`: it is never downgraded to anonymous, so a broken setup is visible.
- A `Bearer` credential is rejected with `401`; the proxy never issues or accepts bearer tokens of its own (it needs none, see "Won't do").

### Authorization

**Actions.** `pull`, `push`, `delete`. The proxy derives the action from the request, never from anything the client claims:

| Request | Action |
|---|---|
| `GET`/`HEAD` on `/v2/<name>/manifests/<ref>`, `/blobs/<digest>`, `/tags/list`, `/referrers/<digest>` | `pull` on `<name>` |
| `POST`/`PATCH`/`PUT` on `/v2/<name>/blobs/uploads/…`, `PUT` on `/v2/<name>/manifests/<ref>` | `push` on `<name>` |
| `POST /v2/<name>/blobs/uploads/?mount=<digest>&from=<other>` | `push` on `<name>` **and** `pull` on `<other>` |
| `DELETE` on `/v2/<name>/manifests/<ref>` or `/blobs/<digest>` | `delete` on `<name>` |
| `GET /v2/` | none (version check) |
| anything else (`/v2/_catalog`, unknown paths) | denied with `404` |

Repository names are parsed with the distribution reference grammar (multi-segment names allowed) and compared lower-cased, as GHCR does.

**Policies.** `GHCRAccessPolicy` (API group `ghcr-proxy.soliddowant.dev`, version `v1alpha1`, namespaced):

```yaml
apiVersion: ghcr-proxy.soliddowant.dev/v1alpha1
kind: GHCRAccessPolicy
metadata:
  namespace: ci-foo
  name: builds
spec:
  serviceAccounts:          # in this namespace only
    - default
  rules:
    - repositories:          # glob: `*` matches within one path segment, `**` across segments
        - soliddowant/foo
        - soliddowant/foo/**
      actions:
        - pull
        - push
status:
  observedGeneration: 1
  conditions:
    - type: Ready           # False with reason OutsideGrant when any repository pattern is outside the grant
  rejected:                 # the repository patterns ignored, per rule
    - rule: 0
      repositories: []
```

A request is allowed when **some** policy in the caller's namespace lists the caller's ServiceAccount and has a rule that matches the repository and the action, **and** the namespace's grant covers that repository and action. Policies only ever add access; there are no deny rules.

**Grants.** The proxy's config file (rendered from chart values) lists which namespaces may grant what. A namespace that matches no grant can grant nothing.

```yaml
grants:
  - namespaces: "ci-(?P<repo>[a-z0-9-]+)"   # RE2, matched against the whole namespace name
    repositories:                            # globs; ${name} expands to a named capture
      - soliddowant/${repo}
      - soliddowant/${repo}/**
    actions: [pull, push, delete]
```

A policy repository pattern is inside a grant only if every name it can match is matched by one of the grant's patterns (pattern containment is decided segment by segment on the glob syntax; when in doubt, the pattern is rejected). Rejected patterns are dropped from the policy, listed in `status.rejected`, and set `Ready=False, reason=OutsideGrant`. The rest of the policy still applies.

**Who may write policies** is decided by Kubernetes RBAC, outside the proxy. The grant is the ceiling a namespace can't exceed even if it can write any policy in its own namespace. Status is written only by the proxy.

### Forwarding

- **Allowed by policy:** forwarded to the upstream with a bearer token the proxy gets from the upstream's token endpoint using the PAT, scoped to exactly the repositories and actions this request needs. The proxy uses `github.com/google/go-containerregistry/pkg/v1/remote/transport` only for the token exchange, sends tokens with its own round tripper (never widening scopes or resending), and caches one token per credential kind and scope set until it expires.
- **Pull not allowed by policy, or anonymous pull:** forwarded with **no credential** (an anonymous token, never the PAT), so public images work. When GHCR refuses it, the client gets the proxy's own `401` Basic challenge, so clients that try anonymously first (BuildKit, containerd) retry with their credential.
- **Push or delete not allowed:** for a ServiceAccount, `403` with registry error code `DENIED` and a message naming the namespace, ServiceAccount, repository and action; for an anonymous request, `401` with the Basic challenge, so Docker retries with its credential. Nothing is sent upstream.
- **Mount (`?mount=&from=`) where `from` isn't pullable by policy, or the parameters are malformed or repeated:** the `mount` and `from` parameters are dropped, so the registry answers with an ordinary upload session (the client then uploads the blob, as the spec allows).
- Request and response bodies are streamed, never buffered whole. Client and upstream timeouts are configurable; uploads of several GB must work.

### Rewrites

- `Location` headers that point at the upstream host are rewritten to the host the client used. Upload session URLs keep their path and query.
- `Location` headers that point anywhere else (GHCR's blob storage, signed URLs) are passed through untouched, so blob downloads go straight to storage.
- `WWW-Authenticate` challenges from the upstream are never passed to the client; the client only ever sees the proxy's Basic challenge.
- Upstream `401`s on a request the proxy authorized are retried once with a fresh token (when the request can be replayed), then returned as `502` with a clear error: they mean the PAT is wrong or lacks a scope.
- The upstream path and query are rebuilt from what the proxy parsed and authorized; nothing from the client's URL is forwarded verbatim.

### Delete

GHCR's support for the OCI `DELETE` endpoints is not documented. Phase 2 verifies it against the real `ghcr.io`. If GHCR refuses `DELETE /v2/<name>/manifests/<digest>`, the proxy implements it through the GitHub REST API (`DELETE /users/{user}/packages/container/{package}/versions/{id}` or the `orgs` equivalent), finding the version whose name is the digest. Deleting by tag is passed through as is; if GHCR refuses it, the client gets GHCR's error.

### Pull mirrors (phase 2)

The config may name a **policy mirror** and an **anonymous mirror**, each `{url, pathPrefix, credentialsFile, fallbackToUpstream}`. Allowed pulls of manifests and blobs go to the policy mirror; anonymous and not-allowed pulls go to the anonymous mirror. Tag lists, referrers, pushes, deletes and mounts always go to the upstream. The PAT is never sent to a mirror; a mirror uses its own credentials. When a mirror fails (connection error, `5xx`, or a `404` for a manifest) and `fallbackToUpstream` is true (default), the request goes to the upstream as above.

Admins must configure the anonymous mirror with **no upstream credentials**; otherwise anonymous callers could read private images through it. The proxy can't check this; the admin docs say so prominently.

### Audit

One JSON log line per request to stdout: time, client IP, namespace, ServiceAccount, pod, repository, action, decision (`allowed`, `anonymous`, `denied`, `unauthenticated`), the policy that allowed it, upstream status, bytes, duration, and for manifest pushes and deletes the digest. Never any header values or tokens.

### Credential helper

`docker-credential-ghcr-proxy` implements the Docker credential helper protocol:
- `get`: for server `ghcr.io` (with or without scheme or path), reads the token file (`GHCR_PROXY_TOKEN_FILE`, default `/var/run/secrets/ghcr-proxy/token`) fresh on every call and prints `{"ServerURL":…,"Username":"ghcr-proxy","Secret":<token>}`. Any other server: the protocol's "credentials not found" answer.
- `store`, `erase`: succeed and do nothing.
- `list`: `{}`.

It is a static Go binary (`CGO_ENABLED=0`) shipped alone in the image `ghcr.io/soliddowant/ghcr-proxy-credential-helper` (`FROM scratch`, binary at `/docker-credential-ghcr-proxy`), for amd64 and arm64.

## 5. Security model

- The proxy's Kubernetes RBAC: create `TokenReview`; get, list and watch `GHCRAccessPolicy`; update `GHCRAccessPolicy/status`; leases for leader election. Nothing else (no events); it reads the PAT and TLS Secrets as mounted files.
- A caller's reach is the intersection of its namespace's policies and its namespace's grant. A caller in a namespace with no grant gets only anonymous pulls.
- ServiceAccount tokens for the proxy should use their own audience (`ghcr-proxy`) and a short lifetime (the docs recommend 1–6 hours; the kubelet rotates them). A token with the default API server audience is rejected unless the admin configures that audience, which the docs advise against.
- The CA that signs the proxy's `ghcr.io` certificate is the admin's. The docs require name constraints limiting it to `ghcr.io` and trusting it only through Docker's per-registry `certs.d` directory, never the system trust store.
- The PAT should belong to a dedicated machine-user account with only `read:packages`, `write:packages` and `delete:packages`.
- All callers share one GitHub identity and its rate limits. The audit log is how the owner tells who did what.

## 6. Won't do

- Minting or managing GitHub PATs, GitHub App tokens, or any GitHub-side setup.
- Issuing credentials of its own (bearer tokens, robot accounts). Kubernetes ServiceAccount tokens are the only client credential.
- Proxying registries other than `ghcr.io`, or more than one upstream.
- Pull-through caching inside the proxy (use a mirror, phase 2), or storing any image data.
- Deny rules, cluster-scoped policies, or policies that name ServiceAccounts in other namespaces.
- `/v2/_catalog`, the GitHub packages REST API beyond the one delete call above, package visibility or settings.
- Untagging a tag without deleting the version it points to, when GHCR itself doesn't support it.
- Issuing or rotating the TLS certificate (the admin uses cert-manager or similar).
- Hand-written cryptography or registry protocol code where a maintained library exists (see `CLAUDE.md`).
- Formal verification, or fuzz or simulation runs that define done.

## 7. Phases

Each phase ends with a release an admin can deploy: images `ghcr.io/soliddowant/ghcr-proxy:<version>` and `ghcr.io/soliddowant/ghcr-proxy-credential-helper:<version>`, chart `oci://ghcr.io/soliddowant/charts/ghcr-proxy`. The owner deploys it in their own time; deploying never blocks a phase.

1. **Push and pull through the proxy** (`docs/acceptance/phase-1.md`). TLS serving, ServiceAccount authentication, `GHCRAccessPolicy` with grants and status, forwarding and rewrites, anonymous pulls, audit log, credential helper, images, chart, and the admin guide for setting up a CI pod. This is everything the owner's CI needs.
2. **Delete, mirrors and operations** (`docs/acceptance/phase-2.md`). Delete verified against real GHCR (with the REST fallback if needed), policy and anonymous pull mirrors, metrics, an optional validating admission webhook served by the proxy (the same containment code) that rejects policies outside the grant at apply time, PodDisruptionBudget and NetworkPolicy options.

Each phase's design (`docs/design/phase-N.md`) is reviewed before its issues are scheduled.

## 8. Testing

- **Unit:** Go `testing` + `testify`, table-driven. Path parsing and action mapping, glob matching and containment, grant expansion, rewrites and the credential helper are unit-tested.
- **Fake GHCR** (`test/fakeghcr`): an in-process registry built from `github.com/google/go-containerregistry/pkg/registry`, wrapped to behave like GHCR where it matters: bearer-token challenge with a `/token` endpoint that checks a PAT and enforces scopes, absolute `Location` URLs for uploads, `307` redirects for blob downloads to a separate storage host, public and private repositories. It is a test double, not product code.
- **Integration** (`//go:build integration`): envtest plus the proxy and the fake GHCR in-process; real `TokenReview` from envtest's API server.
- **e2e** (`//go:build e2e`): kind with `ImageVolume` on, the proxy installed from the chart with a locally built image, the fake GHCR in the cluster, and a pod set up exactly as the admin guide says (hostAliases, `certs.d` CA, helper image volume, projected token, dind) running real `docker build`, `docker push` and `docker pull`.
- **Live** (`//go:build live`): the same client flows against the real `ghcr.io`. **Never in CI.** The only PAT available belongs to the owner's main GitHub account, so the operator runs `make test-live` by hand outside CI, with `GHCR_LIVE_PAT_FILE` pointing at the token file; agents never get the token. Without the variable the tests skip. The tests touch only packages they create themselves (section 1 of `docs/design/phase-2.md`). Phase 2 uses them to settle the delete question.
- **Acceptance:** each checklist scenario lists its tests on a `Tests:` line. `make acceptance` prints passing scenarios out of the total.
- Tests never depend on wall-clock speed. Wait on conditions with deadlines. Timing-sensitive tests pass `-count=50 -race`.

## 9. Repository layout

```
cmd/ghcr-proxy/                     proxy main
cmd/docker-credential-ghcr-proxy/   credential helper main
api/v1alpha1/                       GHCRAccessPolicy Go types (controller-gen)
internal/server/                    TLS listener, routing, health and metrics endpoints
internal/authn/                     TokenReview with cache
internal/authz/                     policy index, grants, glob matching and containment
internal/registry/                  request classification (path, action), forwarding, rewrites, upstream transports
internal/controller/                GHCRAccessPolicy status controller
internal/audit/                     audit log lines
internal/credhelper/                credential helper logic
config/crd/bases/, config/rbac/     generated
deploy/charts/ghcr-proxy            Helm chart
test/fakeghcr/, test/e2e/           fake GHCR, e2e harness and fixtures
docs/                               user/ admin/ dev/ agents/ design/ acceptance/
scripts/                            dev and agent scripts
```

## 10. Documentation plan

| Audience | Knows | Needs | Pages |
|---|---|---|---|
| Chart authors (users) | Kubernetes YAML, Helm, Docker | How to write a `GHCRAccessPolicy`, what its status means, why a push is denied | `docs/user/` |
| Admins | Kubernetes, Helm, Flux, cert-manager, Tekton | Install, the PAT, the certificate and CA, grants, setting up a CI pod (hostAliases, certs.d, helper, token), security limits, troubleshooting | `docs/admin/` (CRD and chart value references generated) |
| Developers | Go, controller-runtime, the registry API | Architecture, packages, the fake GHCR, test tiers, releasing | `docs/dev/` |
| Agents | This repo | Playbooks | `docs/agents/`, `.claude/`, `CLAUDE.md` |

## 11. Open questions

1. Does GHCR support the OCI `DELETE` endpoints for manifests by digest and by tag? Settled in phase 2 with the live tests.
2. How long do GHCR's registry bearer tokens live, and does GHCR enforce their scopes? The proxy doesn't rely on either (it caches by the `expires_in` it is given, else 60 seconds, and never hands tokens to clients), but the live tests record the answer in `docs/dev/`.
