# Phase 3 acceptance checklist: registrygate

Approved by the owner on 2026-11-16. Only the owner adds scenarios. Each scenario lists its tests on a `Tests:` line (`` `package/dir: TestName/subtest` ``); a scenario passes when every listed test passes on `master`. Workers add their tests to the line; that is not a new scenario. `make acceptance` prints the scoreboard.

In the e2e scenarios, "a CI pod" is a pod with a dind sidecar set up exactly as `docs/admin/` says: `hostAliases` for each intercepted host, `certs.d/<host>/ca.crt`, the helper image volume, `docker-credential-registrygate configure`, and a projected token. "Upstream A" is the fake GHCR and "upstream B" is a second fake registry with plural upload paths and Basic-credential token auth.

## Rename

- **AC-3-01 New names.** The binaries, images (`ghcr.io/soliddowant/registrygate`, `ghcr.io/soliddowant/registrygate-credential-helper`), chart (`oci://ghcr.io/soliddowant/charts/registrygate`), Go module, API group (`registrygate.soliddowant.dev`), audience default and metric names use `registrygate`. `GHCRAccessPolicy` and the 0.2.x config file are gone.
  Tests:

## Upstreams and interception

- **AC-3-02 Two upstreams.** With `UpstreamRegistry` objects for upstreams A and B, a CI pod pulls a public image from each through the proxy.
  Tests:
- **AC-3-03 Certificate by server name.** Each host is served with its own `UpstreamRegistry`'s certificate. A TLS handshake for a server name no ready `UpstreamRegistry` claims fails. A certificate whose SANs don't cover a host makes the object `Ready=False, reason=CertificateMismatch`.
  Tests: `internal/server: TestSNICertificatesPickByServerName`, `internal/model: TestBuildServesReadyUpstreamHostsWithTheirCertificate`
- **AC-3-04 Host conflict.** Two `UpstreamRegistry` objects claiming the same host are both `Ready=False, reason=HostConflict` for it, and the host isn't served.
  Tests: `internal/server: TestSNICertificatesRefuseHostsInConflict`, `internal/model: TestBuildHostConflictMarksBothAndServesNeither`
- **AC-3-05 Admin namespaces only.** An `UpstreamRegistry` outside the admin namespaces is ignored by the proxy and, with the webhook on, rejected.
  Tests: `internal/webhook: TestUpstreamValidatorAdmitsOnlyAdminNamespaces`, `internal/webhook: TestAdmissionRejectsPoliciesOutsideTheGrant`
- **AC-3-06 Registry API only.** A request outside `/v2` gets `404`. A non-canonical path (`..`, `//`, encoded `/`, `/V2/`, `/%76%32/`) gets `400`. Neither sends anything upstream.
  Tests: `internal/server: TestNonCanonicalPathsAre400AndNeverReachTheUpstream`, `internal/server: TestCatalogAndUnknownPathsAre404AndNeverReachTheUpstream`, `internal/registry: TestClassifyRefusesNonCanonicalPaths`
- **AC-3-07 Upstream auth by challenge.** The proxy works with an upstream that allows anonymous access, one that challenges with Basic, and one that challenges with Bearer. A token endpoint on a host other than the `url` host or `tokenRealmHosts` is refused with `502`.
  Tests:

## Authentication

- **AC-3-08 ServiceAccount tokens.** A valid token with the `registrygate` audience authenticates; a bad or wrong-audience token gets `401` and is never treated as anonymous; a request with no credential is anonymous; a `Bearer` credential gets `401`.
  Tests:

## Credentials and policies

- **AC-3-09 Push with a credential.** A CI pod whose policy has a credential rule pushes to upstream A with `docker push`; the upstream sees the credential's identity.
  Tests:
- **AC-3-10 Push denied.** A push no rule allows gets `403 DENIED` naming the namespace, ServiceAccount, repository and action, for a ServiceAccount, or `401` with the Basic challenge when anonymous. Nothing is sent upstream.
  Tests:
- **AC-3-11 Ceilings.** A credential rule outside its credential's ceiling has `Accepted=False, reason=OutsideCeiling` in `status.rules`, keyed by the rule's name, and is ignored; the policy's other rules still apply.
  Tests:
- **AC-3-12 Admission between namespaces.** A rule referencing a credential, mirror or upstream whose `allowedNamespaces` doesn't admit the policy's namespace has `Accepted=False, reason=NotAllowed`. When admission is withdrawn, the next request no longer uses it.
  Tests:
- **AC-3-13 Several credentials by repository.** A policy with two credential rules for upstream A, on disjoint repositories and with different credentials, pushes each repository with its own credential.
  Tests:
- **AC-3-14 Ambiguous rules.** A request matching rules that name different credentials (or different mirrors) is denied with `403` naming both rules. Rules naming the same credential combine their actions.
  Tests:
- **AC-3-15 Credentialed pulls skip mirrors.** When a credential rule and a mirror rule both match a pull, the credential wins and the pull goes to the upstream with the credential; no mirror sees it.
  Tests:
- **AC-3-16 Delete.** With `delete` allowed, OCI `DELETE` of a manifest by digest, by tag, and of a blob is forwarded with the credential, and the upstream's answer is returned (including `405 UNSUPPORTED` from an upstream that doesn't support it). Without `delete`, `403` and nothing is sent.
  Tests:
- **AC-3-17 Upload sessions on any registry.** Pushes work on upstream A (singular `blobs/upload/` sessions) and upstream B (plural). A session URL forged to point outside the same repository's `/v2/<name>/blobs/` on the upstream gets `404` and sends nothing.
  Tests:
- **AC-3-18 Mounts.** A cross-repository mount whose source resolves to the same credential is forwarded as a mount; otherwise `mount` and `from` are dropped and the push still succeeds.
  Tests:

## Secrets

- **AC-3-19 Rotation without restart.** Changing an `UpstreamCredential`'s Secret makes the next push use the new credential; changing a mirror's Secret makes the next mirror pull use it; renewing a serving certificate Secret makes new connections get the new certificate. No restart.
  Tests:
- **AC-3-20 Tenant Secrets through a named Role.** A `RegistryMirror` in a tenant namespace, whose Secret the proxy can read only through a Role with `resourceNames`, is ready and serves pulls. Without the Role it is `Ready=False, reason=SecretUnavailable`, and pulls fall back as for a failed mirror.
  Tests:
- **AC-3-21 Missing Secrets fail closed.** A credential whose Secret is missing or lacks its keys is `Ready=False`, and pushes that would use it get `403`; nothing is sent with a stale or empty credential.
  Tests:
- **AC-3-22 Separate key names.** A Secret with `username` and `password` keys (a harbor-operator robot's opaque Secret) works as is, and `usernameKey` / `passwordKey` select other key names.
  Tests:

## Mirrors

- **AC-3-23 A Harbor project per namespace.** Two CI namespaces, each with its own `RegistryMirror` pointing at its own Harbor proxy-cache project for upstream A, pull the same public image; Harbor lists it in both projects, and each namespace's pulls reach only its own project.
  Tests:
- **AC-3-24 Default mirror.** Anonymous pulls and pulls no rule matches go to the upstream's `defaultMirrorRef` when set, and to the upstream anonymously when not.
  Tests:
- **AC-3-25 Fallback.** With a mirror down or answering `5xx`, pulls fall back to the upstream; with `fallbackToUpstream: false` they fail with `502`; a mirror `401`/`403` gives `502` and never falls back.
  Tests:
- **AC-3-26 Mirror URLs are limited.** A `RegistryMirror` whose URL doesn't match its upstream's `allowedMirrorURLs` is `Ready=False, reason=URLNotAllowed` and is never contacted.
  Tests:
- **AC-3-27 Credentials stay with their endpoint.** No request to a mirror carries an upstream credential or a token made from one, and no request to an upstream carries a mirror credential.
  Tests:
- **AC-3-28 Builds through mirrors.** A CI pod runs `docker build` with `FROM` images from upstreams A and B, and both base images come through its mirrors, with no change to the Dockerfile and no BuildKit configuration.
  Tests:

## Client side

- **AC-3-29 Credential helper hosts.** The helper answers `get` only for hosts in `REGISTRYGATE_HOSTS`, and answers "credentials not found" for any other.
  Tests: `internal/credhelper: TestGetAnswersOnlyForListedHosts`, `internal/credhelper: TestGetAnswersNothingWithoutHosts`
- **AC-3-30 `configure`.** `docker-credential-registrygate configure` adds `credHelpers` entries for every listed host to an existing `config.json`, keeping its other keys, or creates it; `docker buildx build` works afterwards.
  Tests: `internal/credhelper: TestConfigureMergesIntoExistingConfig`, `internal/credhelper: TestConfigureCreatesFileAndDirectoryPrivately`, `internal/credhelper: TestConfigureLeavesInvalidFilesUntouched`, `internal/credhelper: TestConfigureRejectsNoHosts`

## Operations

- **AC-3-31 Status.** Every kind reports `observedGeneration` and `Ready` with the reasons in the spec; `RegistryAccessPolicy` reports `status.rules[]` by name.
  Tests: `internal/controller: TestStatusShowsEachReadinessOutcome`, `internal/controller: TestStatusIsNotRewrittenWhenNothingChanged`, `internal/controller: TestStatusFollowsASecretChange`
- **AC-3-32 Admission webhook.** With the webhook on, it rejects an `UpstreamRegistry` outside the admin namespaces, a rule with both or neither of `credentialRef` and `mirrorRef`, a mirror rule with a non-pull action, and a credential rule outside its ceiling (naming the patterns). A valid policy applies.
  Tests: `internal/webhook: TestValidateCreate`, `internal/webhook: TestValidateUpdate`, `internal/webhook: TestAdmissionRejectsPoliciesOutsideTheGrant`
- **AC-3-33 Metrics and audit.** `/metrics` has the series in the spec with `upstream` as the only object-name label. Each request writes one audit line with the upstream, rule, and credential or mirror used, and never a credential, token or header value.
  Tests:
- **AC-3-34 Docs walk-through.** Following `docs/admin/` from an empty kind cluster (cert-manager, one name-constrained CA per upstream, the chart, two upstreams, a tenant mirror, a CI pod) gets to a working `docker push` and a mirrored `docker pull`, scripted as an e2e test that uses only commands from the docs.
  Tests:
- **AC-3-35 Live GHCR (by hand).** With the live PAT behind an `UpstreamCredential`, the operator's `make test-live` run pushes and pulls through the proxy to the real `ghcr.io`, and delete returns GHCR's `405`.
  Tests:
