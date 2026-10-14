#!/usr/bin/env bash

set -Eeuo pipefail

CLUSTER_NAME="releaseyard-a"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"
NAMESPACE="idp-system"
DEPLOYMENT="releaseyard-dashboard"
DASHBOARD_CONTAINER="dashboard"
ROLLOUT_TIMEOUT="180s"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"

on_error() {
	local status=$?
	printf 'Dashboard deployment failed at line %s (exit %s); no automatic rollback was performed\n' \
		"${BASH_LINENO[0]:-unknown}" "$status" >&2
	exit "$status"
}
trap on_error ERR

for tool in docker kind kubectl; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		printf 'Required command not found: %s\n' "$tool" >&2
		exit 1
	fi
done

if [[ ! -f "$REPO_ROOT/Dockerfile.dashboard" ]]; then
	printf 'Dashboard Dockerfile not found: %s\n' "$REPO_ROOT/Dockerfile.dashboard" >&2
	exit 1
fi

# Preflight the exact live target so a typo or wrong context cannot cause an
# image load or rollout against a different deployment.
container_names="$(kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" get deployment "$DEPLOYMENT" \
	-o 'jsonpath={.spec.template.spec.containers[*].name}')" || {
	printf 'Could not read Deployment %s/%s using context %s\n' "$NAMESPACE" "$DEPLOYMENT" "$KUBE_CONTEXT" >&2
	exit 1
}

if [[ " $container_names " != *" $DASHBOARD_CONTAINER "* ]]; then
	printf 'Deployment %s/%s is missing expected container %s (found: %s); refusing to continue\n' \
		"$NAMESPACE" "$DEPLOYMENT" "$DASHBOARD_CONTAINER" "${container_names:-none}" >&2
	exit 1
fi

tag="$(date -u +%Y%m%dT%H%M%S%NZ)-$$"
image="releaseyard-dashboard:${tag}"

printf 'Building dashboard image %s\n' "$image"
docker build --file "$REPO_ROOT/Dockerfile.dashboard" --tag "$image" "$REPO_ROOT"

printf 'Loading image into Kind cluster %s\n' "$CLUSTER_NAME"
kind load docker-image "$image" --name "$CLUSTER_NAME"

printf 'Updating only container %s in Deployment %s/%s\n' "$DASHBOARD_CONTAINER" "$NAMESPACE" "$DEPLOYMENT"
kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" set image "deployment/$DEPLOYMENT" "$DASHBOARD_CONTAINER=$image"

kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" rollout status "deployment/$DEPLOYMENT" --timeout="$ROLLOUT_TIMEOUT"
