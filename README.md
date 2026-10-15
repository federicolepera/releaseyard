# ReleaseYard

ReleaseYard is a Kubernetes-native operator **and** a web dashboard that turn a curated Helm catalog into self-service application deployment across multiple Kubernetes clusters.

A platform team registers the target clusters and publishes reusable templates (`ApplicationDefinition`). A team then picks a template from the catalog and creates an `Application`; the operator installs, updates, and uninstalls the matching Helm release on the selected cluster — one release per Application.

## What it is

- **Operator** — reconciles three custom resources on a management cluster:
  - `Cluster` — a registered target cluster, reached through a kubeconfig Secret.
  - `ApplicationDefinition` — a pinned chart from an HTTP(S) ChartMuseum repository, with the target namespace, default values, and the typed value paths an Application may override.
  - `Application` — a request to deploy that chart on a chosen cluster.
- **Dashboard** — a web UI for both platform admins and teams: browse the catalog, create and manage Applications, inspect Activity, and configure access.
- **Authentication & authorization** — sign in with OIDC (or a built-in local administrator); what a user can do is decided by ordinary Kubernetes RBAC on the management cluster.

## Ideas behind it

- One Application = one Helm release on one cluster, named after the team and the Application.
- The value surface is explicit: an Application can only override the types and limits its definition declares.
- Teams are isolated by namespace (`releaseyard-team-<group>`). The administrator grants each group exactly the definitions and clusters it may use, all through Kubernetes RBAC.
- Chart bytes are pinned by digest before Helm ever sees them.

## Getting started (local Kind demo)

Requirements: Docker, `kubectl`, Kind, Go, Helm and Python 3.

```sh
# Creates the management cluster (releaseyard-a), two workload clusters
# (releaseyard-managed-a/b) and publishes the demo chart to ChartMuseum.
bash hack/setup-kind-workloads.sh
```

Then pick one:

- **Operator** — use the examples in `config/samples/` to register a `Cluster`, apply an `ApplicationDefinition`, and create an `Application`.
- **Dashboard** — build and install it, then open the UI:

```sh
docker build -f Dockerfile.dashboard -t releaseyard-dashboard:dev .
kind load docker-image releaseyard-dashboard:dev --name releaseyard-a
kubectl --context kind-releaseyard-a apply -k config/dashboard
kubectl --context kind-releaseyard-a -n idp-system port-forward service/releaseyard-dashboard 8080:8080
```

Read the generated local administrator password and sign in as `admin`:

```sh
kubectl --context kind-releaseyard-a -n idp-system get secret releaseyard-dashboard-local-admin \
  -o jsonpath='{.data.initial-password}' | base64 -d
```

## Distribution

- **YAML bundle** — `make build-installer IMG=<registry>/idp:tag` writes `dist/install.yaml`, installable with a single `kubectl apply -f`.
- **Helm chart** — generated under `dist/chart` by the `helm/v2-alpha` plugin.

## Contributing

Run `make help` for the available targets. This project is built on [Kubebuilder](https://book.kubebuilder.io).

## License

Copyright 2026. Licensed under the [Apache License, Version 2.0](http://www.apache.org/licenses/LICENSE-2.0).
