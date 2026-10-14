# ReleaseYard
ReleaseYard is a Kubernetes-native operator that manages one Helm release per Application in an explicitly selected cluster. The MVP supports self-contained charts only; charts using a values schema, dependencies, hooks, or CRDs are not supported.

## Description
// TODO(user): An in-depth paragraph about your project and overview of use

## ApplicationDefinition schema

The cluster-scoped `ApplicationDefinition` example in `config/samples/platform_v1alpha1_applicationdefinition.yaml` declares a pinned chart from a ChartMuseum-compatible HTTP(S) Helm repository, the target namespace, default values, and the typed value paths Applications may configure. `spec.release.namespace: applications` is the namespace on a target workload cluster. The Helm release name is the consuming Application's `metadata.name`. OCI repositories are not supported.

The ApplicationDefinition controller validates the definition, verifies that the chart is listed by the repository, and downloads its archive bytes. `status.digest` is the SHA-256 digest of those downloaded bytes. A `Validated=True` condition means the internal contract passed and the chart was listed and downloaded; it does not certify that the Helm chart archive is valid or installable. The MVP supports self-contained charts without a values schema, dependencies, hooks, or CRDs.

Chart fetches are limited by the manager's comma-separated `--allowed-chart-hosts` flag, which defaults to `127.0.0.1:18080`. The allowlist is the trust boundary: HTTP and HTTPS are both accepted, but only for allowlisted authorities. The sample targets ChartMuseum over its in-cluster Service (`http://chartmuseum.chartmuseum.svc.cluster.local:8080`); the local setup workflow below installs ChartMuseum in the management cluster and publishes the demo chart there. Its `releaseyard-demo:0.2.0` chart is installed only after you explicitly apply the Application sample. The Application controller installs or updates that chart in the selected workload cluster using the Application's name as the Helm release name.

## Getting Started

### Prerequisites
- Go 1.26.0, as specified by `go.mod`.
- Docker, `kubectl`, and Kind for the local workload-cluster demo.
- Helm 3 and `curl` for the local ChartMuseum setup; the setup script also requires Python 3 and `sha256sum`.
- The current development environment uses Kubernetes 1.37; a minimum supported Kubernetes version has not been established.

### Local Kind workload clusters

On WSL, the setup script requires `fs.inotify.max_user_instances` to be at least `512`. If it is lower, run `sudo sysctl -w fs.inotify.max_user_instances=512` in the WSL terminal and rerun setup. This temporary setting resets after WSL restarts. A low limit can cause kube-proxy to CrashLoop with an fsnotify watcher error, preventing the local-path provisioner from reaching the Kubernetes service and leaving the ChartMuseum PVC Pending.

For the local demo, run this from a machine with `kind`, Docker, `kubectl`, and Python 3 installed:

```sh
bash hack/setup-kind-workloads.sh
```

The script creates or reuses exactly these three Kind clusters on the default shared Docker `kind` network: management cluster `releaseyard-a`, and workload clusters `releaseyard-managed-a` and `releaseyard-managed-b`. It never deletes or recreates existing clusters. It exports **only** the workload clusters' internal kubeconfigs to `.local/kubeconfigs/releaseyard-managed-a.kubeconfig` and `.local/kubeconfigs/releaseyard-managed-b.kubeconfig`; it does not export a management-cluster kubeconfig, create Secrets, or apply Cluster resources. Kubeconfig output files are sensitive and are created with mode `0600` in a private output directory.

After the clusters and workload kubeconfigs are ready, the script installs ChartMuseum chart version `3.10.4` in namespace `chartmuseum` on `kind-releaseyard-a`, with persistence and the upload API enabled, then packages and publishes `hack/charts/releaseyard-demo` version `0.2.0`. It verifies the indexed version and downloaded package bytes; if that version already exists with different bytes, setup stops rather than replacing it. The chart is published to ChartMuseum only; it is **not** installed into either managed workload cluster. Packaging output is kept under the private `.local/charts` directory.

The setup script briefly opens `127.0.0.1:18080` for upload and verification, then closes its temporary port-forward when it exits. To keep the sample repository URL reachable from your host during VSCode debugging, start this command in a separate terminal and leave it running:

```sh
kubectl --context kind-releaseyard-a -n chartmuseum port-forward service/chartmuseum 18080:8080 --address 127.0.0.1
```

The demo chart installs two nginx Deployments: the primary Deployment uses the release name and configurable `replicaCount`, while a secondary Deployment named `<release>-secondary` always has one replica. Both use the same image, ConfigMap-backed page, and ConfigMap checksum rollout behavior. The single Service selects only primary Pods. The chart exposes `replicaCount`, `message`, `image.repository`, and `image.tag` in `hack/charts/releaseyard-demo/values.yaml`; the Application sample overrides `replicaCount` and `message`, while image values remain at the definition defaults. Updating the chart to `0.2.0` preserves the primary Deployment's existing selector; applying the update adds the secondary Deployment and rolls primary Pods to add their component label. For example, use `--set replicaCount=3 --set message='Different message' --set image.tag=1.27` with `helm template` to inspect a customized render.

Kind provides the management context `kind-releaseyard-a` in the host kubeconfig (`~/.kube/config`, unless `KUBECONFIG` is set). Use this host context for management operations and always pass `--context kind-releaseyard-a`; do not set the host's `KUBECONFIG` to either exported `--internal` workload file. Kind may update the host kubeconfig/current context while creating clusters; the script does not otherwise edit it. These workload kubeconfigs use cluster-admin credentials for a local demo only; never use them as production credentials. Only a platform administrator should enroll/register target Clusters and their credential Secrets. The remote identity in each kubeconfig must be allowed to create `selfsubjectreviews.authentication.k8s.io`, which the controller uses to probe authentication. For WSL debugging, create the workload credential Secret in `idp-system` using the host kubeconfig file as shown below; never print credentials or commit kubeconfig/Secret data. Run the manager locally with F5 in VSCode rather than deploying an operator Pod. The local process uses the host kubeconfig and can reach the host's ChartMuseum port-forward. The in-cluster sample points at the ChartMuseum Service; when running the manager on the host (F5), override the ApplicationDefinition's `spec.chart.repository` to `http://127.0.0.1:18080` and include `127.0.0.1:18080` in `--allowed-chart-hosts`.

Before creating Secrets, verify the exact management context and that the credentials namespace exists:

```sh
kubectl config get-contexts kind-releaseyard-a
kubectl --context kind-releaseyard-a get namespace idp-system
```

`idp-system` must exist before creating Secrets. It is normally created when deploying the operator; otherwise create it explicitly with `kubectl --context kind-releaseyard-a create namespace idp-system`. Then create each workload credential Secret:

```sh
kubectl config view --context kind-releaseyard-managed-a --raw --minify --flatten |
  kubectl --context kind-releaseyard-a -n idp-system create secret generic releaseyard-managed-a-kubeconfig \
    --from-file=kubeconfig=/dev/stdin
kubectl config view --context kind-releaseyard-managed-b --raw --minify --flatten |
  kubectl --context kind-releaseyard-a -n idp-system create secret generic releaseyard-managed-b-kubeconfig \
    --from-file=kubeconfig=/dev/stdin
```

Set each workload Cluster's `spec.kubeconfigSecretRef.name` to its matching Secret: `releaseyard-managed-a-kubeconfig` or `releaseyard-managed-b-kubeconfig`. The explicit `--context kind-releaseyard-a` protects against accidentally writing credentials to the current workload context; still verify the context and namespace before running commands. The setup script's internal kubeconfigs are intended for an in-cluster manager; for local F5 debugging, use the host-context kubeconfig-derived Secrets shown above so the manager can reach the workload cluster from the host.

The script always exports each current workload kubeconfig to a temporary file before checking output. Identical credentials are reused. If a workload cluster has been recreated and its credentials differ, the script fails without overwriting the file or Secret. After verifying the recreation, deliberately remove only the affected output file (`.local/kubeconfigs/releaseyard-managed-a.kubeconfig` or `.local/kubeconfigs/releaseyard-managed-b.kubeconfig`) and rerun the script. Then deliberately replace the corresponding Secret in the management cluster (repeat for the other workload if needed):

```sh
kubectl config view --context kind-releaseyard-managed-a --raw --minify --flatten |
  kubectl --context kind-releaseyard-a -n idp-system create secret generic releaseyard-managed-a-kubeconfig \
    --from-file=kubeconfig=/dev/stdin --dry-run=client -o yaml |
  kubectl --context kind-releaseyard-a -n idp-system apply -f -
```

The script never deletes a cluster, output file, or Secret. The Cluster `Connected` condition is the last recorded probe result, not a live signal; if the controller stops, it can remain stale. Check `status.lastProbeTime` to see when the last completed probe occurred.

### Run the local Application demo

Run the manager locally from VSCode with F5; this workflow does not deploy an operator Pod. First make the CRDs available on the management cluster, explicitly selecting its context:

```sh
kubectl --context kind-releaseyard-a apply -k config/crd
```

Create the `idp-system` namespace and target Cluster/credential Secret as described above, then apply the ApplicationDefinition and Cluster resources to the management cluster with that same explicit context. The Cluster sample already targets `releaseyard-managed-a` and its matching credential Secret. For this local demo, the target release namespace must already exist on the workload cluster. Create it before applying the Application:

```sh
kubectl --context kind-releaseyard-managed-a create namespace applications
kubectl --context kind-releaseyard-a apply -f config/samples/platform_v1alpha1_applicationdefinition.yaml
kubectl --context kind-releaseyard-a apply -f config/samples/platform_v1alpha1_cluster.yaml
```

Keep ChartMuseum reachable through the port-forward command above. The Application sample is intentionally excluded from `config/samples/kustomization.yaml`, so `kubectl apply -k config/samples` does not install a chart. After confirming the definition and target Cluster are ready, explicitly apply it:

```sh
kubectl --context kind-releaseyard-a apply -f config/samples/platform_v1alpha1_application.yaml
```

Only this explicit Application apply requests a Helm installation. Its release name is `releaseyard-demo-sample`, matching the Application name. Deleting the Application invokes its finalizer to uninstall the release; deletion can remain blocked while the managed cluster is unavailable, until the controller can complete cleanup.

### Local dashboard

The dashboard is an optional local Kind-only interface. The controller still runs locally with F5, and the dashboard runs as a separate Pod with its own ServiceAccount. It can list ApplicationDefinitions and Applications in `idp-system`, create Applications only in `idp-system`, and list Cluster-scoped Clusters. It requires a local administrator login (user `admin`); OIDC is not configured yet. Do not expose it outside this local setup or treat it as safe for production.

The platform team may set `ApplicationDefinition.spec.release.allowNamespaceOverride: true`. Only then does the creation form accept an optional target-cluster namespace (`Application.spec.releaseNamespace`); leaving it blank uses the Definition's `spec.release.namespace` (`applications` in the sample). The target namespace must already exist and cannot be changed after the Application is created. This does not change the management namespace `idp-system`.

Build and load the dashboard image, then install its independent Kustomize root in the management cluster:

```sh
docker build -f Dockerfile.dashboard -t releaseyard-dashboard:dev .
kind load docker-image releaseyard-dashboard:dev --name releaseyard-a
kubectl --context kind-releaseyard-a apply -k config/dashboard
kubectl --context kind-releaseyard-a -n idp-system rollout status deployment/releaseyard-dashboard
```

The Kustomize root includes a bootstrap Job that creates the local administrator Secret once. Read the generated password:

```sh
kubectl --context kind-releaseyard-a -n idp-system get secret releaseyard-dashboard-local-admin \
  -o jsonpath='{.data.initial-password}' | base64 -d
```

Keep a separate ChartMuseum port-forward on `127.0.0.1:18080` as described above. Open a second terminal for the dashboard port-forward:

```sh
kubectl --context kind-releaseyard-a -n idp-system port-forward service/releaseyard-dashboard 8080:8080 --address 127.0.0.1
```

Browse to <http://127.0.0.1:8080>. Sign in as `admin` with the generated password. The Secret is created once and is never overwritten on re-apply; the password plaintext is never mounted into the Pod.

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/idp:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/idp/<tag or branch>/dist/install.yaml
```

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
kubebuilder edit --plugins=helm/v2-alpha
```

2. See that a chart was generated under 'dist/chart', and users
can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

## Contributing
// TODO(user): Add detailed information on how you would like others to contribute to this project

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
