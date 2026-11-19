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

walkthrough_test='^TestAdminDocsGetToAPushAndAMirroredPull$'
go_filter=()
track="${E2E_TRACK:-all}"
if [[ "$tag" == e2e ]]; then
	case "$track" in
	main) go_filter=(-skip "$walkthrough_test") ;;
	walkthrough) go_filter=(-run "$walkthrough_test") ;;
	all) ;;
	*)
		echo "test-e2e: E2E_TRACK must be main, walkthrough or all, not $track" >&2
		exit 2
		;;
	esac
fi

# Both tracks at the same time, each a run of this script on its own cluster.
if [[ "$tag" == e2e && "$track" == all ]] && docker info >/dev/null 2>&1; then
	base="${E2E_CLUSTER:-registrygate-e2e}"
	mkdir -p build/e2e
	main_json="${GO_TEST_JSON:-}"
	walkthrough_json=""
	if [[ -n "$main_json" ]]; then walkthrough_json="${main_json%.json}-walkthrough.json"; fi
	E2E_TRACK=main E2E_CLUSTER="$base" GO_TEST_JSON="$main_json" "$0" e2e >build/e2e/track-main.log 2>&1 &
	main_pid=$!
	E2E_TRACK=walkthrough E2E_CLUSTER="$base-walkthrough" GO_TEST_JSON="$walkthrough_json" "$0" e2e >build/e2e/track-walkthrough.log 2>&1 &
	walkthrough_pid=$!
	status=0
	wait "$main_pid" || status=$?
	echo "==================== e2e track main (exit $status) ===================="
	cat build/e2e/track-main.log
	walkthrough_status=0
	wait "$walkthrough_pid" || walkthrough_status=$?
	echo "==================== e2e track walkthrough (exit $walkthrough_status) ===================="
	cat build/e2e/track-walkthrough.log
	if ((status == 0)); then status=$walkthrough_status; fi
	exit "$status"
fi

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

# The e2e run is verbose: a passing run then still shows each test's time and
# the e2e-wait lines in the stage log.
verbose=()
if [[ "$tag" == e2e ]]; then verbose=(-v); fi

status=0
timeout "$((minutes + 5))m" ./scripts/ci/go-test-json.sh -race -count=1 "${verbose[@]}" -tags "$tag" -timeout "${minutes}m" "${go_filter[@]}" "${packages[@]}" || status=$?
exit "$status"
