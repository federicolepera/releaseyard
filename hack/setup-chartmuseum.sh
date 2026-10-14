#!/usr/bin/env bash

set -euo pipefail
umask 077

MANAGEMENT_CLUSTER_NAME="releaseyard-a"
CHARTMUSEUM_NAMESPACE="chartmuseum"
CHARTMUSEUM_RELEASE="chartmuseum"
CHART_NAME="releaseyard-demo"
CHART_VERSION="0.2.0"
PORT_FORWARD_PORT="18080"
CHARTMUSEUM_URL="http://127.0.0.1:${PORT_FORWARD_PORT}"

for tool in helm kubectl curl python3 sha256sum; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		printf 'Required command not found: %s\n' "$tool" >&2
		exit 1
	fi
done

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
LOCAL_DIR="$REPO_ROOT/.local"
CHARTS_DIR="$LOCAL_DIR/charts"
CHART_SOURCE="$REPO_ROOT/hack/charts/$CHART_NAME"

if [[ -L "$LOCAL_DIR" ]]; then
	printf 'Refusing symlinked local output directory: %s\n' "$LOCAL_DIR" >&2
	exit 1
fi
mkdir -p -- "$CHARTS_DIR"
if [[ -L "$CHARTS_DIR" || ! -d "$CHARTS_DIR" ]]; then
	printf 'Chart package output path is not a directory: %s\n' "$CHARTS_DIR" >&2
	exit 1
fi
chmod 700 -- "$LOCAL_DIR" "$CHARTS_DIR"

temporary_dir="$(mktemp -d "$LOCAL_DIR/.chartmuseum-setup.XXXXXX")"
chmod 700 -- "$temporary_dir"
port_forward_pid=""
cleanup() {
	if [[ -n "$port_forward_pid" ]] && kill -0 "$port_forward_pid" 2>/dev/null; then
		kill "$port_forward_pid" 2>/dev/null || true
		wait "$port_forward_pid" 2>/dev/null || true
	fi
	rm -rf -- "$temporary_dir"
}
trap cleanup EXIT

helm upgrade --install "$CHARTMUSEUM_RELEASE" chartmuseum \
	--repo https://chartmuseum.github.io/charts \
	--version 3.10.4 \
	--namespace "$CHARTMUSEUM_NAMESPACE" \
	--create-namespace \
	--set env.open.DISABLE_API=false \
	--set persistence.enabled=true \
	--wait \
	--timeout 5m \
	--kube-context "kind-${MANAGEMENT_CLUSTER_NAME}"

helm package "$CHART_SOURCE" --destination "$temporary_dir" >/dev/null
packaged_chart="$temporary_dir/${CHART_NAME}-${CHART_VERSION}.tgz"
CHART_ARCHIVE="$CHARTS_DIR/${CHART_NAME}-${CHART_VERSION}.tgz"

if [[ ! -f "$packaged_chart" || -L "$packaged_chart" ]]; then
	printf 'Expected packaged chart not found: %s\n' "$packaged_chart" >&2
	exit 1
fi
if [[ -L "$CHART_ARCHIVE" ]]; then
	printf 'Refusing symlinked local chart package: %s\n' "$CHART_ARCHIVE" >&2
	exit 1
elif [[ -e "$CHART_ARCHIVE" ]]; then
	if [[ ! -f "$CHART_ARCHIVE" ]] ||
		[[ "$(sha256sum "$CHART_ARCHIVE" | cut -d ' ' -f 1)" != "$(sha256sum "$packaged_chart" | cut -d ' ' -f 1)" ]]; then
		printf 'Local chart package already exists with different bytes; refusing overwrite: %s\n' "$CHART_ARCHIVE" >&2
		printf 'Review the chart version and package before removing only this file.\n' >&2
		exit 1
	fi
else
	if ! ln -- "$packaged_chart" "$CHART_ARCHIVE"; then
		printf 'Could not install chart package without overwriting an existing path: %s\n' "$CHART_ARCHIVE" >&2
		exit 1
	fi
fi

if ! python3 - "$PORT_FORWARD_PORT" <<'PY'
import socket
import sys

with socket.socket() as sock:
    try:
        sock.bind(("127.0.0.1", int(sys.argv[1])))
    except OSError:
        sys.exit(1)
PY
then
	printf 'Local port %s is already in use; refusing to contact an unknown service\n' "$PORT_FORWARD_PORT" >&2
	exit 1
fi

port_forward_log="$temporary_dir/port-forward.log"
kubectl --context "kind-${MANAGEMENT_CLUSTER_NAME}" \
	-n "$CHARTMUSEUM_NAMESPACE" \
	port-forward "service/${CHARTMUSEUM_RELEASE}" \
	"${PORT_FORWARD_PORT}:8080" \
	--address 127.0.0.1 >"$port_forward_log" 2>&1 &
port_forward_pid=$!

forward_ready=false
for _ in {1..50}; do
	if ! kill -0 "$port_forward_pid" 2>/dev/null; then
		break
	fi
	if grep -Fq "Forwarding from 127.0.0.1:${PORT_FORWARD_PORT} -> 8080" "$port_forward_log"; then
		forward_ready=true
		break
	fi
	sleep 0.2
done
if [[ "$forward_ready" != true ]]; then
	printf 'Could not establish the temporary ChartMuseum port-forward\n' >&2
	exit 1
fi

charts_response="$temporary_dir/charts-response"
api_ready=false
for _ in {1..30}; do
	if curl --silent --fail --max-time 2 --noproxy '*' -o "$charts_response" "$CHARTMUSEUM_URL/api/charts"; then
		api_ready=true
		break
	fi
	sleep 1
done
if [[ "$api_ready" != true ]]; then
	printf 'ChartMuseum API did not become ready through the port-forward\n' >&2
	exit 1
fi

remote_archive="$temporary_dir/remote-chart.tgz"
remote_status="$(curl --silent --show-error --max-time 10 \
	--noproxy '*' \
	-o "$remote_archive" -w '%{http_code}' \
	"$CHARTMUSEUM_URL/charts/${CHART_NAME}-${CHART_VERSION}.tgz")" || {
	printf 'Could not check for an existing ChartMuseum chart version\n' >&2
	exit 1
}

case "$remote_status" in
	200)
		local_digest="$(sha256sum "$CHART_ARCHIVE" | cut -d ' ' -f 1)"
		remote_digest="$(sha256sum "$remote_archive" | cut -d ' ' -f 1)"
		if [[ "$local_digest" != "$remote_digest" ]]; then
			printf 'Chart %s version %s already exists with different bytes; refusing overwrite. Bump the chart version to publish new content.\n' "$CHART_NAME" "$CHART_VERSION" >&2
			exit 1
		fi
		printf 'ChartMuseum already contains identical %s version %s; skipping upload\n' "$CHART_NAME" "$CHART_VERSION"
		;;
	404)
		upload_response="$temporary_dir/upload-response"
		upload_status="$(curl --silent --show-error --max-time 30 \
			--noproxy '*' \
			-o "$upload_response" -w '%{http_code}' \
			-X POST -H 'Content-Type: application/gzip' \
			--data-binary "@$CHART_ARCHIVE" "$CHARTMUSEUM_URL/api/charts")" || {
			printf 'Could not upload chart archive to ChartMuseum\n' >&2
			exit 1
		}
		if [[ ! "$upload_status" =~ ^2[0-9][0-9]$ ]]; then
			printf 'ChartMuseum chart upload failed with HTTP status %s\n' "$upload_status" >&2
			exit 1
		fi
		;;
	*)
		printf 'ChartMuseum chart lookup returned unexpected HTTP status %s; refusing upload\n' "$remote_status" >&2
		exit 1
		;;
esac

index_file="$temporary_dir/index.yaml"
if ! curl --silent --show-error --fail --max-time 10 \
	--noproxy '*' \
	-o "$index_file" "$CHARTMUSEUM_URL/index.yaml"; then
	printf 'Could not fetch ChartMuseum index.yaml after chart upload/check\n' >&2
	exit 1
fi
if ! python3 - "$index_file" "$CHART_NAME" "$CHART_VERSION" <<'PY'
import pathlib
import sys

lines = pathlib.Path(sys.argv[1]).read_text().splitlines()
name, version = sys.argv[2:]
selected_chart = False
found_version = False
entry = None
for line in lines:
    if line == f"  {name}:":
        selected_chart = True
        continue
    if selected_chart and line.startswith("  ") and not line.startswith(("  - ", "    ")):
        break
    if not selected_chart:
        continue
    if line.startswith("  - "):
        if entry == {"name": name, "version": version}:
            found_version = True
            break
        entry = {}
        continue
    if entry is not None and line.startswith("    ") and ":" in line:
        key, value = line.strip().split(":", 1)
        if key in ("name", "version"):
            entry[key] = value.strip().strip("\"'")
if entry == {"name": name, "version": version}:
    found_version = True
if not found_version:
    sys.exit(1)
PY
then
	printf 'ChartMuseum index does not list %s version %s\n' "$CHART_NAME" "$CHART_VERSION" >&2
	exit 1
fi

verified_archive="$temporary_dir/verified-chart.tgz"
verified_status="$(curl --silent --show-error --max-time 10 \
	--noproxy '*' \
	-o "$verified_archive" -w '%{http_code}' \
	"$CHARTMUSEUM_URL/charts/${CHART_NAME}-${CHART_VERSION}.tgz")" || {
	printf 'Could not download the chart package from ChartMuseum after upload/check\n' >&2
	exit 1
}
if [[ "$verified_status" != 200 ]] ||
	[[ "$(sha256sum "$CHART_ARCHIVE" | cut -d ' ' -f 1)" != "$(sha256sum "$verified_archive" | cut -d ' ' -f 1)" ]]; then
	printf 'ChartMuseum package verification failed for %s version %s\n' "$CHART_NAME" "$CHART_VERSION" >&2
	exit 1
fi

printf 'ChartMuseum is ready in management cluster %s; published %s version %s\n' "$MANAGEMENT_CLUSTER_NAME" "$CHART_NAME" "$CHART_VERSION"
printf 'For local debugging, run this in a separate terminal (the setup script does not leave a port-forward running):\n'
printf 'kubectl --context kind-%s -n %s port-forward service/%s %s:8080 --address 127.0.0.1\n' \
	"$MANAGEMENT_CLUSTER_NAME" "$CHARTMUSEUM_NAMESPACE" "$CHARTMUSEUM_RELEASE" "$PORT_FORWARD_PORT"
