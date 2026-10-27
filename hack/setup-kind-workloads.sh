#!/usr/bin/env bash

set -euo pipefail
umask 077

MANAGEMENT_CLUSTER_NAME="releaseyard-a"
WORKLOAD_A_NAME="${RELEASEYARD_WORKLOAD_A_NAME:-releaseyard-managed-a}"
WORKLOAD_B_NAME="${RELEASEYARD_WORKLOAD_B_NAME:-releaseyard-managed-b}"
KUBECONFIG_DIR="${RELEASEYARD_KUBECONFIG_DIR:-}"

INOTIFY_LIMIT_FILE="/proc/sys/fs/inotify/max_user_instances"
if [[ -r "$INOTIFY_LIMIT_FILE" ]]; then
	inotify_limit="$(<"$INOTIFY_LIMIT_FILE")"
	if [[ "$inotify_limit" =~ ^[0-9]+$ ]] && (( 10#$inotify_limit < 512 )); then
		printf 'fs.inotify.max_user_instances is %s; in WSL run `sudo sysctl -w fs.inotify.max_user_instances=512` in your WSL terminal, then rerun this script.\n' "$inotify_limit" >&2
		exit 1
	fi
fi

for tool in kind docker kubectl python3; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		printf 'Required command not found: %s\n' "$tool" >&2
		exit 1
	fi
done

for name in "$MANAGEMENT_CLUSTER_NAME" "$WORKLOAD_A_NAME" "$WORKLOAD_B_NAME"; do
	if [[ ! "$name" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || (( ${#name} > 63 )); then
		printf 'Invalid Kind cluster name: %s\n' "$name" >&2
		exit 1
	fi
done

if [[ "$MANAGEMENT_CLUSTER_NAME" == "$WORKLOAD_A_NAME" \
	|| "$MANAGEMENT_CLUSTER_NAME" == "$WORKLOAD_B_NAME" \
	|| "$WORKLOAD_A_NAME" == "$WORKLOAD_B_NAME" ]]; then
	printf 'Management and workload Kind cluster names must be unique\n' >&2
	exit 1
fi

if ! docker info >/dev/null 2>&1; then
	printf 'Docker is unavailable; start the Docker daemon and retry\n' >&2
	exit 1
fi

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
if [[ -z "$KUBECONFIG_DIR" ]]; then
	KUBECONFIG_DIR="$REPO_ROOT/.local/kubeconfigs"
fi

if [[ -L "$KUBECONFIG_DIR" ]]; then
	printf 'Refusing symlinked kubeconfig output directory: %s\n' "$KUBECONFIG_DIR" >&2
	exit 1
fi
mkdir -p -- "$KUBECONFIG_DIR"
if [[ -L "$KUBECONFIG_DIR" || ! -d "$KUBECONFIG_DIR" ]]; then
	printf 'Kubeconfig output path is not a directory: %s\n' "$KUBECONFIG_DIR" >&2
	exit 1
fi
chmod 700 -- "$KUBECONFIG_DIR"

temporary_files=()
cleanup() {
	local file
	for file in "${temporary_files[@]}"; do
		rm -f -- "$file"
	done
}
trap cleanup EXIT

cluster_exists() {
	local requested="$1"
	local cluster
	local clusters
	clusters="$(kind get clusters)" || return 2
	while IFS= read -r cluster; do
		if [[ "$cluster" == "$requested" ]]; then
			return 0
		fi
	done <<< "$clusters"
	return 1
}

ensure_cluster() {
	local name="$1"
	if cluster_exists "$name"; then
		printf 'Kind cluster already exists; leaving it in place: %s\n' "$name"
		return
	else
		local status=$?
		if (( status != 1 )); then
			printf 'Could not list existing Kind clusters\n' >&2
			return "$status"
		fi
	fi
	printf 'Creating Kind cluster on the default shared network: %s\n' "$name"
	kind create cluster --name "$name" --wait 2m
}

validate_kubeconfig() {
	local name="$1"
	local path="$2"
	if ! kubectl config view --raw --kubeconfig "$path" -o json 2>/dev/null |
		python3 -c '
import json
import os
import sys
from urllib.parse import urlsplit

expected_name = sys.argv[1]
try:
    if os.stat(sys.argv[2]).st_mode & 0o777 != 0o600:
        raise ValueError
    config = json.load(sys.stdin)
    contexts = {entry["name"]: entry["context"] for entry in config.get("contexts", [])}
    clusters = {entry["name"]: entry["cluster"] for entry in config.get("clusters", [])}
    users = {entry["name"]: entry["user"] for entry in config.get("users", [])}
    if not contexts or not clusters or not users:
        raise ValueError

    for cluster in clusters.values():
        if (cluster.get("insecure-skip-tls-verify") or cluster.get("insecureSkipTLSVerify")
                or cluster.get("proxy-url") or cluster.get("proxyURL")
                or cluster.get("certificate-authority") or cluster.get("certificateAuthority")):
            raise ValueError
    forbidden_auth = (
        "username", "password", "tokenFile", "token-file",
        "client-certificate", "clientCertificate", "client-key", "clientKey",
        "as", "impersonate", "as-groups", "asGroups", "impersonateGroups",
        "as-user-extra", "asUserExtra", "impersonateUserExtra",
        "as-uid", "asUID", "impersonateUID", "auth-provider", "authProvider", "exec",
    )
    for user in users.values():
        if any(user.get(field) for field in forbidden_auth):
            raise ValueError

    current_name = config.get("current-context")
    current = contexts.get(current_name)
    if not current or current.get("cluster") not in clusters or current.get("user") not in users:
        raise ValueError
    cluster = clusters[current["cluster"]]
    user = users[current["user"]]
    server = cluster.get("server", "")
    parsed_server = urlsplit(server)
    if (server != f"https://{expected_name}-control-plane:6443"
            or parsed_server.scheme != "https" or not parsed_server.hostname
            or parsed_server.username or parsed_server.password
            or parsed_server.query or parsed_server.fragment):
        raise ValueError
    if not cluster.get("certificate-authority-data") or cluster.get("insecure-skip-tls-verify", False):
        raise ValueError
    if not user.get("client-certificate-data") or not user.get("client-key-data") or user.get("token"):
        raise ValueError
    for name, other_user in users.items():
        if name != current["user"] and any(other_user.get(field) for field in (
                "token", "client-certificate-data", "client-key-data")):
            raise ValueError
except Exception:
    sys.exit(1)
' "$name" "$path"; then
		printf 'Kubeconfig validation failed for cluster %s (details withheld)\n' "$name" >&2
		return 1
	fi
}

write_kubeconfig() {
	local name="$1"
	local output="$KUBECONFIG_DIR/$name.kubeconfig"
	local temporary

	temporary="$(mktemp "$KUBECONFIG_DIR/.${name}.kubeconfig.XXXXXX")"
	temporary_files+=("$temporary")
	if ! kind get kubeconfig --name "$name" --internal >"$temporary"; then
		printf 'Could not export kubeconfig for cluster %s\n' "$name" >&2
		return 1
	fi
	chmod 600 -- "$temporary"
	if ! validate_kubeconfig "$name" "$temporary"; then
		return 1
	fi
	if [[ -L "$output" ]]; then
		printf 'Refusing symlinked kubeconfig output: %s\n' "$output" >&2
		return 1
	fi
	if [[ -e "$output" ]]; then
		if [[ ! -f "$output" ]] || ! validate_kubeconfig "$name" "$output"; then
			printf 'Existing kubeconfig fails internal-endpoint or credential-safety checks; refusing to overwrite it: %s\n' "$output" >&2
			printf 'After review, remove only this file and rerun to export again; update its management Secret separately.\n' >&2
			return 1
		fi
		if cmp -s "$output" "$temporary"; then
			rm -f -- "$temporary"
			printf 'Existing kubeconfig matches the current Kind export: %s\n' "$output"
			return
		fi
		printf 'Current Kind credentials differ from the existing kubeconfig; refusing to overwrite: %s\n' "$output" >&2
		printf 'After verifying the recreated cluster, remove only this file and rerun; then deliberately replace the matching management Secret.\n' >&2
		return 1
	fi

	# Linking within the output directory installs atomically and refuses to
	# replace a file created by another process after the initial existence check.
	if ! ln "$temporary" "$output"; then
		printf 'Could not install kubeconfig without overwriting an existing path: %s\n' "$output" >&2
		return 1
	fi
	rm -f -- "$temporary"
	printf 'Wrote current Kind kubeconfig with mode 0600: %s\n' "$output"
}

ensure_cluster "$MANAGEMENT_CLUSTER_NAME"
ensure_cluster "$WORKLOAD_A_NAME"
ensure_cluster "$WORKLOAD_B_NAME"
write_kubeconfig "$WORKLOAD_A_NAME"
write_kubeconfig "$WORKLOAD_B_NAME"

printf 'Management and workload clusters are ready. Review the two workload kubeconfig paths before creating management-cluster Secrets.\n'
