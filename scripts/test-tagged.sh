#!/usr/bin/env bash
# Runs the Go tests behind one build tag (`make test-integration`,
# `make test-e2e`, `make test-live`): only the packages with a
# `//go:build <tag>` file, with the race detector. Passes with "no tests yet"
# while no such file exists.
#
#   scripts/test-tagged.sh integration
#   E2E_CLUSTER=registrygate-12 scripts/test-tagged.sh e2e
#   GHCR_LIVE_PAT_FILE=... GHCR_LIVE_OWNER=... scripts/test-tagged.sh live
#
# GO_TEST_JSON=file also writes the `go test -json` events there
# (scripts/ci/go-test-json.sh).
#
# `integration` needs nothing but the dev shell: envtest's binaries come from
# KUBEBUILDER_ASSETS and the fake GHCR runs in-process.
#
# For `e2e` the script runs test/e2e/up.sh (a kind cluster with the ImageVolume
# feature gate, the images loaded into it, the proxy from its chart) and
# deletes a cluster it created afterwards, unless KEEP_CLUSTER=1. Without
# Docker the tests skip with a message.
#
# `live` talks to the real ghcr.io with the PAT in the file GHCR_LIVE_PAT_FILE
# and the owner GHCR_LIVE_OWNER; the tests skip without them. Only the
# operator runs them, by hand: never in CI, never by agents.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

tag="${1:?usage: test-tagged.sh integration|e2e|live}"
mapfile -t packages < <(
	git ls-files --cached --others --exclude-standard -- '*.go' |
		xargs -r grep -l -E "^//go:build .*\\b${tag}\\b" |
		xargs -r -n1 dirname | sort -u | sed 's|^|./|'
)
if ((${#packages[@]} == 0)); then
	echo "test-$tag: no tests yet"
	exit 0
fi

case "$tag" in
e2e) minutes=50 ;;
*) minutes=25 ;;
esac
echo "test-$tag: ${packages[*]}"

if [[ "$tag" == e2e ]]; then
	if ! docker info >/dev/null 2>&1; then
		echo "test-e2e: Docker is not available; the e2e tests skip" >&2
	else
		export E2E_CLUSTER="${E2E_CLUSTER:-registrygate-e2e}"
		created=true
		if kind get clusters 2>/dev/null | grep -qx "$E2E_CLUSTER"; then created=false; fi
		# shellcheck disable=SC2329 # runs through the EXIT trap
		cleanup() {
			if [[ "$created" == true && "${KEEP_CLUSTER:-}" != 1 ]]; then
				test/e2e/down.sh || true
			fi
		}
		trap cleanup EXIT
		env_file="$(test/e2e/up.sh)"
		set -a
		# shellcheck disable=SC1090
		source "$env_file"
		set +a
	fi
fi

if [[ "$tag" == live && -z "${GHCR_LIVE_PAT_FILE:-}" ]]; then
	echo "test-live: GHCR_LIVE_PAT_FILE is not set; the live tests skip" >&2
fi

status=0
timeout "$((minutes + 5))m" ./scripts/ci/go-test-json.sh -race -count=1 -tags "$tag" -timeout "${minutes}m" "${packages[@]}" || status=$?
exit "$status"
