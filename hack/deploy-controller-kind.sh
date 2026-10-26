#!/usr/bin/env bash

set -euo pipefail

CLUSTER_NAME="releaseyard-a"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"
NAMESPACE="idp-system"
DEPLOYMENT="releaseyard-controller-manager"
MANAGER_CONTAINER="manager"
BRIDGE_CONTAINER="kind-loopback-bridge"
ROLLOUT_TIMEOUT="180s"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"

for tool in docker kind kubectl; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		printf 'Required command not found: %s\n' "$tool" >&2
		exit 1
	fi
done

if [[ ! -f "$REPO_ROOT/Dockerfile" ]]; then
	printf 'Manager Dockerfile not found: %s\n' "$REPO_ROOT/Dockerfile" >&2
	exit 1
fi

# Check the live target before building so a typo or wrong context cannot cause
# an image load or rollout against a different deployment.
container_names="$(kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" get deployment "$DEPLOYMENT" \
	-o 'jsonpath={.spec.template.spec.containers[*].name}')" || {
	printf 'Could not read Deployment %s/%s using context %s\n' "$NAMESPACE" "$DEPLOYMENT" "$KUBE_CONTEXT" >&2
	exit 1
}

has_container() {
	local expected="$1"
	local name
	local deployment_containers=()
	read -r -a deployment_containers <<< "$container_names"
	for name in "${deployment_containers[@]}"; do
		if [[ "$name" == "$expected" ]]; then
			return 0
		fi
	done
	return 1
}

if ! has_container "$MANAGER_CONTAINER" || ! has_container "$BRIDGE_CONTAINER"; then
	printf 'Deployment %s/%s does not contain the expected %s and %s containers (found: %s); refusing to continue\n' \
		"$NAMESPACE" "$DEPLOYMENT" "$MANAGER_CONTAINER" "$BRIDGE_CONTAINER" "${container_names:-none}" >&2
	exit 1
fi

tag="$(date -u +%Y%m%dT%H%M%S%NZ)-$$"
image="releaseyard-controller:${tag}"

printf 'Building manager image %s\n' "$image"
docker build --file "$REPO_ROOT/Dockerfile" --tag "$image" "$REPO_ROOT"

printf 'Loading image into Kind cluster %s\n' "$CLUSTER_NAME"
kind load docker-image "$image" --name "$CLUSTER_NAME"

printf 'Updating only container %s in Deployment %s/%s\n' "$MANAGER_CONTAINER" "$NAMESPACE" "$DEPLOYMENT"
kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" set image "deployment/$DEPLOYMENT" "$MANAGER_CONTAINER=$image"

kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" rollout status "deployment/$DEPLOYMENT" --timeout="$ROLLOUT_TIMEOUT"
