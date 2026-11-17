#!/usr/bin/env bash
# Lints the Helm chart without a cluster (`make chart-lint`, part of
# `make lint`): helm lint, then renders with the required values and checks the
# properties the chart promises (hardening, resources, config rollout, Service IP, upload signing Secret).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

chart=deploy/charts/registrygate
required=()

helm lint --strict "$chart" "${required[@]}"

render() { helm template registrygate "$chart" --namespace registrygate "${required[@]}" "$@"; }
fail() { echo "chart-lint: $1" >&2; exit 1; }

default=$(render)
for want in 'runAsNonRoot: true' 'readOnlyRootFilesystem: true' 'allowPrivilegeEscalation: false' '- ALL' 'checksum/config:' 'POD_NAMESPACE'; do
	grep -qF -- "$want" <<<"$default" || fail "default render lacks: $want"
done
# Memory request equals limit, and there is no CPU limit.
mem=$(grep -E '^ +memory:' <<<"$default" | sort -u | wc -l)
[ "$mem" -eq 1 ] || fail "memory request and limit differ"
helm template registrygate "$chart" "${required[@]}" --show-only templates/deployment.yaml |
	awk '/limits:/{f=1;next} f&&/cpu:/{exit 1} /requests:/{f=0}' || fail "a CPU limit is set"

sum() { render "$@" --show-only templates/deployment.yaml | grep 'checksum/config'; }
[ "$(sum)" != "$(sum --set authentication.audience=other)" ] || fail "config change does not change the checksum"

render --set service.clusterIP=10.96.0.50 --show-only templates/service.yaml | grep -qF 'clusterIP: "10.96.0.50"' ||
	fail "service.clusterIP is not used"

opts=(--set podDisruptionBudget.enabled=true --set networkPolicy.enabled=true --set serviceMonitor.enabled=true
	--set 'networkPolicy.apiServerCIDRs={10.0.0.5/32}' --set 'networkPolicy.egressHosts={192.0.2.7/32}' --set 'networkPolicy.egressPorts={5000}' --set networkPolicy.clientNamespaceSelector.matchLabels.team=ci)
for kind in PodDisruptionBudget NetworkPolicy ServiceMonitor; do
	grep -qF "kind: $kind" <<<"$(render)" && fail "$kind rendered by default"
	grep -qF "kind: $kind" <<<"$(render "${opts[@]}")" || fail "$kind not rendered when enabled"
done
np=$(render "${opts[@]}" --show-only templates/networkpolicy.yaml)
for want in 'port: 8443' 'port: 8080' 'port: 9443' 'port: 6443' 'team: ci' '10.0.0.5/32' '192.0.2.7/32' 'port: 5000'; do
	grep -qF -- "$want" <<<"$np" || fail "network policy lacks: $want"
done

signing=$(render --show-only templates/upload-signing-secret.yaml)
grep -qE '^  key: .{44,}' <<<"$signing" || fail "the upload signing Secret has no key of at least 32 bytes"
grep -qF -- '--upload-signing-secret=registrygate-upload-signing' <<<"$default" || fail "the proxy is not given the signing Secret"
grep -qF 'name: registrygate-upload-signing' <<<"$(render --set uploadSigning.existingSecret=mine)" &&
	fail "the chart creates a signing Secret when uploadSigning.existingSecret is set"
grep -qF -- '--upload-signing-secret=mine' <<<"$(render --set uploadSigning.existingSecret=mine)" ||
	fail "the proxy is not given uploadSigning.existingSecret"
echo "chart-lint: passed"
