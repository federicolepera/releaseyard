# ReleaseYard documentation

Start with the [README](../README.md) for the product overview and quickstart.

| Page | What it covers |
|------|----------------|
| [Getting started](getting-started.md) | Local Kind demo, Helm install, dashboard access, cleanup |
| [Architecture](architecture.md) | Control plane, CRDs, reconciliation, safety model |
| [Dashboard](dashboard.md) | Pages, detail tabs, team scope, sessions |
| [Applications](applications.md) | Catalog contract, placement, values, release lifecycle |
| [Projects](projects.md) | Project, GitConnection e repository Git con struttura iniziale |
| [Cloud resources](cloud-resources.md) | Crossplane integration, targets, definitions, AWS examples |
| [Access control](access-control.md) | Local admin, OIDC, impersonation, team namespaces, RBAC |
| [Hardening](hardening.md) | Security and reliability backlog, priorities, acceptance criteria |
| [Observability](observability.md) | Live resources, logs, metrics, events, Prometheus |
| [Configuration](configuration.md) | CRD fields and Helm values |
| [Development](development.md) | Build, test, lint, e2e, local workflows |
| [Troubleshooting](troubleshooting.md) | Common failures and fixes |

## Suggested reading order

1. [Getting started](getting-started.md) to install the local demo.
2. [Architecture](architecture.md) to understand the control plane.
3. [Applications](applications.md) and [Cloud resources](cloud-resources.md) for
   the two delivery paths.
4. [Access control](access-control.md) before exposing the dashboard to teams.
5. [Configuration](configuration.md) as a reference while operating it.
