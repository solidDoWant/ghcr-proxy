# Phase 1 acceptance checklist: push and pull through the proxy

Approved for the owner by the operator on 2026-11-15. Only the owner adds scenarios. Each scenario lists its tests on a `Tests:` line (`` `package/dir: TestName/subtest` ``); a scenario passes when every listed test passes on `master`. Workers add their tests to the line; that is not a new scenario. `make acceptance` prints the scoreboard.

## Docker end to end

- **AC-1-01 Build and push.** In a pod set up as `docs/admin/ci-pods.md` describes, with a policy granting push on `soliddowant/app`, `docker build -t ghcr.io/soliddowant/app:1 . && docker push ghcr.io/soliddowant/app:1` succeeds, and the image is in the upstream with that tag and digest.
  Tests:
- **AC-1-02 Pull a private image.** In the same pod, `docker pull` of a private image the policy allows succeeds, and a `FROM` of it in a `docker build` works.
  Tests:
- **AC-1-03 Public images still work.** In the same pod, `docker pull` of a public image in a repository no policy mentions succeeds, and the upstream sees the request with no PAT-backed token.
  Tests:
- **AC-1-04 Push outside the policy is refused.** `docker push ghcr.io/soliddowant/other:1` fails, the error Docker prints names the repository and the push action, and nothing reached the upstream.
  Tests:
- **AC-1-05 Private image outside the policy is invisible.** `docker pull` of a private image the policy doesn't allow fails as it would on GHCR without credentials, and the upstream saw no PAT-backed request for it.
  Tests:
- **AC-1-06 Large layer.** A push of an image with a 2 GiB layer succeeds through the proxy, and the proxy's memory stays under its limit.
  Tests:
- **AC-1-07 Cross-repository mount.** Pushing an image whose layers exist in another repository the policy can pull mounts them; when the policy can't pull the other repository, the push still succeeds by uploading the layers, and no mount from that repository reached the upstream.
  Tests:

## Authentication

- **AC-1-08 Wrong audience.** A ServiceAccount token without the `ghcr-proxy` audience gets `401`, and nothing reached the upstream.
  Tests:
- **AC-1-09 Deleted pod.** After the pod a token is bound to is deleted, that token stops working within the cache TTL.
  Tests:
- **AC-1-10 Garbage credential.** A Basic credential that isn't a valid token gets `401`, never anonymous access.
  Tests:
- **AC-1-11 API server down.** With token review failing, authenticated requests get `503` and the upstream sees nothing PAT-backed.
  Tests:
- **AC-1-12 Token rotation.** The credential helper returns the new token after the token file is replaced, without restarting anything.
  Tests: `internal/credhelper: TestGetReadsReplacedTokenFile`

## Policies and grants

- **AC-1-13 Policy is per ServiceAccount.** A second ServiceAccount in the same namespace that no policy lists gets only anonymous pulls and `403` on push.
  Tests:
- **AC-1-14 Policy is per namespace.** A policy in namespace A naming repository R gives nothing to ServiceAccounts in namespace B, even with the same ServiceAccount name.
  Tests:
- **AC-1-15 Grant caps the policy.** With the grant `ci-(?P<repo>…)` → `soliddowant/${repo}/**`, a policy in `ci-foo` naming `soliddowant/bar` is `Ready=False, reason=OutsideGrant` with `soliddowant/bar` in `status.rejected`; pushes to `soliddowant/bar` are refused while `soliddowant/foo/x` works.
  Tests:
- **AC-1-16 No grant.** A policy in a namespace no grant matches is `Ready=False, reason=NoGrant`, and its ServiceAccounts get only anonymous pulls.
  Tests:
- **AC-1-17 Glob matching.** `soliddowant/foo/*` allows `soliddowant/foo/a` but not `soliddowant/foo/a/b` or `soliddowant/foo`; `soliddowant/foo/**` allows all three; `soliddowant/foo/**` is not inside a grant of `soliddowant/foo/*`.
  Tests:
- **AC-1-18 Live policy changes.** Adding a rule allows the next push within a few seconds, and removing it refuses the next one, without restarting the proxy.
  Tests:
- **AC-1-19 Actions are separate.** A policy granting only `pull` refuses push and delete; `push` also allows pull.
  Tests:

## Upstream credential and protocol

- **AC-1-20 PAT rotation.** Replacing the PAT in its Secret is picked up without a restart, and later requests use the new PAT.
  Tests:
- **AC-1-21 Bad PAT.** With a PAT the upstream rejects, an allowed push gets `502` with a message saying the proxy's credential was rejected, and the PAT appears in no log line, response or status.
  Tests:
- **AC-1-22 Location rewrite.** Upload `Location` headers pointing at the upstream host come back pointing at `ghcr.io`, and blob-download redirects to the storage host come back unchanged.
  Tests:
- **AC-1-23 Catalog and unknown paths.** `/v2/_catalog` and unknown paths get `404` and never reach the upstream.
  Tests:
- **AC-1-24 Not ready until synced.** A replica reports not ready, and serves `503` to authenticated requests, until its policy cache has synced and the PAT and certificate are loaded.
  Tests:

## Operations

- **AC-1-25 Audit line.** Each request writes one JSON line with namespace, ServiceAccount, repository, action, decision and upstream status, and for a manifest push its digest; no line contains the PAT, a ServiceAccount token or an `Authorization` value.
  Tests:
- **AC-1-26 Certificate rotation.** Replacing the TLS Secret is picked up without a restart.
  Tests:
- **AC-1-27 Two replicas.** With two replicas, killing one during a sequence of pushes doesn't fail pushes that start after it is gone, and only one replica writes policy status.
  Tests:
- **AC-1-28 Install from the chart.** The chart installs on kind with only the PAT and TLS Secrets created beforehand, every pod runs as non-root with a read-only root filesystem, and the e2e Docker scenarios above run against that install.
  Tests:
- **AC-1-29 Credential helper protocol.** `docker-credential-ghcr-proxy get` answers for `ghcr.io` (with or without `https://`) and says "credentials not found" for other servers; `store`, `erase` and `list` behave as `SPEC.md` says; the helper image contains only the static binary and runs on amd64 and arm64.
  Tests: `internal/credhelper: TestGetAnswersOnlyForGHCR`, `internal/credhelper: TestGetFailsOnMissingOrEmptyTokenFile`, `internal/credhelper: TestStoreEraseAndListDoNothing`
