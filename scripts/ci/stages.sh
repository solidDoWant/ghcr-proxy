#!/bin/sh
# Runs the CI stages on Tekton, all at the same time, each in its own
# container of the CI image (scripts/ci/ci-image.sh), inside the docker Task's
# dind. Runs in the docker Task's step, from the checkout at $WS/src, where WS
# is the Task's workspace: the step and dockerd see it at the same path.
#
#   scripts/ci/stages.sh pull_request|push
#
# A pull request runs the stages its changes need (scripts/ci/changes.py); a
# push runs them all and then cleans the Docker storage that the cache
# promoter keeps. The script writes $WS/ci/stages.json, each stage's name,
# result, time and failure report (scripts/ci/stages-report.py), also to the
# file DOCKER_TASK_OUTPUT names (the docker Task's result, read by report), and
# exits 0 also when a stage failed: the report task fails the run.
set -eu

event="${1:?usage: stages.sh pull_request|push}"
t0=$(date +%s)
elapsed() { echo $(($(date +%s) - t0)); }

src=$(git rev-parse --show-toplevel)
cd "$src"
WS=$(dirname "$src")
ci="$WS/ci"
out="$ci/stages"
mkdir -p "$ci/bin" "$ci/docker" "$out"

image=$(./scripts/ci/ci-image.sh)
echo "ci image $image ready after $(elapsed)s"

# Runs a command in the CI image's dev shell, from the checkout.
in_image() {
	docker run --rm -i -v "$WS:$WS" -w "$src" "$image" bash -c \
		'source /etc/devshell.sh && git config --global --add safe.directory "*" && "$@"' bash "$@"
}

if [ "$event" = push ]; then
	stages=$(in_image ./scripts/ci/changes.py push </dev/null)
else
	# HEAD is the PR head merged with master; HEAD^1 is the PR head.
	git diff --name-only "$(git merge-base HEAD^1 origin/master)" HEAD^1 >"$ci/changed-files"
	stages=$(in_image ./scripts/ci/changes.py pull_request <"$ci/changed-files")
fi
if [ -z "$stages" ]; then
	echo "no stage needs to run for these changes"
	echo '[]' >"$ci/stages.json"
	if [ -n "${DOCKER_TASK_OUTPUT:-}" ]; then cp "$ci/stages.json" "$DOCKER_TASK_OUTPUT"; fi
	exit 0
fi
echo "stages: $stages"

# What a stage needs to reach the registries through registrygate like this
# step's own Docker CLI: the system CAs plus registrygate's CA (dockerd's
# certs.d, the same CA for every intercepted host), the credential helper, its
# token and the Docker configuration that names it.
cp /etc/ssl/certs/ca-certificates.crt "$ci/ca-bundle.crt"
docker run --rm -v /etc/docker/certs.d:/certsd:ro --entrypoint /bin/sh "$image" \
	-c 'f=$(ls /certsd/*/ca.crt 2>/dev/null | head -n1); if [ -n "$f" ]; then cat "$f"; fi' >>"$ci/ca-bundle.crt"
if [ -x /opt/registrygate/docker-credential-registrygate ]; then
	cp /opt/registrygate/docker-credential-registrygate "$ci/bin/"
	cp /var/run/secrets/registrygate/token "$ci/token"
fi
if [ -f "${HOME:-/nonexistent}/.docker/config.json" ]; then
	cp "$HOME/.docker/config.json" "$ci/docker/config.json"
fi

# The Go caches live on dockerd's volume, which the cache promoter keeps after
# a successful master push run.
docker volume create gomod >/dev/null
docker volume create gobuild >/dev/null

# check-generate rewrites the generated files in place while the other stages
# build them, so it gets its own copy of the tree, mounted at the same path so
# that Go's build cache entries still match.
generate_src="$WS/generate-src"

command_of() {
	case "$1" in
	lint) echo 'make lint' ;;
	unit) echo 'make test && make build' ;;
	generate) echo 'make check-generate' ;;
	integration) echo 'make test-integration' ;;
	e2e) echo 'make test-e2e' ;;
	docs)
		# The style report is advisory, as in ci.yml.
		if [ "$event" = pull_request ]; then
			echo 'make docs-check && ./scripts/check-tracking-ids.py && { ./scripts/docs/style_report.sh origin/master || true; }'
		else
			echo 'make docs-check && ./scripts/check-tracking-ids.py'
		fi
		;;
	scoreboard) echo './scripts/acceptance-scoreboard_test.py' ;;
	esac
}

start_stage() {
	stage="$1"
	mount="-v $WS:$WS"
	if [ "$stage" = generate ]; then
		rm -rf "$generate_src"
		git clone -q --no-hardlinks "$src" "$generate_src"
		git -C "$generate_src" checkout -q --detach "$(git rev-parse HEAD)"
		mount="$mount -v $generate_src:$src"
	fi
	date +%s >"$out/$stage.start"
	(
		status=0
		# shellcheck disable=SC2086 # $mount is a list of options
		docker run --rm --name "stage-$stage" --network host $mount -w "$src" \
			-v gomod:/cache/gomod -v gobuild:/cache/gobuild \
			-e GOMODCACHE=/cache/gomod -e GOCACHE=/cache/gobuild/go \
			-e GOLANGCI_LINT_CACHE=/cache/gobuild/golangci-lint \
			-e DOCKER_HOST=tcp://127.0.0.1:2375 -e DOCKER_CONFIG="$ci/docker" \
			-e REGISTRYGATE_HOSTS="${REGISTRYGATE_HOSTS:-}" -e REGISTRYGATE_TOKEN_FILE="$ci/token" \
			-e CI_BIN="$ci/bin" -e CI_CA="$ci/ca-bundle.crt" -e CI_STATS="$out/$stage.stats" \
			-e E2E_CLUSTER=registrygate-ci \
			-e STAGE_COMMAND="$(command_of "$stage")" \
			"$image" bash -c '
				source /etc/devshell.sh
				export PATH="$CI_BIN:$PATH" SSL_CERT_FILE="$CI_CA"
				git config --global --add safe.directory "*"
				status=0
				bash -c "set -e; $STAGE_COMMAND" || status=$?
				# The container cgroup: this stage alone (the kind node of e2e
				# is a container of its own).
				{
					echo "memory_peak $(cat /sys/fs/cgroup/memory.peak 2>/dev/null || echo 0)"
					grep usage_usec /sys/fs/cgroup/cpu.stat 2>/dev/null || echo "usage_usec 0"
				} >"$CI_STATS"
				exit "$status"
			' >"$out/$stage.log" 2>&1 || status=$?
		date +%s >"$out/$stage.end"
		echo "$status" >"$out/$stage.exit"
	) &
}

for stage in $stages; do
	start_stage "$stage"
done
echo "started after $(elapsed)s: $stages"

# Prints each passed stage's log as one block as soon as it ends, and the
# failed ones last, so the failure is at the end of the step log.
printed=""
failed=""
remaining="$stages"
while [ -n "$remaining" ]; do
	next=""
	for stage in $remaining; do
		if [ ! -f "$out/$stage.exit" ]; then
			next="$next $stage"
			continue
		fi
		took=$(($(cat "$out/$stage.end") - $(cat "$out/$stage.start")))
		if [ "$(cat "$out/$stage.exit")" = 0 ]; then
			echo "==================== stage $stage: passed in ${took}s ===================="
			cat "$out/$stage.log"
		else
			echo "stage $stage: FAILED after ${took}s (log below, after the other stages)"
			failed="$failed $stage"
		fi
		printed="$printed $stage"
	done
	remaining=$(echo "$next" | xargs)
	if [ -n "$remaining" ]; then sleep 2; fi
done
wait
for stage in $failed; do
	took=$(($(cat "$out/$stage.end") - $(cat "$out/$stage.start")))
	echo "==================== stage $stage: FAILED after ${took}s ===================="
	cat "$out/$stage.log"
done

# Leave nothing but caches on the volume the promoter may keep: a kind
# cluster left behind is only its node containers.
leftover=$(docker ps -aq)
if [ -n "$leftover" ]; then
	# shellcheck disable=SC2086 # a list of IDs
	docker rm -f $leftover >/dev/null 2>&1 || true
fi

if [ "$event" = push ]; then
	date +%s >"$out/cleanup.start"
	status=0
	./scripts/ci/cleanup.sh "$image" >"$out/cleanup.log" 2>&1 || status=$?
	date +%s >"$out/cleanup.end"
	echo "$status" >"$out/cleanup.exit"
	echo "==================== cleanup (exit $status) ===================="
	cat "$out/cleanup.log"
	stages="$stages cleanup"
fi

# shellcheck disable=SC2086 # a list of stage names
in_image ./scripts/ci/stages-report.py "$out" "$ci/stages.json" $stages
if [ -n "${DOCKER_TASK_OUTPUT:-}" ]; then cp "$ci/stages.json" "$DOCKER_TASK_OUTPUT"; fi
echo "all stages done after $(elapsed)s"
