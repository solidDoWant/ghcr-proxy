# How GHCR behaves

For: people who change the ghcr-proxy code.

This page records what the real `ghcr.io` answers to the questions that the delete design depends on. The live tests in `test/live` ask these questions. Only the operator runs them, by hand, with `make test-live`. The operator then fills in each section below and amends the delete section of the [design](../design/phase-2.md) with the result.

## How the live tests run

The live tests need three environment variables:

- `GHCR_LIVE_PAT_FILE`: the path of a file that holds the PAT.
- `GHCR_LIVE_OWNER`: the GitHub user or organization that owns the test packages.
- `KUBEBUILDER_ASSETS`: the envtest binaries. The dev shell sets it.

If one of them is not set, every live test skips.

The tests start envtest and the proxy in one process. The proxy forwards to `https://ghcr.io`. Each run picks a random run ID. The only grant of the proxy is `<owner>/ghcr-proxy-live-<run id>` and `<owner>/ghcr-proxy-live-<run id>/**`. At the end, the tests delete the packages that the run pushed, through the GitHub REST API at `https://api.github.com/`. They never delete a package outside the run prefix. If a run stops before the cleanup, remove its packages by hand. A later run does not remove them.

The test output shows status codes, registry error codes and `expires_in` only. It never shows the PAT or a token.

## DELETE of a manifest by digest

To be filled by the operator's live run. The `TestGHCRDeleteBehaviour/DELETE_manifest_by_digest` lines give the answer to `DELETE /v2/<name>/manifests/<digest>`, and the answer to a `HEAD` of the digest afterwards.

## DELETE of a manifest by tag

To be filled by the operator's live run. The `TestGHCRDeleteBehaviour/DELETE_manifest_by_tag` lines give the answer to `DELETE /v2/<name>/manifests/<tag>`, and the answers to a `HEAD` of the tag and of its digest afterwards.

## DELETE of a blob

To be filled by the operator's live run. The `TestGHCRDeleteBehaviour/DELETE_blob` lines give the answer to `DELETE /v2/<name>/blobs/<digest>`, and the answer to a `HEAD` of the blob afterwards.

## Delete scopes that the token endpoint accepts

To be filled by the operator's live run. The `TestGHCRDeleteBehaviour/token_endpoint_scopes_for_delete` lines show the answer of the token endpoint for the actions `delete`, `*` and `pull,push,delete`. For each token that the endpoint gives, they also show the answer to a `DELETE` of a manifest by tag with that token.

## Lifetime of registry tokens

To be filled by the operator's live run. The same lines show the `expires_in` value that the token endpoint gives with each token.
