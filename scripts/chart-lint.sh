#!/usr/bin/env bash
# Lints the Helm chart without a cluster (`make chart-lint`, part of
# `make lint`): helm lint, then renders with the required values and checks the
# properties the chart promises (hardening, resources, config rollout, Service IP).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

chart=deploy/charts/ghcr-proxy
required=(--set pat.existingSecret=pat --set tls.existingSecret=tls)

helm lint --strict "$chart" "${required[@]}"

render() { helm template ghcr-proxy "$chart" --namespace ghcr-proxy "${required[@]}" "$@"; }
fail() { echo "chart-lint: $1" >&2; exit 1; }

default=$(render)
for want in 'runAsNonRoot: true' 'readOnlyRootFilesystem: true' 'allowPrivilegeEscalation: false' '- ALL' 'checksum/config:' 'POD_NAMESPACE'; do
	grep -qF -- "$want" <<<"$default" || fail "default render lacks: $want"
done
# Memory request equals limit, and there is no CPU limit.
mem=$(grep -E '^ +memory:' <<<"$default" | sort -u | wc -l)
[ "$mem" -eq 1 ] || fail "memory request and limit differ"
helm template ghcr-proxy "$chart" "${required[@]}" --show-only templates/deployment.yaml |
	awk '/limits:/{f=1;next} f&&/cpu:/{exit 1} /requests:/{f=0}' || fail "a CPU limit is set"

sum() { render "$@" --show-only templates/deployment.yaml | grep 'checksum/config'; }
[ "$(sum)" != "$(sum --set authentication.audience=other)" ] || fail "config change does not change the checksum"

render --set service.clusterIP=10.96.0.50 --show-only templates/service.yaml | grep -qF 'clusterIP: "10.96.0.50"' ||
	fail "service.clusterIP is not used"

opts=(--set podDisruptionBudget.enabled=true --set networkPolicy.enabled=true --set serviceMonitor.enabled=true
	--set 'networkPolicy.apiServerCIDRs={10.0.0.5/32}' --set networkPolicy.clientNamespaceSelector.matchLabels.team=ci)
for kind in PodDisruptionBudget NetworkPolicy ServiceMonitor; do
	grep -qF "kind: $kind" <<<"$(render)" && fail "$kind rendered by default"
	grep -qF "kind: $kind" <<<"$(render "${opts[@]}")" || fail "$kind not rendered when enabled"
done
np=$(render "${opts[@]}" --show-only templates/networkpolicy.yaml)
for want in 'port: 8443' 'port: 8080' 'port: 9443' 'port: 6443' 'team: ci' '10.0.0.5/32'; do
	grep -qF -- "$want" <<<"$np" || fail "network policy lacks: $want"
done

if helm template ghcr-proxy "$chart" >/dev/null 2>&1; then
	fail "render without the Secret names must fail"
fi
echo "chart-lint: passed"
