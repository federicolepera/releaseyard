# Architecture

## Control plane

ReleaseYard runs on a **management cluster**. It does not need to run on the
workload clusters it manages.

```text
                    ┌──────────────────────────────┐
                    │        Management cluster      │
                    │                              │
  kubectl / GitOps ─►  ReleaseYard CRDs            │
                    │        │                     │
                    │        ▼                     │
                    │  ReleaseYard controller      │
                    │        │                     │
                    │        ├── Helm ─────────────┼──► workload cluster A
                    │        │                     │
                    │        ├── Helm ─────────────┼──► workload cluster B
                    │        │                     │
                    │        └── Crossplane ───────┼──► cloud provider
                    │                              │
                    │  Dashboard (optional)        │
                    └──────────────────────────────┘
```

The controller uses its own kubeconfig for each target cluster. The dashboard
authenticates and authorizes users against the management cluster; it does not
propagate user identity to workload clusters.

## Custom resources

| Kind | Scope | Purpose |
|------|-------|---------|
| `Cluster` | Cluster | Target cluster plus the Secret that holds its kubeconfig. |
| `ApplicationDefinition` | Cluster | Pinned chart, defaults, and allowed value paths. |
| `Application` | Namespaced | A deployment request for one cluster or an environment. |
| `CloudTarget` | Cluster | Provider, region, and Crossplane `ProviderConfig` reference. |
| `ResourceDefinition` | Cluster | Managed-resource templates, mappings, and outputs. |
| `Resource` | Namespaced | A cloud resource request rendered from a definition. |

The API group is `platform.releaseyard.io/v1alpha1`.

## Application reconciliation

1. Resolve the referenced `ApplicationDefinition` and target `Cluster`.
2. Load the target kubeconfig from the credentials namespace.
3. Validate that the Application only overrides paths declared in
   `configurableValues`, and merge definition defaults with the overrides.
4. Fetch the chart from an allowed host, verify its SHA-256 digest, and pin the
   digest in status.
5. Install or upgrade the Helm release in the existing target namespace.
6. Record the binding, chart identity, revision, conditions, and observed
   resources in `Application.status`.
7. On delete, run the finalizer: uninstall the release, verify ownership, and
   remove the remote claim before allowing the Application to disappear.

The controller deliberately refuses to adopt a Helm release it does not own.
Release ownership is recorded in a ConfigMap claim and in the release labels.

## Environment placement

An Application with `spec.environment.name` follows the Clusters whose
environment label matches. The controller re-resolves the set on every
reconcile, adds releases on new matching Clusters, and uninstalls/removes
instances for Clusters that leave. A non-empty `spec.environment.targets` pins
the set instead; later label changes do not alter it. Each target is reconciled
independently with per-instance status.

`spec.clusterRef`, `spec.environment`, `spec.applicationDefinitionRef`, and
`spec.releaseNamespace` are immutable after creation.

## Cloud reconciliation

Crossplane is an **external dependency**, not part of the ReleaseYard chart.

1. Resolve the `ResourceDefinition` and `CloudTarget`.
2. Validate `Resource.spec.values` against the definition.
3. Render one managed resource per template, merging the static `base` with
   mappings from `values.*` and `target.*`.
4. Create or update the managed resource in the same namespace as the
   `Resource`. This matters for namespaced Crossplane `ProviderConfig`s.
5. Read configured fields from the managed resource status into
   `Resource.status.outputs`.
6. Report composed objects and a `Ready` condition.

Managed resources are owned by the `Resource`, so deleting the request cascades
to the cloud objects. The `base` in the examples sets
`spec.managementPolicies: ["*"]`, which tells Crossplane to manage the full
lifecycle, including deletion.

## Safety model

- **Chart allowlist** — the manager only fetches charts from configured hosts.
- **Digest pinning** — chart bytes are verified before Helm receives them.
- **Immutable bindings** — a target or definition cannot be swapped silently.
- **Release ownership** — the controller does not adopt a pre-existing release.
- **No secret rendering** — live Application resources exclude Secret data.
- **RBAC-first authorization** — the dashboard impersonates users instead of
  maintaining a parallel permission store.
- **Optional cloud** — without Crossplane, the Kubernetes application path is
  unchanged.

## Current scope

The chart path targets self-contained Helm charts. The current implementation
does not support chart dependencies, `values.schema.json`, hooks, or CRDs
installed by the chart. Chart templates are trusted input; validation checks the
contract and the archive digest, not whether Helm can render every template.

The dashboard supports an in-memory session store and a shared Redis store.
The in-memory default is suitable only for a single replica: a rollout signs
everyone out. Use Redis (`SESSION_STORE=redis`) for production, for multiple
replicas, and to keep sessions across rollouts.

Security-relevant actions are written as structured JSON audit events to
stdout: local and OIDC login outcomes, logout, state-changing API requests, and
administration changes. The audit trail never includes passwords, tokens,
client secrets, or request bodies; ship the container logs to a durable central
sink.

## Related docs

- [Applications](applications.md)
- [Cloud resources](cloud-resources.md)
- [Access control](access-control.md)
- [Observability](observability.md)
