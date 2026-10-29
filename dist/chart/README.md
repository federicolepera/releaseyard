# IDP Helm chart

This chart installs the IDP controller manager and can optionally install the
dashboard. The dashboard is disabled by default and, when enabled, starts with
local administrator authentication only. No identity provider is required to
install or use it.

When enabling it, configure `dashboard.image.repository`,
`dashboard.image.tag`, and `dashboard.publicURL` (the dashboard's canonical base
URL, for example `https://dashboard.example.com`). HTTP is accepted only for
`localhost` hosts.

## Local administrator bootstrap

With `dashboard.bootstrap.enabled` (the default) the chart runs a bounded
`pre-install,pre-upgrade` Job with a dedicated ServiceAccount and namespace Role
to create a stable Secret named `<release>-dashboard-local-admin` only if it is
absent. Because it is a pre-install hook, the Secret exists before the dashboard
Deployment that mounts it, so `helm install --wait` completes normally. The Job
does not print or expose the generated password in Helm values, manifests, or
logs.

The Secret contains a bcrypt password hash (`password-hash`) and the generated
`initial-password`. Read the password with:

```bash
kubectl get secret <release>-dashboard-local-admin -n <namespace> \
  -o jsonpath='{.data.initial-password}' | base64 -d
```

Sign in at the dashboard as user `admin`. The Secret is created once and is never
overwritten on upgrade; the password plaintext is not mounted into the dashboard
Pod. Treat the initial password as sensitive. The Secret persists after Helm
uninstall unless the namespace is deleted.

## OIDC configuration

OIDC is not configured through Helm values. After installing, sign in as the
local administrator and set the issuer URL, client ID, and client secret from
the **Administration** page. The dashboard stores them in the Secret
`<release>-dashboard-identity-provider` and applies them immediately, without a
restart. Once configured, the login page also offers single sign-on; removing
the configuration falls back to local-only.

The chart grants the dashboard ServiceAccount permission to `create` Secrets and
to `get/update/patch/delete` only that identity provider Secret. It cannot read
other Secrets in the namespace.

The dashboard Pod must be able to reach the issuer's discovery and token
endpoints, and the browser must be able to reach the authorization endpoint. For
a Keycloak that is only exposed on a localhost hostname, enable the
development-only loopback aid (never in production).

Recommended: point the hostAliases at the in-cluster ingress ClusterIP so no
sidecar is needed. `hack/setup-oidc-loopback.sh` wires this up automatically (it
also adds the matching port to the ingress Service):

```yaml
dashboard:
  oidcLoopback:
    enabled: true
    hostnames:
      - keycloak.localhost
    hostAliasIP: <ingress ClusterIP>
    listenPort: 18080
    sidecar:
      enabled: false
```

If the Pod cannot reach the ingress directly, keep `hostAliasIP: 127.0.0.1` and
enable the socat-style sidecar that forwards `listenPort` to an in-cluster
service:

```yaml
dashboard:
  oidcLoopback:
    enabled: true
    hostnames:
      - keycloak.localhost
    hostAliasIP: 127.0.0.1
    listenPort: 18080
    sidecar:
      enabled: true
      image:
        repository: releaseyard-kind-socat
        tag: dev
        pullPolicy: IfNotPresent
      targetHost: releaseyard-keycloak-service.keycloak-operator.svc.cluster.local
      targetPort: 8080
```

Configure the OIDC issuer with the same hostname and port, for example
`http://keycloak.localhost:18080/realms/releaseyard`.

## Session storage

The dashboard keeps sessions and OIDC login transactions in memory by default.
That is suitable for one replica only: a rollout signs everyone out and
replicas do not share sessions.

For production and HA, use Redis:

```yaml
dashboard:
  replicas: 2
  sessionStore:
    type: redis
    redis:
      address: redis.example.com:6379
      username: ""
      db: 0
      passwordSecret:
        name: redis-auth
        key: password
      tls:
        enabled: true
      keyPrefix: "releaseyard:dashboard:"
```

Create the password Secret separately; the chart never writes Redis credentials
into Helm values. Redis itself is not deployed by the chart: use a managed
service or deploy it separately. The chart fails when `dashboard.replicas` is
greater than 1 without the Redis store, or when Redis is selected without an
address.

## Audit logs

The dashboard writes structured JSON audit events to stdout. They cover local
and OIDC login outcomes, logout, state-changing API calls, and administration
changes, with actor, groups, auth method, impersonation flag, HTTP status,
request ID, and remote address. Passwords, tokens, secrets, and request bodies
are never logged. Ship the container logs to your central log pipeline.

## Authorization (management cluster RBAC)

Authorization is the management cluster's Kubernetes RBAC. The dashboard
impersonates the logged-in user, so the API server decides what they may do.
Any authenticated user can read the catalog; writes are allowed only when the
user's RBAC permits them. The dashboard ServiceAccount is granted `impersonate`
on `users`/`groups`; restrict this in production (for example with
`resourceNames` or constrained impersonation).

The chart ships user-facing roles when `rbac.helpers.enabled=true`
(`<release>-application-admin/editor/viewer-role` and the same for
`applicationdefinition` and `cluster`). Bind them to your OIDC groups, for
example:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: developers-application-editor
  namespace: idp-system
subjects:
  - kind: Group
    name: releaseyard:developers
    apiGroup: rbac.authorization.k8s.io
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: <release>-application-editor-role
```

### Administration area

The Administration pages (Identity provider, Project access, and Platform admins)
are available to the built-in local administrator and to
OIDC users whose RBAC already allows the actions the panel performs (the
dashboard checks `create` on `clusterrolebindings` as the impersonated user, so
the panel cannot escalate privileges).

To let an OIDC group administer the dashboard, assign it from
**Administration → Platform admins** after the fresh install. The legacy
`dashboard.adminGroups` value is no longer supported; use the local
administrator to assign the first platform admin.
(requires `rbac.helpers.enabled=true`). The chart binds the shipped
`<release>-dashboard-admin-role` to each group:

```yaml
dashboard:
  # removed: use Administration -> Platform admins
    - releaseyard:administrators
```

### Observability

The Application detail page can show live resources (YAML with status), Pod logs
and Pod CPU/memory read from the managed cluster. This requires two things:

- the dashboard ServiceAccount may `get` the managed clusters' kubeconfig
  Secrets (the chart grants `get` on Secrets in the release namespace); and
- `metrics-server` runs on the managed clusters for the CPU/memory section.
  `hack/setup-managed-metrics.sh` installs it on the Kind workload clusters. When
  metrics-server is absent the dashboard reports it instead of failing.

The controller keeps using its own kubeconfig (`Cluster.spec.kubeconfigSecretRef`)
to install into managed clusters; there is no per-user authorization on the
target clusters.

## Application reference enforcement

The chart installs a `ValidatingAdmissionPolicy` that rejects an `Application`
create when the requesting user cannot `get` the referenced
`ApplicationDefinition`, the referenced `Cluster`, or any `Cluster` explicitly
listed in a pinned environment target list. Dynamic environment targets are
resolved by the controller, not supplied by the client. The policy applies to
dashboard impersonation and direct clients such as `kubectl`.

```yaml
admission:
  enabled: true
  failurePolicy: Fail
```

Requires Kubernetes 1.31 or newer. `failurePolicy: Fail` rejects a create when
the authorization check cannot be evaluated. Set `admission.enabled=false` only
for troubleshooting.

## Other options

The optional Ingress is disabled by default. Enabling it requires an Ingress
class, hostname, and an existing TLS Secret; the dashboard is routed at `/`.
