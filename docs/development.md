# Development

## Requirements

- Go 1.26+
- Docker
- Kind
- kubectl
- Helm
- Python 3

## Common commands

```bash
make help          # list targets
make manifests     # regenerate CRDs, RBAC, and webhook config
make generate      # regenerate DeepCopy code
make fmt           # go fmt
make vet           # go vet
make test          # unit and envtest suites
make lint          # golangci-lint
make test-e2e      # isolated Kind end-to-end suite
```

`make test` runs `manifests`, `generate`, `fmt`, and `vet` first, then the unit
and envtest suites.

`make test-e2e` creates its own Kind cluster (`idp-test-e2e` by default),
deploys the manager, and checks the manager, metrics endpoint, and `Resource`
controller. It removes the cluster afterwards.

## Local Kind workflow

The demo setup is described in [Getting started](getting-started.md).

### Rebuild the dashboard

```bash
bash hack/deploy-dashboard-kind.sh
```

This builds `releaseyard-dashboard:<timestamp>`, loads it into
`releaseyard-a`, updates the `releaseyard-dashboard` Deployment, and waits for
the rollout.

### Rebuild the controller

The current chart names the Deployment `<release>-controller-manager` with a
single `manager` container. Build and roll it manually:

```bash
tag="dev-$(date +%s)"
docker build -t "releaseyard-controller:${tag}" .
kind load docker-image "releaseyard-controller:${tag}" --name releaseyard-a
kubectl --context kind-releaseyard-a -n idp-system set image \
  deployment/idp-controller-manager manager="releaseyard-controller:${tag}"
kubectl --context kind-releaseyard-a -n idp-system rollout status \
  deployment/idp-controller-manager
```

`hack/deploy-controller-kind.sh` is useful only when the live Deployment matches
its expected name and containers (`idp-controller-manager` plus
`kind-loopback-bridge`). Check the live Deployment before using it.

## Tests

### Unit and envtest

```bash
make test
```

This requires the envtest binaries. The Makefile downloads them through
`setup-envtest`.

### End-to-end

```bash
make test-e2e
```

### Chart

```bash
helm lint dist/chart
```

The CI chart workflow builds the manager image, loads it into Kind, installs the
chart, and checks the release.

## Project layout

```text
api/v1alpha1/           CRD types
cmd/                    manager, dashboard, bootstrap admin
config/                 kustomize, CRDs, RBAC, samples, dashboard
dist/chart/             Helm chart
docs/                   documentation
examples/               cloud examples (S3, AWS EC2)
hack/                   local setup and deployment helpers
internal/chartfetch/    chart download, host allowlist, digest verification
internal/contract/      ApplicationDefinition and Application validation
internal/controller/    reconcilers
internal/dashboard/     HTTP server, auth, RBAC, web assets
test/e2e/               end-to-end suite
```

## Documentation

Keep the README as the entry point and put details in `docs/`. When behavior
changes, update the relevant page and any command examples.

## Before opening a pull request

```bash
make lint
make test
```

Add tests for behavior changes. For chart changes, run `helm lint dist/chart`.
