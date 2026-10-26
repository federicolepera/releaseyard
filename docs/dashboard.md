# Dashboard

The dashboard is served by the optional `releaseyard-dashboard` Deployment. It is
disabled by default in the Helm chart and starts with local administrator
authentication only.

## Pages

### Catalog

Browse `ApplicationDefinition`s visible to the signed-in user. Each entry shows
the definition description and its configurable values. Creating an Application
opens a form generated from the definition contract.

### Applications

List Applications in the caller's team scope. The page supports filtering,
status, placement, and a team selector. Opening an Application shows five tabs:

- **Overview** — status, placement, release information, and recent activity.
- **Resources** — rendered workload resources and their live state.
- **Configuration** — definition contract, values, rendered YAML, and history.
- **Observability** — live resource YAML, Pod logs, and Pod CPU/memory.
- **Activity** — Kubernetes events, warnings, and the latest deployment.

### Clusters

Show registered target clusters, connection conditions, nodes, and environment
labels. Platform administrators can change the environment label from this page.

### Cloud

Available when Crossplane is detected at dashboard startup. The page shows:

- provisioned `Resource` requests and their status;
- the `ResourceDefinition` catalog;
- registered `CloudTarget`s;
- recent cloud activity.

Creating a resource selects a definition, a target, and the definition's
configurable values. The Cloud detail page currently focuses on the Overview
tab; Configuration, Conditions, Outputs, Composed resources, and Events are
placeholders in the current build.

If Crossplane is installed after the dashboard starts, restart the dashboard
Deployment so detection runs again.

### Administration

- **Identity provider** — configure or remove OIDC.
- **Access definitions** — grant a group `get` on selected
  `ApplicationDefinition`s.
- **Cluster access** — grant a group `get` on selected `Cluster`s.
- **Access applications** — create a team namespace and bind the group to the
  Application editor role.

These pages create Kubernetes RBAC objects. There is no separate dashboard ACL.

## Authentication

A fresh install authenticates with the local `admin` account. The bootstrap Job
creates `<release>-dashboard-local-admin` with a bcrypt hash and a generated
initial password. Only the hash is mounted into the dashboard Pod.

OIDC is configured from the Administration page and stored in
`<release>-dashboard-identity-provider`. The dashboard applies it immediately,
without a restart. Once enabled, the login page offers both local and SSO login.

See [Access control](access-control.md).

## Sessions

Sessions are server-side:

- 30-minute sliding idle timeout;
- 8-hour absolute timeout;
- opaque, `HttpOnly`, `SameSite=Lax` cookies;
- `Secure` when the public URL is HTTPS;
- CSRF validation on state-changing requests.

Sessions and OIDC login transactions are stored in memory by default. For
production and for more than one replica, enable Redis:

```yaml
dashboard:
  replicas: 2
  sessionStore:
    type: redis
    redis:
      address: redis.example.com:6379
      passwordSecret:
        name: redis-auth
        key: password
      tls:
        enabled: true
```

The in-memory store signs everyone out on rollout and is not shared between
replicas. The Redis store shares sessions and login transactions and keeps users
signed in across rollouts.

## Team scope

For OIDC users, the dashboard derives candidate team namespaces from group
membership:

```text
releaseyard-team-<sanitized-group>
```

A namespace is selectable only if the impersonated user may list Applications
there. The local administrator uses the management namespace (`idp-system`) and
is not impersonated.

## Static assets

The dashboard serves its HTML, CSS, and JavaScript from the same binary. Static
assets skip Kubernetes authorization checks; API routes are always authorized.

## Related docs

- [Access control](access-control.md)
- [Applications](applications.md)
- [Observability](observability.md)
