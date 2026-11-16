# ghcr-proxy

ghcr-proxy is a proxy for the GitHub container registry, `ghcr.io`. It runs in a Kubernetes cluster and holds one GitHub personal access token (PAT). CI pods push, pull and delete images through the proxy, and they never see the PAT. Each pod signs in with the token of its own Kubernetes ServiceAccount. A `GHCRAccessPolicy` resource says what each ServiceAccount can do, and the configuration of the proxy limits each namespace.

ghcr-proxy is in early development. The latest release is 0.1.1. It covers pushing and pulling through the proxy. [`SPEC.md`](SPEC.md) describes the full system. [`docs/README.md`](docs/README.md) lists the documentation for users, admins and developers. ghcr-proxy is licensed under the GNU Affero General Public License v3.0 (see [`LICENSE`](LICENSE)).
