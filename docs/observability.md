# Observability

## Application detail

The Application detail page reads live data from the target workload cluster.
Access is allowed only when the signed-in user may read the Application on the
management cluster.

### Resources

The **Resources** tab shows the resources recorded in the Application status:

- API version, kind, namespace, and name;
- live YAML with status for supported resources;
- Secret data is never returned.

### Logs

The **Observability** tab can tail logs from a Pod that belongs to the release.
The log request is bounded by tail lines and response size.

### Metrics

Per-Pod CPU and memory come from the target cluster's `metrics.k8s.io` API. This
requires `metrics-server` on the workload cluster.

```bash
bash hack/setup-managed-metrics.sh
```

The script installs metrics-server on the Kind workload clusters with
`--kubelet-insecure-tls`. That flag is for Kind only; do not use it in
production.

If metrics-server is missing, the dashboard reports that metrics are
unavailable instead of failing the whole page.

### Events

The **Activity** tab reads Kubernetes Events for the Application and shows
warnings, sources, and the latest deployment activity. Kubernetes event
retention is cluster-defined and usually short.

## Cloud resources

Cloud runtime observability is not implemented yet. The Cloud detail page shows
Crossplane status, outputs, and composed resources, but not provider metrics or
logs. CPU/memory for an EC2 instance would require a CloudWatch adapter; memory
also requires the CloudWatch Agent.

## Controller metrics

The manager exposes Prometheus metrics when `metrics.enabled=true`:

- default port: `8443`;
- secure by default (`metrics.secure=true`);
- optional ServiceMonitor with `prometheus.enabled=true` when the
  prometheus-operator CRDs are installed.

The metrics endpoint uses the controller-runtime metrics server. Configure
scraping and authentication according to your monitoring stack.

## Dashboard health

The dashboard exposes `GET /healthz` for liveness and readiness. The controller
manager exposes `/healthz` and `/readyz` on its health probe port (`8081` by
default).

## Useful commands

```bash
kubectl -n idp-system get pods
kubectl -n idp-system logs deploy/releaseyard-dashboard
kubectl -n idp-system logs deploy/idp-controller-manager -c manager
kubectl get applications.platform.releaseyard.io -A
kubectl get resources.platform.releaseyard.io -A
```

## Related docs

- [Dashboard](dashboard.md)
- [Applications](applications.md)
- [Troubleshooting](troubleshooting.md)
