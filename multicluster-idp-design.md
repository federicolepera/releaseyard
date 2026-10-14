# ReleaseYard: Kubernetes-Native Application Platform
## MVP Architecture and Development Plan

> ReleaseYard is a deliberately limited, Kubernetes-native platform for defining safe Helm application contracts and managing one real Helm release for each Application in one explicitly chosen Kubernetes cluster.

---

# 1. Vision and scope

The platform provides a Kubernetes-native API between platform engineers and application developers. It does not replace Kubernetes, Helm, GitOps, CI, policy engines, or observability systems.

The MVP proves this workflow:

> A Platform Engineer defines a Helm chart contract and its allowed values. A Developer creates an Application, chooses exactly one Cluster, and supplies values within that contract. The control plane installs, upgrades, and uninstalls a real Helm release in that target cluster.

The MVP includes:

- Kubernetes custom resources for `Cluster`, `ApplicationDefinition`, and `Application`;
- controlled developer values and validation against the definition;
- one explicit, immutable `spec.clusterRef` per Application;
- direct control-plane access to target clusters using credentials or kubeconfigs suitable for a demo;
- Helm Go SDK lifecycle management for one predictable release per Application;
- clear validation and reconciliation outcomes.

The MVP does **not** include cluster fleets, cluster selectors, placement policies, automatic placement, multi-cluster replication of one Application, failover, migration, a remote agent, a resource graph, or a portal. An Application stays on its selected cluster. If that cluster is unavailable, the platform reports the limitation; it does not relocate the release.

Later, optional multi-cluster replication can introduce concepts such as `Fleet`, `PlacementPolicy`, `ApplicationDeployment`, and a cluster agent. Those are future evolution, not part of the MVP model or lifecycle.

---

# 2. Design principles

## Kubernetes is the API

Platform state is represented by Kubernetes custom resources, inspectable with `kubectl`. The API server is the source of desired state; there is no hidden primary database. A future portal or CLI can be a client of the same API.

## Helm manages the release

Helm is not merely a template renderer in this design. The controller uses Helm's Go SDK to perform release `install`, `upgrade`, and `uninstall` operations against the target cluster. It must not replace Helm lifecycle management with rendered-manifest apply/prune logic.

Each Application has one predictable Helm release identity and a target namespace. Release identity and namespace are defined by the Application contract (with documented defaults if needed) and remain stable for the lifetime of the Application. The controller resolves credentials for `spec.clusterRef`, configures the Helm action for that cluster, and performs the corresponding lifecycle operation there.

## Platform engineer defines the contract

An `ApplicationDefinition` identifies a chart and establishes defaults and the values developers may set. Developers cannot submit arbitrary Helm values. The controller validates both the shape and constraints of submitted values, then merges validated developer values over definition defaults before invoking Helm.

## An Application has one target in the MVP

An Application names exactly one Cluster. It does not mean an abstract service that the platform fans out to multiple clusters. Its cluster reference is immutable in the MVP: changing clusters requires an explicitly designed later workflow, not an automatic move, reinstall, or migration.

---

# 3. Architecture

```text
 Developer / kubectl / future client
                 |
                 v
       Platform Kubernetes API
        Cluster  ApplicationDefinition
                 Application
                 |
                 v
        Platform Controller
       - validate and merge values
       - resolve immutable clusterRef
       - Helm Go SDK lifecycle
                 |
       direct authenticated connection
          +------+------+
          |             |
          v             v
    Kubernetes A   Kubernetes B
    Helm release   Helm release
    for app-a      for app-b
```

The control plane can hold or access target-cluster credentials for the demo. Credential storage and production-grade secret distribution need appropriate security treatment, but no agent protocol is required to prove the MVP. Clusters are existing Kubernetes clusters; provisioning them is out of scope.

The controller needs API reconcilers for the three core resources and an internal Helm client that can create Helm action configurations for a selected cluster and namespace. A UI, CLI, agent, and separate placement controller are not MVP components.

---

# 4. Core API model

The MVP has exactly three core resources:

```text
Cluster                 connection identity and cluster metadata
ApplicationDefinition   chart, release contract, defaults, allowed values
Application             one definition, one immutable target, supplied values
```

`Fleet`, `PlacementPolicy`, `ApplicationDeployment`, and an agent are deferred. In particular, the MVP creates no second deployment object for an Application.

## 4.1 Cluster

`Cluster` is cluster-scoped and registers an existing target cluster. It refers to a Kubernetes Secret by name only; that Secret must be in the controller manager's credentials namespace and contain the kubeconfig under `data.kubeconfig`. The reference does not specify a namespace or key, and raw credentials are not embedded in the Cluster resource. The controller uses the Secret to construct a target Kubernetes client and Helm configuration.

```yaml
apiVersion: platform.releaseyard.io/v1alpha1
kind: Cluster
metadata:
  name: cluster-a
spec:
  kubeconfigSecretRef:
    name: cluster-a-credentials
  labels:
    environment: development
    region: eu-west
```

Labels are descriptive metadata only in the MVP. They do not select a cluster or trigger placement. A `Connected` condition reports whether the latest connection probe could authenticate to the target, and `lastProbeTime` records when that probe occurred; neither claims cluster or workload health. Detailed capacity inventory and heartbeat protocols are not required.

## 4.2 ApplicationDefinition

An `ApplicationDefinition` is the platform engineer's contract for a class of applications. It identifies a single chart, provides fixed defaults, and lists the values that an Application may configure. The definition specifies the target namespace and predictable release identity convention, or a safe default for each. An Application must not change its release identity during its lifetime.

```yaml
apiVersion: platform.releaseyard.io/v1alpha1
kind: ApplicationDefinition
metadata:
  name: web-service
spec:
  chart:
    repository: oci://ghcr.io/acme/platform-charts
    name: web-service
    version: "1.4.0"
  release:
    namespace: applications
    nameTemplate: "{{ .ApplicationName }}"
  defaults:
    replicaCount: 2
    service:
      type: ClusterIP
  configurableValues:
    image.repository:
      type: string
      required: true
    image.tag:
      type: string
      required: true
    replicaCount:
      type: integer
      minimum: 1
      maximum: 10
```

The example illustrates the contract; the exact schema representation and allowed template syntax should be specified in the API implementation. Keep the release name deterministic and valid for Helm/Kubernetes naming constraints.

## 4.3 Application

An `Application` references one definition, explicitly names one registered cluster, and supplies only allowed values.

```yaml
apiVersion: platform.releaseyard.io/v1alpha1
kind: Application
metadata:
  name: payments
spec:
  definitionRef:
    name: web-service
  clusterRef:
    name: cluster-a
  values:
    image:
      repository: registry.company.local/payments
      tag: "1.4.2"
    replicaCount: 3
```

`spec.clusterRef` is required and immutable in the MVP. The platform does not infer it from labels, a default, a Fleet, or a selector. If the chosen cluster is unavailable, the Application remains targeted there and reports the connection/reconciliation problem.

If a developer supplies a value not exposed by the definition, violates a type/range/required constraint, or provides a value that cannot be merged under the contract, the Application is invalid. Invalid input must not invoke Helm upgrade and must not alter the installed release. The controller should communicate validation through useful conditions or events. The exact observed-versus-desired revision fields and advanced status semantics remain an open future decision; the MVP should not prematurely standardize them.

---

# 5. Helm release lifecycle

The controller reconciles one Application to one release on its referenced Cluster:

```text
Application + ApplicationDefinition
                 |
                 v
  resolve definition and target credentials
                 |
                 v
 validate exposed values and constraints
                 |
                 v
 merge defaults with valid Application values
                 |
                 v
 Helm Go SDK: install / upgrade / uninstall
                 |
                 v
 one release in the target namespace
```

The target is always the Application's `clusterRef`. Release name and namespace are predictable and stable. The controller uses Helm's release state to decide whether to install or upgrade; deleting an Application causes Helm uninstall for that release. It does not render a chart and independently apply or prune its resources.

## Create

1. Resolve the referenced `ApplicationDefinition` and `Cluster`.
2. Validate Application values against the definition, including allowed paths, types, required values, and constraints.
3. Merge definition defaults with the validated values.
4. Configure Helm Go SDK for the target cluster, release name, and namespace.
5. Install the chart as a real Helm release.

Missing definitions, missing/unusable credentials, invalid values, chart retrieval failures, or Helm errors are reported without silently targeting another cluster.

## Update

An update to valid configurable values, or a definition/chart change allowed by the API contract, is reconciled against the same release on the same cluster using Helm upgrade. Validate and merge before invoking Helm. If validation fails, do not upgrade; the last successful release remains untouched. `clusterRef` cannot be changed, so update reconciliation never relocates or duplicates an Application.

## Delete

Deleting an Application uninstalls its Helm release from its target cluster and namespace through Helm. Finalization must account for an unreachable target: retain/report the pending cleanup rather than claiming successful uninstall or applying a custom resource-pruning substitute. The exact retention policy for a permanently unreachable cluster can be decided before production use.

## Failure behavior

- Invalid values: mark/report invalid; do not call Helm upgrade.
- Cluster unavailable: report that the explicitly selected target cannot be reached; no failover.
- Chart or Helm operation fails: report the operation failure and allow reconciliation to retry according to controller policy.
- Application deleted while target unavailable: report cleanup as pending; do not silently declare success.

Status should remain useful without overcommitting to an elaborate deployment status API. Conditions and concise operation messages are sufficient for the MVP; desired/applied revision tracking is intentionally deferred.

---

# 6. Repository and implementation shape

A small Go monorepo is sufficient:

```text
platform/
├── api/v1alpha1/                 # Cluster, ApplicationDefinition, Application
├── internal/
│   ├── helm/                     # Helm Go SDK client/action configuration
│   ├── values/                   # validation and defaults merge
│   └── cluster/                  # credential lookup and target clients
├── controllers/
│   ├── cluster/
│   ├── applicationdefinition/
│   └── application/
├── cmd/platform-controller/
├── config/                       # CRDs, RBAC, manager configuration
├── examples/
└── docs/
```

Use Go with Kubebuilder/controller-runtime. Prefer Helm's Go SDK; do not shell out to the Helm CLI. Keep remote cluster access behind a small client boundary so credential handling and Helm action setup are testable. Do not create `agent/`, placement controllers, or deployment CRDs in the MVP repository plan.

---

# 7. Development phases and acceptance

## Phase 0 — Bootstrap

- Initialize the Go module and Kubebuilder project.
- Define the API group and controller-manager setup.
- Configure local development with a management cluster and two workload clusters (for example, three Kind clusters total).
- Establish a secure demo mechanism for supplying each workload cluster's credentials.

**Acceptance:** the manager starts and can read the platform API and connect to explicitly registered test clusters.

## Phase 1 — Cluster registration

- Add the `Cluster` CRD and credential reference.
- Build target-cluster clients from registered credentials.
- Expose enough connection status to diagnose a bad or unreachable target.

**Acceptance:** register two clusters and verify each is independently reachable. Labels remain informational and are not used to choose targets.

## Phase 2 — Application contract and validation

- Add `ApplicationDefinition` and `Application` CRDs.
- Support a Helm chart reference, release namespace/name convention, defaults, and controlled values.
- Validate unknown paths, types, required inputs, and constraints.
- Enforce immutable `spec.clusterRef`.
- Merge only validated user values with defaults.

**Acceptance:** a valid Application produces effective values; an unsupported or out-of-range value is rejected and cannot trigger a Helm upgrade. Attempting to change `clusterRef` is rejected.

## Phase 3 — Helm lifecycle

- Implement Helm Go SDK install and upgrade against the Application's target cluster.
- Keep one stable release identity per Application.
- Add deletion finalization that invokes Helm uninstall.
- Surface chart, connection, validation, and Helm errors.

**Acceptance:** create one Application and observe one Helm release in exactly its specified cluster/namespace; update values and verify Helm upgrades that release; delete it and verify Helm uninstalls it. Confirm the other cluster has no release for that Application.

## Phase 4 — Demo hardening

- Provide complete examples and repeatable local setup instructions.
- Test unavailable target behavior and invalid-update safety.
- Keep status/conditions concise and actionable.

**Acceptance:** demonstrate two distinct Applications, one explicitly assigned to each of two clusters. Each has its own Helm release. No single Application is automatically replicated, rescheduled, or failed over.

---

# 8. Demo scenario and value

Start the control plane and two workload clusters. Register both Clusters, apply one `ApplicationDefinition`, then create two Applications with distinct names and explicit targets:

```yaml
apiVersion: platform.releaseyard.io/v1alpha1
kind: Application
metadata:
  name: hello-a
spec:
  definitionRef:
    name: web-service
  clusterRef:
    name: cluster-a
  values:
    image:
      repository: nginx
      tag: "1.27"
    replicaCount: 2
---
apiVersion: platform.releaseyard.io/v1alpha1
kind: Application
metadata:
  name: hello-b
spec:
  definitionRef:
    name: web-service
  clusterRef:
    name: cluster-b
  values:
    image:
      repository: nginx
      tag: "1.27"
    replicaCount: 1
```

The demo shows that the platform can safely manage Helm releases in more than one cluster while each Application has an explicit, stable destination. Updating `hello-a` upgrades only its release in cluster-a. Making cluster-b unavailable affects `hello-b`; it does not move it to cluster-a. An invalid values update is rejected without upgrading the release.

The MVP value proposition is:

> **Define a safe Helm application contract once, then manage each Application as a real Helm release on its explicitly selected Kubernetes cluster.**

This is more than a Helm wrapper because it adds Kubernetes-native application contracts, controlled configuration, explicit cluster registration, and a reconciled lifecycle. It does not claim fleet-wide placement or automatic multi-cluster delivery.

---

# 9. Explicitly deferred evolution

Only after the single-target lifecycle is reliable should the project consider optional replication across clusters. That future design may introduce:

- `Fleet` for grouping clusters;
- `PlacementPolicy` for explicit selection and replica intent;
- `ApplicationDeployment` for a per-cluster instance of a logical Application;
- a cluster agent for environments where direct credentials are unsuitable;
- richer resource discovery, status aggregation, failover, or migration workflows.

These concepts require separate API and safety decisions. They must not be implied by MVP fields, examples, controller behavior, or acceptance criteria. In particular, future replication is not a reason to make `clusterRef` mutable or to relocate an existing MVP Application automatically.

The MVP intentionally excludes a portal, policy engine, CI system, cluster provisioning, advanced rollout orchestration, global DNS, multi-cluster networking, cost-aware placement, and automatic failover. Complete the narrow Helm lifecycle and its safety guarantees before expanding scope.

---

# 10. Final MVP model

```text
ApplicationDefinition
  - chart and release contract
  - defaults and configurable values
              |
              v
Application
  - definitionRef
  - immutable clusterRef (exactly one)
  - validated values
              |
              v
Platform Controller + Helm Go SDK
              |
              v
One Helm release in that Cluster and namespace
       install / upgrade / uninstall
```

The first milestone is deliberately modest: an Application has one explicit destination, a controlled contract, and a real Helm-managed lifecycle there. Multi-cluster replication is a possible later evolution, not an MVP promise.
