# How GHCR behaves

For: people who change the registrygate code.

This page records what the real `ghcr.io` answers to the questions that the delete design depends on. The live tests in `test/live` ask these questions. Only the operator runs them, by hand, with `make test-live`. The operator then fills in each section below and amends the delete section of the [design](../design/phase-2.md) with the result.

## How the live tests run

The live tests need three environment variables:

- `GHCR_LIVE_PAT_FILE`: the path of a file that holds the PAT.
- `GHCR_LIVE_OWNER`: the GitHub user or organization that owns the test packages.
- `KUBEBUILDER_ASSETS`: the envtest binaries. The dev shell sets it.

If one of them is not set, every live test skips.

The tests start envtest and the proxy in one process. The proxy forwards to `https://ghcr.io`. Each run picks a random run ID. The only grant of the proxy is `<owner>/registrygate-live-<run id>` and `<owner>/registrygate-live-<run id>/**`. At the end, the tests delete the packages that the run pushed, through the GitHub REST API at `https://api.github.com/`. They never delete a package outside the run prefix. If a run stops before the cleanup, remove its packages by hand. A later run does not remove them.

The test output shows status codes, registry error codes and `expires_in` only. It never shows the PAT or a token.

## Results of the live run

The operator ran the live tests on 2026-11-16 against `master` at `a700f7e`. Every test passed. The push, pull, cross-repository mount and anonymous public pull through the proxy all worked. The cleanup deleted every package the run had pushed.

## DELETE of a manifest by digest

GHCR refuses it: `405` with the registry error code `UNSUPPORTED`. A `HEAD` of the digest afterwards still answers `200`, so nothing was deleted. The proxy must delete by digest through the GitHub REST API instead.

## DELETE of a manifest by tag

GHCR refuses it: `405 UNSUPPORTED`. A `HEAD` of the tag and of its digest afterwards both answer `200`.

## DELETE of a blob

GHCR refuses it: `405 UNSUPPORTED`. A `HEAD` of the blob afterwards answers `200`.

## Delete scopes that the token endpoint accepts

The token endpoint gives a token (`200`) for each of the actions `delete`, `*` and `pull,push,delete`. With each of them, a `DELETE` of a manifest by tag still answers `405 UNSUPPORTED`. The scope makes no difference: GHCR doesn't support the registry delete endpoints at all.

## Lifetime of registry tokens

The token endpoint gives `expires_in` as `0` (or leaves it out). The proxy treats that as "not given" and keeps each token for 60 seconds.
