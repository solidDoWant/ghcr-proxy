# ghcr-proxy for developers

For: people who change the ghcr-proxy code. You know Go, controller-runtime and the registry API.

ghcr-proxy is in early development. The pages on the architecture are not written yet. They will describe:

- the architecture and the packages under `internal/`;
- the fake GHCR in `test/fakeghcr`;
- the test tiers: unit tests, integration tests (envtest with the proxy and the fake GHCR in one process), e2e tests (kind, the fake GHCR and the chart) and live tests (the real `ghcr.io`);
- how acceptance tests are tied to scenarios (the `Tests:` lines of the checklist) and counted by `make acceptance`;
- how to release.

## Test tiers

- `make test` runs the unit tests.
- `make test-integration` runs the `integration` tests. They need only the dev shell: envtest gets its binaries from `KUBEBUILDER_ASSETS`.
- `make test-e2e` runs the `e2e` tests on the kind cluster `E2E_CLUSTER` (default `ghcr-proxy-e2e`). The script `test/e2e/up.sh` creates the cluster with the `ImageVolume` feature gate, loads both images and installs the chart. The cluster is deleted afterwards, unless you set `KEEP_CLUSTER=1`. Without Docker the tests skip.
- `make test-live` runs the `live` tests against the real `ghcr.io`. Only the operator runs them, by hand and never in CI, with the PAT in the file `GHCR_LIVE_PAT_FILE` and the owner in `GHCR_LIVE_OWNER`; they skip without them.

## Getting started

The tools come from the Nix flake. Run `nix develop` (or use `direnv`) in the checkout. Then `make help` lists the commands. The most common ones are:

- `make build`: build `bin/ghcr-proxy` and `bin/docker-credential-ghcr-proxy`.
- `make fmt lint`: format the code and run every check.
- `make test`: run the unit tests.
- `make generate`: regenerate the CRD, RBAC and deepcopy code after a change to `api/`.

Until the other pages exist, read [`SPEC.md`](../../SPEC.md) and the design for the current phase in [`docs/design/`](../design/phase-1.md).
