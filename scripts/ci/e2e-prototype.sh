#!/bin/sh
# Prototype of one CI stage on Tekton: builds the CI image and runs
# `make test-e2e` in it, the way scripts/ci/stages.sh will run every stage.
# Runs in the docker Task's step, from the checkout in its workspace.
set -eu
t0=$(date +%s)
elapsed() { echo $(($(date +%s) - t0)); }
ws=$(cd .. && pwd)
ci="$ws/ci"
mkdir -p "$ci/bin" "$ci/docker"

echo "== memory: $(grep MemTotal /proc/meminfo), cpus: $(nproc)"
docker info --format 'dockerd: {{.ServerVersion}}, {{.NCPU}} CPUs, {{.MemTotal}} bytes, storage {{.Driver}}'
docker system df || true

image=$(./scripts/ci/ci-image.sh)
echo "== ci image $image: $(elapsed)s"

# What the stage containers need to reach the registries through registrygate
# like the step's own Docker CLI: the CA bundle, the credential helper, its
# token and the Docker configuration.
cp /etc/ssl/certs/ca-certificates.crt "$ci/ca-bundle.crt"
docker run --rm -v /etc/docker/certs.d:/certsd:ro --entrypoint /bin/sh "$image" \
	-c 'f=$(ls /certsd/*/ca.crt 2>/dev/null | head -n1); if [ -n "$f" ]; then cat "$f"; fi' >>"$ci/ca-bundle.crt"
if [ -x /opt/registrygate/docker-credential-registrygate ]; then
	cp /opt/registrygate/docker-credential-registrygate "$ci/bin/"
	cp /var/run/secrets/registrygate/token "$ci/token"
fi
if [ -f "$HOME/.docker/config.json" ]; then
	cp "$HOME/.docker/config.json" "$ci/docker/config.json"
fi
echo "== /etc/hosts:"
cat /etc/hosts
echo "== docker config:"
cat "$ci/docker/config.json" 2>/dev/null || true

docker volume create gomod >/dev/null
docker volume create gobuild >/dev/null

echo "== e2e start: $(elapsed)s"
status=0
docker run --rm --network host \
	-v "$ws:$ws" -w "$ws/src" \
	-v gomod:/cache/gomod -v gobuild:/cache/gobuild \
	-e GOMODCACHE=/cache/gomod -e GOCACHE=/cache/gobuild/go -e GOLANGCI_LINT_CACHE=/cache/gobuild/golangci-lint \
	-e DOCKER_HOST=tcp://127.0.0.1:2375 -e DOCKER_CONFIG="$ci/docker" \
	-e REGISTRYGATE_HOSTS="${REGISTRYGATE_HOSTS:-}" -e REGISTRYGATE_TOKEN_FILE="$ci/token" \
	-e CI_BIN="$ci/bin" -e CI_CA="$ci/ca-bundle.crt" \
	-e E2E_CLUSTER=registrygate-ci \
	"$image" bash -c '
		source /etc/devshell.sh
		export PATH="$CI_BIN:$PATH" SSL_CERT_FILE="$CI_CA"
		git config --global --add safe.directory "*"
		start=$(date +%s)
		make test-e2e 2>&1 | while IFS= read -r line; do printf "[%4ds] %s\n" $(($(date +%s) - start)) "$line"; done
		exit "${PIPESTATUS[0]}"
	' || status=$?
echo "== e2e done (exit $status): $(elapsed)s"
docker system df || true
exit "$status"
