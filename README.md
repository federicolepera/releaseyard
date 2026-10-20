<h1 align="center">ReleaseYard</h1>

<h3 align="center">
  <a name="readme-top"></a>
  <img src="img/releaseYard_logo.png" height="250" alt="ReleaseYard">
</h3>

<div align="center">

<p align="center">
  A Kubernetes-native platform that turns a curated Helm catalog into self-service application delivery across clusters, with a real-time dashboard and optional cloud provisioning.
</p>

<div align="center">
  <img src="https://img.shields.io/badge/go-1.26-00ADD8?logo=go&logoColor=white" alt="Go">
  <img src="https://img.shields.io/badge/kubernetes-ready-326CE5?logo=kubernetes&logoColor=white" alt="Kubernetes">
  <img src="https://img.shields.io/badge/helm-chart-0F1689?logo=helm&logoColor=white" alt="Helm Chart">
  <img src="https://img.shields.io/badge/license-Apache%202.0-blue" alt="License">
</div>
</div>

## Table of Contents
- [What is ReleaseYard?](#what-is-releaseyard)
- [How it works](#how-it-works)
- [Dashboard](#dashboard)
- [Application placement](#application-placement)
- [Optional cloud provisioning](#optional-cloud-provisioning)
- [Quickstart](#quickstart)
  - [Local Kind demo](#local-kind-demo)
  - [Helm chart](#helm-chart)
  - [YAML bundle](#yaml-bundle)
- [Configuration](#configuration)
  - [Cluster](#cluster)
  - [ApplicationDefinition](#applicationdefinition)
  - [Application](#application)
  - [Cloud resources](#cloud-resources)
- [Authentication & authorization](#authentication--authorization)
- [Observability](#observability)
- [Development](#development)
- [Contributing](#contributing)
- [Disclaimer](#disclaimer)
- [License](#license)

## What is ReleaseYard?

**ReleaseYard** is a Kubernetes operator **and** a web dashboard that turn a
curated Helm catalog into self-service application deployment across one or
more Kubernetes clusters. A platform team registers target clusters and
publishes reusable templates; a team then picks a template from the catalog and
creates an `Application`. The operator installs, updates, and uninstalls the
matching Helm release on the selected cluster.

It features:

- **Kubernetes-native API**: everything is a CRD on the management cluster, so
  the platform is GitOps-friendly and has no proprietary state store.
- **Curated catalog**: `ApplicationDefinition` pins a chart, its defaults, and
  the *only* value paths an `Application` may override.
- **Multi-cluster delivery**: install on a single `Cluster` or fan out to every
  cluster matching an environment.
- **Safe installs**: chart bytes are fetched and pinned by SHA-256 digest before
  Helm ever sees them, and the manager only talks to allowed chart hosts.
- **Real-time dashboard**: browse the catalog, create and manage Applications,
  inspect live workloads, logs, CPU/memory, and activity.
- **RBAC-driven authorization**: users sign in with OIDC (or a local
  administrator) and the management cluster's Kubernetes RBAC decides what they
  may do.
- **Team isolation**: teams live in `releaseyard-team-<group>` namespaces and
  are granted exactly the definitions and clusters they need.
- **Optional cloud provisioning**: request cloud resources through
  `CloudTarget` / `ResourceDefinition` / `Resource` and let Crossplane create
  them. Crossplane is never bundled with ReleaseYard.

## How it works

```
 ApplicationDefinition ──┐
 Cluster ────────────────┼──► ReleaseYard operator ──► managed clusters (Helm)
 Application ────────────┘

 CloudTarget ────────────┐
 ResourceDefinition ─────┼──► ReleaseYard operator ──► Crossplane managed resources
 Resource ───────────────┘
```

The operator runs on a **management cluster** and reconciles six custom
resources:

| Kind | Scope | Purpose |
|------|-------|---------|
| `Cluster` | Cluster | A target workload cluster, reached through a kubeconfig Secret. |
| `ApplicationDefinition` | Cluster | A pinned Helm chart plus defaults and the typed value surface. |
| `Application` | Namespaced | A request to deploy that chart on one cluster or an environment. |
| `CloudTarget` | Cluster | Where cloud resources should be created (provider, region, credentials). |
| `ResourceDefinition` | Cluster | A golden path for a cloud resource: templates, mappings, and outputs. |
| `Resource` | Namespaced | A team request for a cloud resource, rendered into Crossplane objects. |

For each `Application`, the operator resolves the definition and target, fetches
and verifies the chart, and installs a Helm release named after the team and the
Application. It keeps the release in sync on updates and uninstalls it when the
`Application` is deleted.

## Dashboard

The dashboard is a web UI for platform admins and teams. It includes:

- **Catalog**: browse validated Application Definitions and create Applications.
- **Applications**: manage every Application owned by the signed-in user.
- **Clusters**: see registered target clusters, connectivity, and nodes.
- **Application detail**: four tabs for a running Application.
  - **Overview**: status, placement, release information, and recent activity.
  - **Configuration**: the definition contract, overrides, rendered YAML, and change history.
  - **Observability**: live resources, Pod logs, and Pod CPU/memory usage.
  - **Activity**: Kubernetes events, warnings, and the latest deployment.
- **Administration**: Identity provider, Cluster access, Access definitions, and
  Access applications for the built-in local administrator or authorized OIDC
  groups.

The dashboard starts with **local administrator authentication only**; no
identity provider is required to install it. OIDC can be configured later from
the Administration page, without a restart.

## Application placement

An `Application` can target one cluster or an environment:

- **One cluster**: set `spec.clusterRef`. The Application produces one Helm
  release on that cluster.
- **All clusters in an environment**: set `spec.environment`. ReleaseYard takes
  an immutable snapshot of the matching `Cluster` names and UIDs at creation
  time and produces one Helm release per target.

The release namespace can be selected with `spec.releaseNamespace` when the
definition allows it; otherwise the definition's `spec.release.namespace` is
used. Release names are built from the team namespace token and the Application
name, so different teams can use the same Application name safely.

## Optional cloud provisioning

ReleaseYard can expose cloud resources through the same self-service model. The
integration is intentionally **optional** and **separate**: Crossplane is
installed and operated by the platform team, while ReleaseYard only creates
Managed Resources through the Kubernetes API.

> [!IMPORTANT]
> The ReleaseYard Helm chart never installs Crossplane. Do not add it as a
> subchart or expect the ReleaseYard install to provide it.

The model has three pieces:

- **`CloudTarget`** — *where* to create resources: provider, region, and a
  reference to a namespaced Crossplane `ProviderConfig` with credentials and
  endpoint.
- **`ResourceDefinition`** — *what* to create: one or more managed-resource
  templates, a static `base`, mappings from `values.*` / `target.*` into the
  managed resource, and outputs read back from its status.
- **`Resource`** — the team request: a definition reference, a target
  reference, and the allowed values.

For the local demo, [`examples/cloud-bucket/`](examples/cloud-bucket/) contains
the full sequence:

```text
00-provider.yaml        Crossplane AWS S3 provider
01-secret.yaml          fake LocalStack credentials
02-providerconfig.yaml  credentials + LocalStack endpoint
03-cloudtarget.yaml     ReleaseYard CloudTarget
04-resourcedefinition.yaml  ReleaseYard ResourceDefinition
05-resource.yaml        ReleaseYard Resource -> S3 bucket
```

`hack/setup-crossplane-localstack.sh` wires a Crossplane provider to a local
LocalStack instance. It is a development helper only: LocalStack accepts any
credentials and the provider is configured to skip real AWS checks. Never use
that endpoint or those settings in production.

## Quickstart

Requirements: Docker, `kubectl`, Kind, Go, Helm and Python 3.

### Local Kind demo

Create the management cluster (`releaseyard-a`), two workload clusters
(`releaseyard-managed-a/b`), install ChartMuseum, and publish the demo chart:

```bash
bash hack/setup-kind-workloads.sh
```

Build and load the controller and dashboard images:

```bash
docker build -t releaseyard-controller:dev .
docker build -f Dockerfile.dashboard -t releaseyard-dashboard:dev .
kind load docker-image releaseyard-controller:dev --name releaseyard-a
kind load docker-image releaseyard-dashboard:dev --name releaseyard-a
```

Install the controller manager with the chart:

```bash
helm upgrade --install idp dist/chart \
  -n idp-system --create-namespace \
  --set manager.image.repository=releaseyard-controller \
  --set manager.image.tag=dev \
  --set manager.allowedChartHosts=chartmuseum.chartmuseum.svc.cluster.local:8080
```

Deploy the dashboard with the local kustomization (it expects the
`releaseyard-dashboard:dev` image loaded above):

```bash
kubectl --context kind-releaseyard-a apply -k config/dashboard
```

Register the workload kubeconfigs with the management cluster:

```bash
kubectl --context kind-releaseyard-a -n idp-system create secret generic releaseyard-managed-a-kubeconfig \
  --from-file=kubeconfig=.local/kubeconfigs/releaseyard-managed-a.kubeconfig
kubectl --context kind-releaseyard-a -n idp-system create secret generic releaseyard-managed-b-kubeconfig \
  --from-file=kubeconfig=.local/kubeconfigs/releaseyard-managed-b.kubeconfig
```

Apply the samples:

```bash
kubectl --context kind-releaseyard-a apply -f config/samples/platform_v1alpha1_cluster.yaml
kubectl --context kind-releaseyard-a apply -f config/samples/platform_v1alpha1_applicationdefinition.yaml
kubectl --context kind-releaseyard-a apply -f config/samples/platform_v1alpha1_application.yaml
```

Open the dashboard at `http://127.0.0.1:8080` with a port-forward:

```bash
kubectl --context kind-releaseyard-a -n idp-system port-forward service/releaseyard-dashboard 8080:8080
```

Read the generated local administrator password and sign in as `admin`:

```bash
kubectl --context kind-releaseyard-a -n idp-system get secret releaseyard-dashboard-local-admin \
  -o jsonpath='{.data.initial-password}' | base64 -d
```

To try the optional cloud flow, install Crossplane on `releaseyard-a`, run
`bash hack/setup-crossplane-localstack.sh`, then apply the manifests in
`examples/cloud-bucket/` in order.

After code changes, `hack/deploy-controller-kind.sh` and
`hack/deploy-dashboard-kind.sh` rebuild the image, load it into Kind, and roll
the existing Deployment.

### Helm chart

The chart lives in [`dist/chart`](dist/chart). A minimal install is:

```bash
helm upgrade --install idp dist/chart -n idp-system --create-namespace \
  --set manager.image.repository=<registry>/releaseyard-controller \
  --set manager.image.tag=<tag>
```

Enable the dashboard with `dashboard.enabled=true` and set
`dashboard.image.repository`, `dashboard.image.tag`, and `dashboard.publicURL`.
With the default chart naming the dashboard Service is `<release>-dashboard`
(for example `idp-dashboard`) and the local administrator Secret is
`<release>-dashboard-local-admin`. The chart can also create an optional Ingress
when an Ingress class, hostname, and TLS Secret are available.

### YAML bundle

Generate a single installable manifest and apply it:

```bash
make build-installer IMG=<registry>/releaseyard-controller:<tag>
kubectl apply -f dist/install.yaml
```

## Configuration

### Cluster

A `Cluster` points at a kubeconfig Secret in the manager's credentials
namespace. The Secret must contain the kubeconfig under the `kubeconfig` key:

```yaml
apiVersion: platform.releaseyard.io/v1alpha1
kind: Cluster
metadata:
  name: releaseyard-managed-a
spec:
  kubeconfigSecretRef:
    name: releaseyard-managed-a-kubeconfig
```

### ApplicationDefinition

An `ApplicationDefinition` is the catalog entry. It pins the chart, the release
namespace, defaults, and the typed paths an `Application` may override:

```yaml
apiVersion: platform.releaseyard.io/v1alpha1
kind: ApplicationDefinition
metadata:
  name: releaseyard-demo
spec:
  description: Demo application
  chart:
    repository: http://chartmuseum.chartmuseum.svc.cluster.local:8080
    name: releaseyard-demo
    version: 0.2.0
  release:
    namespace: applications
    allowNamespaceOverride: true
  defaults:
    replicaCount: 1
    image:
      repository: nginx
      tag: 1.27-alpine
  configurableValues:
    - path: image.repository
      type: string
    - path: image.tag
      type: string
    - path: replicaCount
      type: integer
      minimum: 1
      maximum: 10
```

Only the paths listed in `configurableValues` are accepted from an
`Application`; the definition's `defaults` supply the rest.

### Application

```yaml
apiVersion: platform.releaseyard.io/v1alpha1
kind: Application
metadata:
  name: demo
  namespace: releaseyard-team-platform
spec:
  applicationDefinitionRef:
    name: releaseyard-demo
  clusterRef:
    name: releaseyard-managed-a
  values:
    replicaCount: 2
    image:
      tag: 1.27-alpine
```

Use `spec.environment` instead of `spec.clusterRef` to target every matching
cluster. Both `applicationDefinitionRef` and the placement choice are immutable
after creation.

### Cloud resources

```yaml
apiVersion: platform.releaseyard.io/v1alpha1
kind: CloudTarget
metadata:
  name: localstack
spec:
  provider: aws
  region: eu-west-1
  providerConfigRef:
    name: localstack
    namespace: releaseyard-team-platform
---
apiVersion: platform.releaseyard.io/v1alpha1
kind: ResourceDefinition
metadata:
  name: aws-bucket
spec:
  description: An S3 bucket managed by Crossplane.
  configurableValues:
    - path: bucketName
      type: string
      required: true
  resources:
    - name: bucket
      apiVersion: s3.aws.m.upbound.io/v1beta1
      kind: Bucket
      base:
        spec:
          managementPolicies:
            - "*"
      mappings:
        - from: values.bucketName
          to: "metadata.annotations[crossplane.io/external-name]"
        - from: target.region
          to: spec.forProvider.region
  outputs:
    - name: arn
      template: bucket
      from: status.atProvider.arn
---
apiVersion: platform.releaseyard.io/v1alpha1
kind: Resource
metadata:
  name: demo-bucket
  namespace: releaseyard-team-platform
spec:
  definitionRef:
    name: aws-bucket
  targetRef:
    name: localstack
  values:
    bucketName: demo-bucket
```

The `Resource` status reports the composed objects, the collected `outputs`, and
a `Ready` condition. Managed resources are owned by the `Resource`, so deleting
the request cascades to the cloud objects.

## Authentication & authorization

- **Local administrator** — the chart bootstraps a named Secret with a bcrypt
  hash and a generated initial password. The password is never stored in Helm
  values or printed in logs. Sign in as `admin` and change the configuration
  from the dashboard.
- **OIDC** — configure issuer URL, client ID, and client secret from the
  Administration page. The dashboard stores them in a dedicated Secret and
  applies them immediately, without a restart.
- **Authorization** — the dashboard impersonates the signed-in user, so the
  management cluster's Kubernetes RBAC is the source of truth. Any user may read
  the catalog; writes require the corresponding RBAC permission.
- **Administration area** — available to the local administrator and to OIDC
  groups listed in `dashboard.adminGroups` (with `rbac.helpers.enabled=true`
  binding the shipped dashboard admin role).
- **Helper roles** — the chart can install admin/editor/viewer roles for
  Applications, Application Definitions, and Clusters when
  `rbac.helpers.enabled=true`.

## Observability

The Application detail page shows live resources, Pod logs, and Pod CPU/memory
read from the managed cluster. CPU/memory requires `metrics-server` on the
managed cluster; when it is absent the dashboard reports that instead of
failing. `hack/setup-managed-metrics.sh` installs it on the Kind workload
clusters.

The controller manager exposes Prometheus metrics on its secure metrics endpoint
when `metrics.enabled=true`. The dashboard also ships an Overview with recent
activity and a per-Application Activity timeline built from Kubernetes events
(note that Kubernetes event retention is cluster-defined and usually short).

## Development

```bash
make help          # list targets
make manifests     # regenerate CRDs and RBAC
make generate      # regenerate DeepCopy code
make test          # unit and envtest suites
make lint          # golangci-lint
make test-e2e      # isolated Kind end-to-end suite
```

The end-to-end suite creates its own Kind cluster, deploys the manager, and
checks the manager, the metrics endpoint, and the `Resource` controller. The
chart has a separate workflow that lints and installs the chart to catch
distribution drift.

## Contributing

Contributions are welcome. Please:

1. Fork the repository.
2. Create a feature branch.
3. Make your changes and add tests where it matters.
4. Run `make lint` and `make test`.
5. Submit a pull request explaining the change.

## Disclaimer

> [!CAUTION]
> ReleaseYard installs Helm releases into the clusters you register and can
> create cloud resources through Crossplane. Grant access deliberately, review
> the RBAC it uses, and test in an isolated environment first. The LocalStack
> helper and fake credentials are for development only; never use them in
> production.

## License

Copyright 2026. Licensed under the [Apache License, Version 2.0](https://www.apache.org/licenses/LICENSE-2.0).
