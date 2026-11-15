#!/usr/bin/env bash
# Prepares the e2e environment on the kind cluster E2E_CLUSTER (default
# ghcr-proxy-e2e, created with test/e2e/kind-config.yaml, ImageVolume on): the
# proxy and credential helper images built for the host architecture and
# loaded into kind, together with the fake GHCR image. The tests install the
# fake GHCR and the proxy chart themselves, because the chart needs the PAT and
# TLS Secrets first. Prints the env file path; it holds the kube context and
# the image names.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

cluster="${E2E_CLUSTER:-ghcr-proxy-e2e}"
node_image="mirror.gcr.io/kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0"
tag="e2e"
proxy_image="ghcr.io/soliddowant/ghcr-proxy:${tag}"
helper_image="ghcr.io/soliddowant/ghcr-proxy-credential-helper:${tag}"
fake_image="ghcr.io/soliddowant/ghcr-proxy-fakeghcr:dev"
arch="$(go env GOARCH)"
env_file="build/e2e/${cluster}.env"
mkdir -p build/e2e

log() { echo "e2e-up: $*" >&2; }

if ! kind get clusters 2>/dev/null | grep -qx "$cluster"; then
	log "creating kind cluster $cluster"
	kind create cluster --name "$cluster" --image "$node_image" --config test/e2e/kind-config.yaml --wait 120s >&2
fi

log "building the images for linux/${arch}"
mkdir -p "build/images/${arch}"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -ldflags="-s -w -X main.version=${tag}" -o "build/images/${arch}/ghcr-proxy" ./cmd/ghcr-proxy
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -ldflags="-s -w -X main.version=${tag}" -o "build/images/${arch}/docker-credential-ghcr-proxy" ./cmd/docker-credential-ghcr-proxy
docker buildx build --platform "linux/${arch}" --file Dockerfile --tag "$proxy_image" --load . >&2
docker buildx build --platform "linux/${arch}" --file Dockerfile.credential-helper --tag "$helper_image" --load . >&2

make build-fakeghcr-image >&2

log "loading the images into kind"
kind load docker-image "$proxy_image" "$helper_image" "$fake_image" --name "$cluster" >&2

cat >"$env_file" <<ENV
E2E_CLUSTER=${cluster}
E2E_KUBE_CONTEXT=kind-${cluster}
E2E_PROXY_IMAGE=${proxy_image}
E2E_HELPER_IMAGE=${helper_image}
E2E_FAKE_IMAGE=${fake_image}
ENV
log "ready: ${env_file}"
echo "$env_file"
