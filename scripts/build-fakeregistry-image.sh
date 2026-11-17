#!/usr/bin/env bash
# Builds the registry test double image ghcr.io/soliddowant/registrygate-fakeregistry:dev
# for the host architecture and loads it into the local Docker daemon, for e2e
# tests to load into kind. It is never pushed.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

arch=$(go env GOARCH)
mkdir -p "build/images/$arch"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "build/images/$arch/fakeregistry" ./cmd/fakeregistry
docker buildx build --platform "linux/$arch" --file Dockerfile.fakeregistry \
	--tag ghcr.io/soliddowant/registrygate-fakeregistry:dev --load .
