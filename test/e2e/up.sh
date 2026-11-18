#!/usr/bin/env bash
# Prepares the e2e environment on the kind cluster E2E_CLUSTER (default
# registrygate-e2e, created with test/e2e/kind-config.yaml, ImageVolume on): the
# proxy and credential helper images built for the host architecture and
# loaded into kind, together with the fake GHCR image. The tests install the
# fake GHCR and the proxy chart themselves, because the chart needs the PAT and
# TLS Secrets first. Prints the env file path; it holds the kube context and
# the image names.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

cluster="${E2E_CLUSTER:-registrygate-e2e}"
node_image="mirror.gcr.io/kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5"
tag="e2e"
proxy_image="ghcr.io/soliddowant/registrygate:${tag}"
helper_image="ghcr.io/soliddowant/registrygate-credential-helper:${tag}"
fake_image="ghcr.io/soliddowant/registrygate-fakeregistry:dev"
arch="$(go env GOARCH)"
env_file="build/e2e/${cluster}.env"
mkdir -p build/e2e

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

log() { echo "e2e-up: $*" >&2; }

if ! kind get clusters 2>/dev/null | grep -qx "$cluster"; then
	log "creating kind cluster $cluster"
	kind create cluster --name "$cluster" --image "$node_image" --config test/e2e/kind-config.yaml --wait 120s >&2
fi

log "building the images for linux/${arch}"
mkdir -p "build/images/${arch}"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -ldflags="-s -w -X main.version=${tag}" -o "build/images/${arch}/registrygate" ./cmd/registrygate
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -ldflags="-s -w -X main.version=${tag}" -o "build/images/${arch}/docker-credential-registrygate" ./cmd/docker-credential-registrygate
docker buildx build --platform "linux/${arch}" --file Dockerfile --tag "$proxy_image" --load . >&2
docker buildx build --platform "linux/${arch}" --file Dockerfile.credential-helper --tag "$helper_image" --load . >&2

make build-fakeregistry-image >&2

# Every other image a pod in the cluster pulls comes through the Docker daemon
# too, so the node's containerd never pulls from a registry itself. The list is
# derived from the files that name the images, so Renovate keeps it current.
log "listing the images the cluster's pods pull"
cert_manager_version="$(grep -o -m1 -- '--version v[0-9][0-9.]*' docs/admin/upstreams.md | head -n1 | cut -d' ' -f2)"
{
	grep -E '^\s*repository:' test/e2e/harbor-values.yaml | awk '{print $2}' >"$work/repos"
	grep -E '^\s*tag:' test/e2e/harbor-values.yaml | awk '{print $2}' >"$work/tags"
	paste -d: "$work/repos" "$work/tags"
	grep -oE '"mirror\.gcr\.io/[^"]+"' test/e2e/env_test.go | tr -d '"'
	helm template cert-manager oci://quay.io/jetstack/charts/cert-manager --version "$cert_manager_version" --set crds.enabled=true |
		awk '$1 == "image:" || $1 == "-" && $2 == "image:" {print}' | sed -E 's/.*image: *//; s/"//g'
} | sort -u >"$work/images"
log "pulling $(wc -l <"$work/images") images through the Docker daemon"
pull_image() { docker pull --quiet "$1" >/dev/null; }
export -f pull_image
xargs -P 8 -I{} bash -c 'pull_image {}' <"$work/images"

log "loading the images into kind"
kind load docker-image "$proxy_image" "$helper_image" "$fake_image" --name "$cluster" >&2
# kind load fails on multi-platform images from the Docker daemon's containerd
# store (it imports every platform, but the daemon holds only one). Import each
# image with containerd's own import instead, named as a pod's pinned reference
# names it: the repository, then the digest.
node="${cluster}-control-plane"
import_image() {
	local ref="$1"
	local repo="${ref%%[:@]*}"
	docker save "$ref" | docker exec -i --privileged "$node" ctr --namespace=k8s.io images import --digests --base-name "$repo" --snapshotter=overlayfs - >/dev/null
}
export -f import_image
export node
xargs -P 4 -I{} bash -c 'import_image {}' <"$work/images" >&2

cat >"$env_file" <<ENV
E2E_CLUSTER=${cluster}
E2E_KUBE_CONTEXT=kind-${cluster}
E2E_PROXY_IMAGE=${proxy_image}
E2E_HELPER_IMAGE=${helper_image}
E2E_FAKE_IMAGE=${fake_image}
ENV
log "ready: ${env_file}"
echo "$env_file"
