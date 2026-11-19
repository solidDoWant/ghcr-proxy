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

start=$SECONDS
log() { echo "e2e-up: [+$((SECONDS - start))s] $*" >&2; }

# The cluster needs nothing from the image builds and the builds need nothing
# from the cluster, so create it in the background while the images build. Only
# the image load needs both.
cluster_pid=""
cluster_log="$work/cluster.log"
if ! kind get clusters 2>/dev/null | grep -qx "$cluster"; then
	log "creating kind cluster $cluster (in the background)"
	kind create cluster --name "$cluster" --image "$node_image" --config test/e2e/kind-config.yaml --wait 120s >"$cluster_log" 2>&1 &
	cluster_pid=$!
fi
# A failed build must not leave the cluster creation running.
stop_cluster() {
	if [[ -n "$cluster_pid" ]]; then
		kill "$cluster_pid" 2>/dev/null || true
		wait "$cluster_pid" 2>/dev/null || true
	fi
}
trap 'stop_cluster; rm -rf "$work"' EXIT

# Two tracks may run at the same time and share the build tree and the image
# tags: build one at a time. The second finds everything cached.
mkdir -p build/e2e
# mkdir is atomic and flock is not in every image.
lock=build/e2e/images.lockdir
until mkdir "$lock" 2>/dev/null; do
	if [[ -n "$(find "$lock" -maxdepth 0 -mmin +15 2>/dev/null)" ]]; then rmdir "$lock" 2>/dev/null || true; fi
	sleep 2
done
trap 'stop_cluster; rm -rf "$work"; rmdir "$lock" 2>/dev/null || true' EXIT
log "building the images for linux/${arch}"
mkdir -p "build/images/${arch}"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -ldflags="-s -w -X main.version=${tag}" -o "build/images/${arch}/registrygate" ./cmd/registrygate
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -ldflags="-s -w -X main.version=${tag}" -o "build/images/${arch}/docker-credential-registrygate" ./cmd/docker-credential-registrygate
docker buildx build --platform "linux/${arch}" --file Dockerfile --tag "$proxy_image" --load . >&2
docker buildx build --platform "linux/${arch}" --file Dockerfile.credential-helper --tag "$helper_image" --load . >&2

make build-fakeregistry-image >&2

rmdir "$lock" 2>/dev/null || true
trap 'stop_cluster; rm -rf "$work"' EXIT
log "images built"

# Every other image a pod in the cluster pulls comes through the Docker daemon
# too, so the node's containerd never pulls from a registry itself. The list is
# derived from the files that name the images, so Renovate keeps it current.
log "listing the images the cluster's pods pull"
./scripts/ci/e2e-images.sh >"$work/images"
log "pulling $(wc -l <"$work/images") images through the Docker daemon"
pull_image() { docker pull --quiet "$1" >/dev/null; }
export -f pull_image
# shellcheck disable=SC2016 # $1 must expand in the child shell, not here
xargs -P 8 -I{} bash -c 'pull_image "$1"' _ {} <"$work/images"

if [[ -n "$cluster_pid" ]]; then
	log "waiting for the kind cluster"
	if ! wait "$cluster_pid"; then
		cluster_pid=""
		cat "$cluster_log" >&2
		log "kind cluster creation failed"
		exit 1
	fi
	cluster_pid=""
	cat "$cluster_log" >&2
fi
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
# shellcheck disable=SC2016 # $1 must expand in the child shell, not here
xargs -P 4 -I{} bash -c 'import_image "$1"' _ {} <"$work/images" >&2

cat >"$env_file" <<ENV
E2E_CLUSTER=${cluster}
E2E_KUBE_CONTEXT=kind-${cluster}
E2E_PROXY_IMAGE=${proxy_image}
E2E_HELPER_IMAGE=${helper_image}
E2E_FAKE_IMAGE=${fake_image}
ENV
log "ready: ${env_file}"
echo "$env_file"
