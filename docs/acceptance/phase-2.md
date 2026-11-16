# Phase 2 acceptance checklist: delete, mirrors and operations

Approved for the owner by the operator on 2026-11-15. Only the owner adds scenarios. Each scenario lists its tests on a `Tests:` line (`` `package/dir: TestName/subtest` ``); a scenario passes when every listed test passes on `master`. Workers add their tests to the line; that is not a new scenario. `make acceptance` prints the scoreboard.

Scenarios marked **(live)** run against the real `ghcr.io` through `make test-live`, which only the operator runs, by hand and outside CI (the PAT is the owner's own). They pass when the operator's run is green and its result is posted on the issue; the scoreboard shows them as manual.

## Delete

- **AC-2-01 Delete by digest.** With a policy granting `delete`, deleting a manifest by digest through the proxy (`crane delete` / `regctl manifest rm`) removes it, against the fake and against real GHCR **(live)**.
  Tests:
- **AC-2-02 Delete needs the delete action.** A policy without `delete` gets `403`, and the upstream sees nothing.
  Tests:
- **AC-2-03 GHCR's answer is recorded.** `docs/dev/ghcr-behaviour.md` records, from the live tests, whether GHCR supports OCI delete by digest and by tag, and how long its registry tokens live **(live)**.
  Tests:

## Pull mirrors

- **AC-2-04 Allowed pulls use the policy mirror.** With a policy mirror configured, an allowed pull of a private image is served from the mirror (the upstream sees no manifest or blob request for it), using the mirror's own credentials.
  Tests: `internal/registry: TestForwarderServesAllowedPullsFromThePolicyMirror`
- **AC-2-05 Anonymous pulls use the anonymous mirror.** Anonymous and not-allowed pulls go to the anonymous mirror, never to the policy mirror.
  Tests: `internal/registry: TestForwarderServesAnonymousPullsFromTheAnonymousMirror`
- **AC-2-06 Fallback.** With the mirror down or answering `5xx`, pulls fall back to the upstream; with `fallbackToUpstream: false` they fail with `502`.
  Tests: `internal/registry: TestForwarderFallsBackToTheUpstreamWhenTheMirrorFails`, `internal/registry: TestForwarderMapsMirrorLocationsBackToTheClientHost`
- **AC-2-07 Pushes skip mirrors.** Pushes, deletes, tag lists and mounts go to the upstream even with mirrors configured.
  Tests: `internal/registry: TestForwarderSendsBlobChecksAndWritesToTheUpstreamOnly`
- **AC-2-08 PAT never reaches a mirror.** No request to either mirror carries the PAT or a PAT-backed token.
  Tests: `internal/registry: TestForwarderNeverSendsThePATToAMirror`
- **AC-2-09 Harbor as the mirror.** With a Harbor proxy-cache project for the fake GHCR as the policy mirror, a Docker pull in the e2e pod works and the second pull is served by Harbor.
  Tests:

## Operations

- **AC-2-10 Metrics.** `/metrics` exposes request counts by action, decision and status class, upstream and mirror latency histograms, token review results and cache hits, with only fixed-value labels (no repository, namespace or ServiceAccount labels).
  Tests: `internal/metrics: TestRecordedEventsAppearWithFixedLabels`, `internal/server: TestRequestsAreCountedByActionDecisionAndStatusClass`, `internal/registry: TestForwarderCountsUpstreamRequestsAndMirrorFallbacks`, `internal/registry: TestMetricsSeriesCountStaysSmallWithBothMirrors`, `internal/authn: TestReviewsCountResultAndCacheUse`
- **AC-2-11 Admission check.** With the chart's admission webhook enabled, applying a policy whose repositories are outside the namespace's grant is rejected with a message naming the patterns; a policy inside the grant applies.
  Tests: `internal/webhook: TestAdmissionRejectsPoliciesOutsideTheGrant`, `internal/webhook: TestValidateCreate`, `internal/webhook: TestValidateUpdate`
- **AC-2-12 Chart options.** Enabling the PodDisruptionBudget, NetworkPolicy and ServiceMonitor values renders working objects, and the e2e scenarios still pass with the NetworkPolicy on.
  Tests: `test/e2e: TestNetworkPolicyBlocksNamespacesOutsideTheClientSelector`
- **AC-2-13 Docs walk-through.** Following `docs/admin/` from an empty kind cluster (cert-manager, a name-constrained CA, the chart, a CI pod) gets to a working `docker push`, scripted as an e2e test that uses only commands from the docs.
  Tests: `test/e2e: TestAdminDocsWalkThroughEndsWithWorkingDockerPush`
