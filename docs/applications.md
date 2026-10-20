# Applications

An Application is a request to install a pinned Helm chart on one or more
clusters. It is namespaced; the catalog entry and target cluster are
cluster-scoped.

## ApplicationDefinition

An `ApplicationDefinition` is the catalog contract. It declares the chart, the
release namespace, defaults, and the only value paths an Application may
override.

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

Rules:

- `chart.repository` must be HTTP(S) and the host must be allowed by the
  controller.
- `chart.version` must be a pinned semantic version. Floating tags are rejected.
- `release.namespace` is the target namespace on the workload cluster. It is
  immutable after creation.
- `configurableValues` is the complete override surface. A path not listed there
  is rejected.
- Supported value types are `string`, `integer`, and `boolean`. Integer values
  may declare inclusive `minimum` and `maximum`.
- `required: true` means an Application must supply the value.

The controller validates the definition and downloads the chart archive to
compute its SHA-256 digest. A valid definition does not guarantee that Helm can
render or install the chart.

## Application

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

Only paths declared in the definition can appear under `spec.values`. The
controller merges the definition defaults with these overrides.

## Placement

### One cluster

```yaml
spec:
  clusterRef:
    name: releaseyard-managed-a
```

The Application produces one Helm release on that cluster.

### An environment

```yaml
spec:
  environment:
    name: staging
    targets:
      - name: releaseyard-managed-a
        uid: <cluster-uid>
```

The targets are an immutable snapshot of the clusters selected by the
`platform.releaseyard.io/environment` label at creation time. Creating an
environment Application through the dashboard resolves and previews this
snapshot. Direct API clients must provide an explicit target list.

Exactly one of `clusterRef` or `environment` must be set. The choice is
immutable.

## Release namespace

When `release.namespace` is set in the definition, the release goes there. If
`allowNamespaceOverride: true`, the Application may set
`spec.releaseNamespace` to an existing target namespace. The value is immutable
after creation.

The target namespace must already exist; the controller does not create it.

## Release naming

The release name combines the team namespace token and the Application name:

```text
releaseyard-team-platform + payments -> platform-payments
idp-system + payments                -> idp-system-payments
```

The prefix is configurable with `manager.teamNamespacePrefix`. Names are
truncated to 53 characters with a hash suffix when necessary.

## Lifecycle

1. The definition is validated and its chart digest is recorded.
2. The Application resolves its target and binding.
3. The chart is fetched again and must match the pinned digest.
4. Helm installs or upgrades the release.
5. Status records conditions, the applied revision, chart identity, and observed
   resources.
6. On deletion, the finalizer uninstalls the release, checks ownership, and
   removes the remote claim.

The controller refuses to adopt a release it does not own. A release is owned
when its claim ConfigMap and labels match the expected Application identity.

## Safety limits

The chart path currently supports self-contained charts only. It does not
support:

- chart dependencies;
- `values.schema.json`;
- Helm hooks;
- CRDs installed by the chart.

Chart templates are trusted input. The platform verifies the contract and the
archive digest, not the rendered manifest.

## Related docs

- [Architecture](architecture.md)
- [Configuration](configuration.md)
- [Troubleshooting](troubleshooting.md)
