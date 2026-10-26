# Configuration

This page is a reference for the main CRD fields and Helm values. Complete
examples are in [`config/samples/`](../config/samples/) and
[`examples/`](../examples/).

## ApplicationDefinition

| Field | Required | Notes |
|-------|----------|-------|
| `spec.description` | no | Shown in the catalog. Max 280 characters. |
| `spec.chart.repository` | yes | HTTP(S) ChartMuseum-compatible repository. Host must be allowed. |
| `spec.chart.name` | yes | Chart name. |
| `spec.chart.version` | yes | Pinned semantic version; no floating tags. |
| `spec.release.namespace` | yes | Existing namespace on the target cluster. Immutable. |
| `spec.release.allowNamespaceOverride` | no | Allows `Application.spec.releaseNamespace`. |
| `spec.defaults` | no | Nested Helm values supplied when the Application does not override them. |
| `spec.configurableValues` | no | The only value paths an Application may set. |

`configurableValues` entries:

| Field | Notes |
|-------|-------|
| `path` | Dot-separated path. Dots have no escaping syntax. |
| `type` | `string`, `integer`, or `boolean`. |
| `required` | Defaults to `false`. |
| `minimum` / `maximum` | Integer bounds only. |

## Application

| Field | Required | Notes |
|-------|----------|-------|
| `spec.applicationDefinitionRef.name` | yes | Immutable. |
| `spec.clusterRef.name` | one of | Single-cluster placement. Immutable. |
| `spec.environment` | one of | Environment name plus explicit target snapshot. Immutable. |
| `spec.releaseNamespace` | no | Only when the definition allows overrides. Immutable. |
| `spec.values` | no | Only paths declared by the definition. |

Exactly one of `clusterRef` or `environment` must be set.

## CloudTarget

| Field | Required | Notes |
|-------|----------|-------|
| `spec.provider` | yes | Currently `aws`. |
| `spec.region` | yes | Default region for mappings. |
| `spec.providerConfigRef.name` | yes | Crossplane `ProviderConfig` name. |
| `spec.providerConfigRef.namespace` | yes | Must match the `Resource` namespace for namespaced providers. |

## ResourceDefinition

| Field | Required | Notes |
|-------|----------|-------|
| `spec.description` | no | Shown in the cloud catalog. |
| `spec.configurableValues` | no | Same value schema as ApplicationDefinition. |
| `spec.resources` | yes | At least one managed-resource template. |
| `spec.resources[].name` | yes | Template name used by outputs. |
| `spec.resources[].apiVersion` / `kind` | yes | Crossplane managed-resource type. |
| `spec.resources[].base` | no | Static fragment merged into the object. |
| `spec.resources[].mappings` | no | Copy `values.*` or `target.*` into fields. |
| `spec.outputs` | no | Read status fields into `Resource.status.outputs`. |

## Resource

| Field | Required | Notes |
|-------|----------|-------|
| `spec.definitionRef.name` | yes | Cluster-scoped `ResourceDefinition`. |
| `spec.targetRef.name` | yes | Cluster-scoped `CloudTarget`. |
| `spec.values` | no | Validated against the definition. |

## Helm values

### Manager

| Value | Default | Notes |
|-------|---------|-------|
| `manager.enabled` | `true` | Install the controller. |
| `manager.replicas` | `1` | Use leader election for more replicas. |
| `manager.image.repository` | `controller` | Controller image. |
| `manager.image.tag` | chart appVersion | Image tag. |
| `manager.allowedChartHosts` | `""` | Comma-separated chart hosts. Empty uses the binary default `127.0.0.1:18080`. |
| `manager.teamNamespacePrefix` | `releaseyard-team-` | Prefix used for team namespace and release-name derivation. |
| `manager.args` | `["--leader-elect"]` | Extra manager arguments. |
| `manager.healthProbe.port` | `8081` | Liveness/readiness port. |
| `manager.resources` | small requests/limits | Controller resources. |

### RBAC

| Value | Default | Notes |
|-------|---------|-------|
| `rbac.namespaced` | `false` | Use Role/RoleBinding instead of cluster-scoped RBAC. |
| `rbac.helpers.enabled` | `false` | Install admin/editor/viewer helper roles. |

### Dashboard

| Value | Default | Notes |
|-------|---------|-------|
| `dashboard.enabled` | `false` | Deploy the dashboard. |
| `dashboard.replicas` | `1` | More than 1 requires `dashboard.sessionStore.type=redis`. |
| `dashboard.image.repository` | `""` | Required when enabled. |
| `dashboard.image.tag` | `""` | Required when enabled. |
| `dashboard.publicURL` | `""` | Required when enabled; canonical base URL. |
| `dashboard.sessionStore.type` | `memory` | `memory` or `redis`. Use `redis` in production and for HA. |
| `dashboard.sessionStore.redis.address` | `""` | Required when type is `redis`. |
| `dashboard.sessionStore.redis.username` | `""` | Optional Redis ACL user. |
| `dashboard.sessionStore.redis.db` | `0` | Redis database index. |
| `dashboard.sessionStore.redis.passwordSecret.name` | `""` | Existing Secret containing the Redis password. |
| `dashboard.sessionStore.redis.passwordSecret.key` | `password` | Key in the Redis password Secret. |
| `dashboard.sessionStore.redis.tls.enabled` | `false` | Use TLS for the Redis connection. |
| `dashboard.sessionStore.redis.keyPrefix` | `releaseyard:dashboard:` | Key namespace in Redis. |
| `dashboard.teamNamespacePrefix` | `releaseyard-team-` | Team namespace prefix. |
| `dashboard.adminGroups` | `[]` | OIDC groups allowed in Administration. |
| `dashboard.bootstrap.enabled` | `true` | Create the local administrator Secret. |
| `dashboard.ingress.enabled` | `false` | Optional Ingress. Requires class, host, TLS. |
| `dashboard.oidcLoopback.enabled` | `false` | Development-only loopback aid. |

Redis is not deployed by the chart. Use a managed Redis or deploy one separately,
and set `dashboard.sessionStore.type=redis`. The chart requires it when
`dashboard.replicas` is greater than 1.

OIDC is not configured through Helm values. Set it from the Administration page
after installation.

### Metrics and monitoring

| Value | Default | Notes |
|-------|---------|-------|
| `metrics.enabled` | `true` | Expose controller metrics. |
| `metrics.port` | `8443` | Metrics port. |
| `metrics.secure` | `true` | HTTPS and authn/authz. |
| `prometheus.enabled` | `false` | Create a ServiceMonitor. |
| `networkPolicy.enabled` | `false` | Restrict metrics ingress. |

### Admission

| Value | Default | Notes |
|-------|---------|-------|
| `admission.enabled` | `true` | Enforce Application references with a ValidatingAdmissionPolicy. Requires Kubernetes 1.31+. |
| `admission.failurePolicy` | `Fail` | Reject the create when the policy cannot be evaluated. |

### CRDs and lifecycle

| Value | Default | Notes |
|-------|---------|-------|
| `crd.enabled` | `true` | Install CRDs with the chart. |
| `crd.keep` | `true` | Keep CRDs on uninstall. |
| `webhook.enabled` | `false` | Webhook server scaffold. |
| `certManager.enabled` | `false` | cert-manager integration. |

## Examples

```bash
kubectl apply -f config/samples/platform_v1alpha1_cluster.yaml
kubectl apply -f config/samples/platform_v1alpha1_applicationdefinition.yaml
kubectl apply -f config/samples/platform_v1alpha1_application.yaml
```

For cloud resources, see [Cloud resources](cloud-resources.md).
