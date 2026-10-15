#!/usr/bin/env bash
#
# Development-only helper: make the dashboard reach a Keycloak (or other IdP)
# that is exposed only on a localhost hostname, without the socat sidecar.
#
# It does two things:
#   1. Adds a port to the in-cluster ingress (Traefik) Service so the Pod can
#      reach the issuer at the same host:port the browser uses.
#   2. Points the dashboard hostAliases at that Service ClusterIP (disabling the
#      oidc-loopback sidecar) through a Helm upgrade.
#
# The OIDC issuer and the dashboard publicURL are unchanged, so no Keycloak
# client or redirect URI needs to change.
#
# Keycloak and the ingress controller are installed separately from this
# project's Helm chart; this script only updates their Service and the chart.

set -Eeuo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-releaseyard-a}"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"
TRAEFIK_NAMESPACE="${TRAEFIK_NAMESPACE:-traefik}"
TRAEFIK_SERVICE="${TRAEFIK_SERVICE:-traefik}"
TRAEFIK_TARGET_PORT_NAME="${TRAEFIK_TARGET_PORT_NAME:-web}"
LOOPBACK_PORT="${LOOPBACK_PORT:-18080}"
RELEASE_NAME="${RELEASE_NAME:-idp}"
RELEASE_NAMESPACE="${RELEASE_NAMESPACE:-idp-system}"
CHART_DIR="${CHART_DIR:-dist/chart}"
DASHBOARD_DEPLOYMENT="releaseyard-dashboard"
ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-180s}"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"

on_error() {
	local status=$?
	printf 'OIDC loopback setup failed at line %s (exit %s)\n' \
		"${BASH_LINENO[0]:-unknown}" "$status" >&2
	exit "$status"
}
trap on_error ERR

for tool in kubectl helm; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		printf 'Required command not found: %s\n' "$tool" >&2
		exit 1
	fi
done

if [[ "$CHART_DIR" != /* ]]; then
	CHART_DIR="$REPO_ROOT/$CHART_DIR"
fi
if [[ ! -f "$CHART_DIR/Chart.yaml" ]]; then
	printf 'Helm chart not found at %s\n' "$CHART_DIR" >&2
	exit 1
fi

printf 'Checking ingress Service %s/%s on context %s\n' "$TRAEFIK_NAMESPACE" "$TRAEFIK_SERVICE" "$KUBE_CONTEXT"
if ! kubectl --context "$KUBE_CONTEXT" -n "$TRAEFIK_NAMESPACE" get service "$TRAEFIK_SERVICE" >/dev/null 2>&1; then
	printf 'Service %s/%s not found; is the ingress controller installed?\n' "$TRAEFIK_NAMESPACE" "$TRAEFIK_SERVICE" >&2
	exit 1
fi

existing_ports="$(kubectl --context "$KUBE_CONTEXT" -n "$TRAEFIK_NAMESPACE" get service "$TRAEFIK_SERVICE" \
	-o jsonpath='{range .spec.ports[*]}{.port}{"\n"}{end}')"

if grep -qx "$LOOPBACK_PORT" <<<"$existing_ports"; then
	printf 'Service already exposes port %s; leaving it unchanged\n' "$LOOPBACK_PORT"
else
	printf 'Adding port %s -> %s to Service %s/%s\n' "$LOOPBACK_PORT" "$TRAEFIK_TARGET_PORT_NAME" "$TRAEFIK_NAMESPACE" "$TRAEFIK_SERVICE"
	kubectl --context "$KUBE_CONTEXT" -n "$TRAEFIK_NAMESPACE" patch service "$TRAEFIK_SERVICE" --type=json \
		-p "[{\"op\":\"add\",\"path\":\"/spec/ports/-\",\"value\":{\"name\":\"oidc-loopback\",\"port\":$LOOPBACK_PORT,\"targetPort\":\"$TRAEFIK_TARGET_PORT_NAME\",\"protocol\":\"TCP\"}}]"
fi

traefik_ip="$(kubectl --context "$KUBE_CONTEXT" -n "$TRAEFIK_NAMESPACE" get service "$TRAEFIK_SERVICE" -o jsonpath='{.spec.clusterIP}')"
if [[ -z "$traefik_ip" || "$traefik_ip" == "None" ]]; then
	printf 'Could not resolve a ClusterIP for %s/%s\n' "$TRAEFIK_NAMESPACE" "$TRAEFIK_SERVICE" >&2
	exit 1
fi
printf 'Ingress ClusterIP: %s\n' "$traefik_ip"

printf 'Upgrading release %s/%s (hostAliasIP=%s, sidecar disabled)\n' "$RELEASE_NAMESPACE" "$RELEASE_NAME" "$traefik_ip"
helm upgrade "$RELEASE_NAME" "$CHART_DIR" \
	--namespace "$RELEASE_NAMESPACE" \
	--reuse-values \
	--set "dashboard.oidcLoopback.enabled=true" \
	--set "dashboard.oidcLoopback.hostAliasIP=$traefik_ip" \
	--set "dashboard.oidcLoopback.sidecar.enabled=false"

kubectl --context "$KUBE_CONTEXT" -n "$RELEASE_NAMESPACE" rollout status "deployment/$DASHBOARD_DEPLOYMENT" --timeout="$ROLLOUT_TIMEOUT"

printf '\nDone. The dashboard now reaches the IdP through %s:%s without the sidecar.\n' \
	"$TRAEFIK_SERVICE" "$LOOPBACK_PORT"
printf 'Keep the issuer hostname and port aligned, for example http://keycloak.localhost:%s/realms/<realm>.\n' "$LOOPBACK_PORT"
