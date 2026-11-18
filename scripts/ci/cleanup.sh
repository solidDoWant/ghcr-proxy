#!/bin/sh
# Cleans dockerd's storage at the end of a master push run on Tekton, before
# the cache promoter may snapshot it, and fails when it is still nearly full.
# The builder's garbage collection limits only the build cache: old CI images
# and untagged e2e images would otherwise fill the volume until master runs
# fail and no new snapshot is promoted.
#
#   scripts/ci/cleanup.sh <current CI image tag>
set -eu
current="${1:?usage: cleanup.sh <current CI image tag>}"
# The build cache budget: the CI image's layers (~3.5 GB) and the e2e image
# builds, with room for one flake change.
build_cache_budget="${CI_BUILD_CACHE_BUDGET:-8GB}"
max_percent=80

docker image prune -f >/dev/null
for old in $(docker image ls --format '{{.Repository}}:{{.Tag}}' registrygate-ci); do
	if [ "$old" != "$current" ]; then
		docker image rm "$old" >/dev/null
	fi
done
docker builder prune -f --keep-storage "$build_cache_budget" >/dev/null

# dockerd's storage is its own filesystem, mounted only in the dind sidecar.
used=$(docker run --rm -v /var/lib/docker:/storage:ro --entrypoint /bin/sh "$current" \
	-c 'df -P /storage' | /bin/sh "$(dirname "$0")/df-percent.sh")
docker system df
echo "docker storage: ${used}% used after cleanup"
if [ "$used" -gt "$max_percent" ]; then
	echo "docker storage is ${used}% full after cleanup" >&2
	exit 1
fi
