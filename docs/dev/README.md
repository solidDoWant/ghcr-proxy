# registrygate for developers

For: people who change the registrygate code. You know Go, controller-runtime and the registry API.

registrygate is in early development. The pages on the architecture are not written yet. They will describe:

- the architecture and the packages under `internal/`;
- the fake registry in `test/fakeregistry` (bearer, basic or no auth, singular or plural upload paths, delete supported or not);
- the test tiers: unit tests, integration tests (envtest with the proxy and the fake GHCR in one process), e2e tests (kind, the fake GHCR and the chart) and live tests (the real `ghcr.io`);
- how acceptance tests are tied to scenarios (the `Tests:` lines of the checklist) and counted by `make acceptance`;
- how to release.

## Test tiers

- `make test` runs the unit tests.
- `make test-integration` runs the `integration` tests. They need only the dev shell: envtest gets its binaries from `KUBEBUILDER_ASSETS`.
- `make test-e2e` runs the `e2e` tests in two tracks at the same time, each on its own kind cluster. The admin-guide walkthrough runs on `E2E_CLUSTER-walkthrough`. It needs a cluster with no registrygate and no cert-manager, so its setup installs only Harbor and the fake registries. All other tests run on `E2E_CLUSTER` (default `registrygate-e2e`). Run one track with `make test-e2e-main` or `make test-e2e-walkthrough`. The script `test/e2e/up.sh` creates a cluster with the `ImageVolume` feature gate and loads the proxy, helper and fake GHCR images (it builds them with `test/e2e/build-images.sh`, unless `E2E_IMAGES_EXIT_FILE` names a file where another process writes the build's exit status; CI builds them once for both stages, while the clusters are created, and `up.sh` waits for the build only before it loads the images). The tests install the fake GHCR and the chart. The clusters are deleted afterwards, unless you set `KEEP_CLUSTER=1`. When `E2E_DISCARD_CLUSTER` is set (CI sets it, because it throws the cluster away), the walkthrough skips its own teardown of releases and namespaces. Without Docker the tests skip. CI runs the tracks as the stages `e2e` and `e2e-walkthrough`.
- `make test-live` runs the `live` tests against the real `ghcr.io`. Only the operator runs them, by hand and never in CI, with the PAT in the file `GHCR_LIVE_PAT_FILE` and the owner in `GHCR_LIVE_OWNER`; they skip without them. [How GHCR behaves](ghcr-behaviour.md) describes what they test and what they record.

## Getting started

The tools come from the Nix flake. Run `nix develop` (or use `direnv`) in the checkout. Then `make help` lists the commands. The most common ones are:

- `make build`: build `bin/registrygate` and `bin/docker-credential-registrygate`.
- `make fmt lint`: format the code and run every check.
- `make test`: run the unit tests.
- `make generate`: regenerate the CRD, RBAC and deepcopy code after a change to `api/`.

Until the other pages exist, read [`SPEC.md`](../../SPEC.md) and the design for the current phase in [`docs/design/`](../design/phase-1.md).
