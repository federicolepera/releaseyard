# Access control

Authentication and authorization are separate:

- **Authentication** is handled by the dashboard: local password or OIDC.
- **Authorization** is the management cluster's Kubernetes RBAC. For OIDC users,
  the dashboard impersonates the signed-in user and their groups.

There is no separate dashboard permission store.

## Local administrator

A fresh installation starts with local authentication only.

- The bootstrap Job creates `<release>-dashboard-local-admin`.
- The Secret contains `password-hash` (bcrypt) and `initial-password`.
- Only the hash is mounted into the dashboard Pod.
- The local administrator is not impersonated and uses the dashboard
  ServiceAccount directly.
- The Secret is created once and is not overwritten on upgrade.

Read the password:

```bash
kubectl get secret <release>-dashboard-local-admin -n <namespace> \
  -o jsonpath='{.data.initial-password}' | base64 -d
```

Sign in as `admin`. Change the dashboard configuration from the Administration
page. The local account is the break-glass path.

## OIDC

Configure OIDC from **Administration → Identity provider**:

- issuer URL;
- client ID;
- client secret.

The dashboard stores the values in
`<release>-dashboard-identity-provider` and applies them immediately. Discovery
must succeed when saving. Removing the configuration falls back to local-only.

The redirect URL is `<dashboard.publicURL>/auth/callback`; register it with your
identity provider.

For a Keycloak exposed only on a localhost hostname, the chart has a
development-only `dashboard.oidcLoopback` aid. Never enable it in production.

## Sessions

- Server-side sessions with an opaque random identifier.
- 30-minute sliding idle timeout.
- 8-hour absolute timeout.
- Not capped by the OIDC ID token lifetime.
- Cookies are `HttpOnly`, `SameSite=Lax`, and `Secure` on HTTPS.
- State-changing requests require a valid CSRF token.

Sessions and in-flight OIDC login transactions live in memory by default. Set
`dashboard.sessionStore.type=redis` (Helm) or `SESSION_STORE=redis` (binary) to
share them across replicas and keep users signed in across rollouts. Redis also
stores the CSRF token and the ID token used as logout hint, so protect it with
network isolation, authentication, and TLS where possible.

With the in-memory store, a rollout invalidates every session and multiple
dashboard replicas do not share sessions. The chart refuses `dashboard.replicas`
greater than 1 unless the Redis store is selected.

If a write returns `CSRF validation failed`, sign out and sign in again, or hard
refresh the page. A stale page after a rollout is the usual cause.

## Teams

OIDC groups map to team namespaces by prefix:

```text
group: platform -> namespace: releaseyard-team-platform
```

The prefix is configurable with `dashboard.teamNamespacePrefix`. A namespace is
considered a team for the session only if the impersonated user may list
Applications there. The dashboard does not map groups to roles in application
code.

The local administrator works in `idp-system`, not in a team namespace.

## Impersonation

The dashboard ServiceAccount has `impersonate` on `users` and `groups`.

For OIDC sessions:

- writes use a client that impersonates the user and their groups;
- reads use the dashboard client, then filter results by the user's RBAC;
- capability flags (`isAdmin`, `canCreateApps`, etc.) come from
  `SelfSubjectAccessReview`.

This keeps the UI aligned with the API server. It also means the dashboard
ServiceAccount must be protected: `impersonate` is powerful. In production,
restrict it with `resourceNames` or constrained impersonation.

## Administration pages

The Administration area is available to the local administrator and to OIDC
users whose RBAC already permits the actions the panel performs. The dashboard
checks `create` on `clusterrolebindings` as the impersonated user, so the panel
cannot escalate beyond existing permissions.

- **Access definitions** creates a ClusterRole with `get` on selected
  `ApplicationDefinition`s (`resourceNames`) and binds it to a group.
- **Cluster access** creates a ClusterRole with `get` on selected `Cluster`s and
  binds it to a group.
- **Access applications** creates the team namespace and a RoleBinding to the
  Application editor ClusterRole.

Revoking application access removes the RoleBinding. The namespace and its
Applications remain.

## Helper roles

With `rbac.helpers.enabled=true`, the chart installs admin/editor/viewer roles
for Applications, Application Definitions, and Clusters. Bind them to OIDC
groups with normal Kubernetes RoleBindings or ClusterRoleBindings.

`dashboard.adminGroups` binds the shipped dashboard admin role to the listed
OIDC groups. This requires `rbac.helpers.enabled=true`.

## Application references

Kubernetes `resourceNames` restricts `get`, not the references made by
`create`. The chart therefore installs a `ValidatingAdmissionPolicy` that runs
on `Application` create and asks the API server whether the requesting user may
`get`:

- the referenced `ApplicationDefinition`;
- the referenced `Cluster`, or every `Cluster` in an environment target
  snapshot.

The policy applies to dashboard impersonation and to direct clients such as
`kubectl`. It uses `failurePolicy: Fail` by default, so a policy evaluation
failure rejects the request. Requires Kubernetes 1.31 or newer. Disable it only
for troubleshooting with `admission.enabled=false`; see
[Configuration](configuration.md).

## Known gaps

- Environment snapshots are resolved by the dashboard for environment
  Applications created through the UI. Direct clients must provide the snapshot.
  The admission policy verifies that every supplied target is readable, but it
  does not resolve or validate environment membership.
- The dashboard can patch the environment label on Clusters, but RBAC cannot
  restrict a patch to one metadata key. Use an admission policy if the dashboard
  is exposed beyond a trusted environment.

## Related docs

- [Dashboard](dashboard.md)
- [Configuration](configuration.md)
- [Troubleshooting](troubleshooting.md)
