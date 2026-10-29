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

The local administrator can read the management namespace and every Project
namespace. OIDC sessions also see Project namespaces where their Kubernetes RBAC
allows listing Applications.

## Project roles

Project access is assigned from the dashboard and stored as a `RoleBinding` in
the Project namespace:

| Role | Permissions |
|---|---|
| `releaseyard-project-viewer` | read Applications, ApplicationRequests and Resources |
| `releaseyard-project-developer` | viewer plus Application/Resource writes for direct delivery |
| `releaseyard-project-requester` | viewer plus ApplicationRequest creation for GitOps |
| `releaseyard-project-admin` | viewer plus RoleBinding management for viewer/developer/requester |

The project admin cannot grant `project-admin` or `platform-admin`. Catalog
visibility is controlled separately by `Project.spec.catalog`.

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

The Administration area is available to the local administrator, platform
admins, and—for the Project access page only—Project admins whose RBAC allows
RoleBinding creation. The dashboard checks actions as the impersonated user, so
the panel cannot escalate beyond existing permissions.

- **Project access** creates a RoleBinding in `Project.spec.namespace` for a
  group or user and one of the Project roles.
- **Platform admins** creates a ClusterRoleBinding to
  `releaseyard-platform-admin` for a group or user.

Revoking Project access removes only that RoleBinding. Revoking platform access
removes the ClusterRoleBinding. A fresh install has no platform-admin binding:
the local administrator assigns the first one from the dashboard.

## Helper roles

With `rbac.helpers.enabled=true`, the chart installs admin/editor/viewer roles
for Applications, Application Definitions, and Clusters, plus the Project
viewer/developer/requester/admin roles and `releaseyard-platform-admin`.

A fresh install has no OIDC platform admin. The local administrator assigns
`releaseyard-platform-admin` from **Administration → Platform admins**; those
bindings are ClusterRoleBindings managed by the dashboard. This requires
`rbac.helpers.enabled=true`.

## Application references

Kubernetes `resourceNames` restricts `get`, not the references made by
`create`. The chart therefore installs `ValidatingAdmissionPolicy` resources that
ask the API server whether the requesting user may `get`:

- on create: the referenced `ApplicationDefinition`; the referenced `Cluster`,
  every `Cluster` in `spec.clusters`, or every `Cluster` explicitly listed in a
  pinned environment target list;
- on update: only Clusters newly added to `spec.clusters`.

The policy applies to dashboard impersonation and to direct clients such as
`kubectl`. It uses `failurePolicy: Fail` by default, so a policy evaluation
failure rejects the request. Requires Kubernetes 1.31 or newer. Disable it only
for troubleshooting with `admission.enabled=false`; see
[Configuration](configuration.md).

## Audit logs

The dashboard writes structured JSON audit events to stdout. They are meant for
the cluster's log pipeline (for example Loki, Elastic, or CloudWatch), not for a
dashboard-managed database. Kubernetes Events remain the per-object activity
feed; the audit log is the security trail.

Events include:

- `auth.login.local` and `auth.login.oidc`, including failures and the reason;
- `auth.logout`;
- `api.write` for state-changing API requests;
- `admin.identity_provider.*`, `admin.definition_access.*`,
  `admin.project_access.*`, and `admin.platform_access.*` for administration
  changes.

Each event includes `event`, `outcome`, `actor`, `groups`, `auth_method`,
`impersonated`, `method`, `path`, `status`, `request_id`, `remote_addr`, and a
failure `reason` when applicable. The response also carries `X-Request-ID`, so a
browser request can be correlated with the audit line.

Passwords, CSRF tokens, ID tokens, client secrets, and request bodies are never
logged. Protect and retain the log sink according to your security policy.

## Known gaps

- Applications and ApplicationRequests can only be created in a namespace with
  the `platform.releaseyard.io/project` label. The Project controller owns that
  label; changing namespace labels is a platform-admin action.
- Access definitions and Cluster access are no longer group-based pages.
  ApplicationDefinition and Cluster entitlements belong to the Project catalog;
  admission-level catalog enforcement is the next webhook slice.
- Dynamic environment placement is authorized by the permission to create the
  Application in its Project namespace. The controller resolves the target
  Clusters with its own credentials, so per-cluster read access is not checked at
  create time. Add environment-level RBAC before exposing this to untrusted users.
- The dashboard can patch the environment label on Clusters, but RBAC cannot
  restrict a patch to one metadata key. Use an admission policy if the dashboard
  is exposed beyond a trusted environment.

## Related docs

- [Dashboard](dashboard.md)
- [Configuration](configuration.md)
- [Troubleshooting](troubleshooting.md)
