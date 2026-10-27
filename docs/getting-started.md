# Getting started

## Requirements

- Docker
- `kubectl`
- Kind
- Go 1.26+
- Helm 3
- Python 3

The local demo uses three Kind clusters. It is intended for development, not
production. This repository no longer provisions a chart repository.

## Local Kind demo

### 1. Create the clusters

```bash
bash hack/setup-kind-workloads.sh
```

This creates or reuses:

- `releaseyard-a` — management cluster;
- `releaseyard-managed-a` and `releaseyard-managed-b` — workload clusters.

Workload kubeconfigs are written to `.local/kubeconfigs/` with mode `0600`.

### 2. Build and load the images

```bash
docker build -t releaseyard-controller:dev .
docker build -f Dockerfile.dashboard -t releaseyard-dashboard:dev .
kind load docker-image releaseyard-controller:dev --name releaseyard-a
kind load docker-image releaseyard-dashboard:dev --name releaseyard-a
```

### 3. Install the controller

```bash
helm upgrade --install idp dist/chart \
  -n idp-system --create-namespace \
  --set manager.image.repository=releaseyard-controller \
  --set manager.image.tag=dev \
  --set manager.allowedChartHosts=ghcr.io
```

`allowedChartHosts` is the host allowlist used by the chart fetcher. For the
public demo chart, set it to `ghcr.io`. The sample definition uses
`oci://ghcr.io/federicolepera/helm-charts`.

### 4. Deploy the dashboard

```bash
kubectl --context kind-releaseyard-a apply -k config/dashboard
```

The local kustomization expects the `releaseyard-dashboard:dev` image loaded
above and uses `http://127.0.0.1:8080` as its public URL.

### 5. Register the workload clusters

```bash
kubectl --context kind-releaseyard-a -n idp-system create secret generic releaseyard-managed-a-kubeconfig \
  --from-file=kubeconfig=.local/kubeconfigs/releaseyard-managed-a.kubeconfig
kubectl --context kind-releaseyard-a -n idp-system create secret generic releaseyard-managed-b-kubeconfig \
  --from-file=kubeconfig=.local/kubeconfigs/releaseyard-managed-b.kubeconfig
```

The Secret must live in the controller's credentials namespace (`idp-system` by
default) and contain the kubeconfig under the `kubeconfig` key.

### 6. Apply the samples

The sample definition uses the public GHCR OCI chart:

```bash
kubectl --context kind-releaseyard-a apply -f config/samples/platform_v1alpha1_cluster.yaml
kubectl --context kind-releaseyard-a apply -f config/samples/platform_v1alpha1_applicationdefinition.yaml
kubectl --context kind-releaseyard-a apply -f config/samples/platform_v1alpha1_application.yaml
```

### 7. Open the dashboard

```bash
kubectl --context kind-releaseyard-a -n idp-system port-forward service/releaseyard-dashboard 8080:8080
```

Open <http://127.0.0.1:8080> and sign in as `admin`.

```bash
kubectl --context kind-releaseyard-a -n idp-system get secret releaseyard-dashboard-local-admin \
  -o jsonpath='{.data.initial-password}' | base64 -d
```

The local administrator is created once and is never overwritten on upgrade.
OIDC can be configured later from **Administration → Identity provider**.

## Helm chart

A minimal controller install:

```bash
helm upgrade --install idp dist/chart -n idp-system --create-namespace \
  --set manager.image.repository=<registry>/releaseyard-controller \
  --set manager.image.tag=<tag> \
  --set manager.allowedChartHosts=<chart-host>
```

Enable the dashboard:

```bash
helm upgrade --install idp dist/chart -n idp-system --create-namespace \
  --set manager.image.repository=<registry>/releaseyard-controller \
  --set manager.image.tag=<tag> \
  --set manager.allowedChartHosts=<chart-host> \
  --set dashboard.enabled=true \
  --set dashboard.image.repository=<registry>/releaseyard-dashboard \
  --set dashboard.image.tag=<tag> \
  --set dashboard.publicURL=https://dashboard.example.com
```

`dashboard.publicURL` is required when the dashboard is enabled. HTTP is
accepted only for `localhost` hosts; use HTTPS elsewhere.

With the default chart naming:

- dashboard Service: `<release>-dashboard`;
- local administrator Secret: `<release>-dashboard-local-admin`;
- dashboard image pull: `dashboard.image.repository` + `dashboard.image.tag`.

Read the password with:

```bash
kubectl get secret <release>-dashboard-local-admin -n <namespace> \
  -o jsonpath='{.data.initial-password}' | base64 -d
```

## YAML bundle

```bash
make build-installer IMG=<registry>/releaseyard-controller:<tag>
kubectl apply -f dist/install.yaml
```

The generated bundle contains the CRDs and the controller. It does not include
the optional dashboard.

## Optional cloud demo

Cloud provisioning requires a separately installed Crossplane. For the local
S3 flow:

```bash
bash hack/setup-crossplane-localstack.sh
kubectl apply -f examples/cloud-bucket/03-cloudtarget.yaml
kubectl apply -f examples/cloud-bucket/04-resourcedefinition.yaml
kubectl apply -f examples/cloud-bucket/05-resource.yaml
```

The setup script installs LocalStack, the S3 provider, the fake credentials
Secret, and the `ProviderConfig` (manifests `00`–`02`).

The dashboard detects Crossplane at startup. If you install Crossplane after the
dashboard is already running, restart the dashboard Deployment.

See [Cloud resources](cloud-resources.md).

## Cleanup

```bash
kind delete cluster --name releaseyard-a
kind delete cluster --name releaseyard-managed-a
kind delete cluster --name releaseyard-managed-b
```

If you installed Crossplane or LocalStack separately, remove those installations
with their own Helm releases. Deleting the Kind clusters removes the local
demo, but it does not delete cloud resources created in a real provider account.

## Next steps

- [Architecture](architecture.md)
- [Applications](applications.md)
- [Cloud resources](cloud-resources.md)
- [Access control](access-control.md)
- [Troubleshooting](troubleshooting.md)
