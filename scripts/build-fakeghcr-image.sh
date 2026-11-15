#!/usr/bin/env bash
# Builds the GHCR test double image ghcr.io/soliddowant/ghcr-proxy-fakeghcr:dev
# for the host architecture and loads it into the local Docker daemon, for e2e
# tests to load into kind. It is never pushed.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

arch=$(go env GOARCH)
mkdir -p "build/images/$arch"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "build/images/$arch/fakeghcr" ./cmd/fakeghcr
docker buildx build --platform "linux/$arch" --file Dockerfile.fakeghcr \
	--tag ghcr.io/soliddowant/ghcr-proxy-fakeghcr:dev --load .
