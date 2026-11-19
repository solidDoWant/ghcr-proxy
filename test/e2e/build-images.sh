#!/usr/bin/env bash
# Builds the three e2e images (the proxy, the credential helper and the fake
# GHCR) for the host architecture on the local Docker daemon, with the tags
# test/e2e/up.sh loads into kind. Two callers may run it at the same time on a
# developer machine (the two e2e tracks) and share the build tree and the tags,
# so it builds one at a time. CI runs it once before the e2e stages start.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

tag="e2e"
proxy_image="ghcr.io/soliddowant/registrygate:${tag}"
helper_image="ghcr.io/soliddowant/registrygate-credential-helper:${tag}"
arch="$(go env GOARCH)"

mkdir -p build/e2e
# mkdir is atomic and flock is not in every image.
lock=build/e2e/images.lockdir
until mkdir "$lock" 2>/dev/null; do
	if [[ -n "$(find "$lock" -maxdepth 0 -mmin +15 2>/dev/null)" ]]; then rmdir "$lock" 2>/dev/null || true; fi
	sleep 2
done
trap 'rmdir "$lock" 2>/dev/null || true' EXIT

echo "e2e-images: building the images for linux/${arch}" >&2
mkdir -p "build/images/${arch}"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -ldflags="-s -w -X main.version=${tag}" -o "build/images/${arch}/registrygate" ./cmd/registrygate
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -ldflags="-s -w -X main.version=${tag}" -o "build/images/${arch}/docker-credential-registrygate" ./cmd/docker-credential-registrygate
docker buildx build --platform "linux/${arch}" --file Dockerfile --tag "$proxy_image" --load . >&2
docker buildx build --platform "linux/${arch}" --file Dockerfile.credential-helper --tag "$helper_image" --load . >&2
make build-fakeregistry-image >&2
echo "e2e-images: built" >&2
