#!/usr/bin/env bash
#
# Development-only helper: install metrics-server on the managed Kind clusters so
# the dashboard can show per-Pod CPU/memory in the Observability section. Without
# metrics-server the dashboard simply reports that metrics are unavailable.
#
# Kind kubelets use self-signed serving certificates, so metrics-server is
# started with --kubelet-insecure-tls. Never use that flag in production.

set -Eeuo pipefail

CLUSTERS="${CLUSTERS:-releaseyard-managed-a releaseyard-managed-b}"
MANIFEST_URL="${MANIFEST_URL:-https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml}"

for tool in kubectl; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		printf 'Required command not found: %s\n' "$tool" >&2
		exit 1
	fi
done

for name in $CLUSTERS; do
	context="kind-${name}"
	printf 'Installing metrics-server on %s\n' "$context"

	if ! kubectl --context "$context" get namespace kube-system >/dev/null 2>&1; then
		printf 'Cluster %s is not reachable; skipping\n' "$context" >&2
		continue
	fi

	kubectl --context "$context" apply -f "$MANIFEST_URL"

	# Add --kubelet-insecure-tls only if it is not already present.
	args="$(kubectl --context "$context" -n kube-system get deployment metrics-server -o jsonpath='{.spec.template.spec.containers[0].args}')"
	if [[ "$args" != *"--kubelet-insecure-tls"* ]]; then
		kubectl --context "$context" -n kube-system patch deployment metrics-server --type=json \
			-p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
	fi

	kubectl --context "$context" -n kube-system rollout status deployment/metrics-server --timeout=180s
done

printf '\nDone. metrics-server is available on: %s\n' "$CLUSTERS"
