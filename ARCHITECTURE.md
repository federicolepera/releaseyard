# Architecture: Dashboard Authentication

## Overview

A fresh installation authenticates only through the built-in local
administrator. No identity provider is required to install or use the
dashboard. An administrator can later configure OIDC from the administration
panel; from then on the login page offers both the local administrator and
single sign-on.

Authentication and authorization are deliberately separate:

- **Authentication** is done by the dashboard (local password or OIDC ID token).
- **Authorization** is the management cluster's **Kubernetes RBAC** for
  authenticated OIDC users. The dashboard impersonates the logged-in user, so
  the API server decides what they may do. Any authenticated user may read the
  catalog; writes (Applications, Cluster environment, identity provider) are
  allowed only when the user's RBAC permits them.
- The controller keeps using its own kubeconfig (`Cluster.spec.kubeconfigSecretRef`)
  to install into the managed clusters; there is no per-user authorization on the
  target clusters.

## Local administrator credentials

**Bootstrap (out-of-band):**

- `internal/dashboard/local_auth.go` generates a random password (32 random
  bytes, base64url), hashes it with bcrypt (cost 12), and creates the Secret
  `<release>-dashboard-local-admin` with keys `password-hash` and
  `initial-password`. `EnsureLocalAdminSecret` is idempotent and validates an
  existing Secret without overwriting it.
- `cmd/bootstrapadmin/main.go` runs that logic from a Job. In the Helm chart
  the Job and its ServiceAccount/Role/RoleBinding are `pre-install,pre-upgrade`
  hooks with weights, so the Secret exists before the dashboard Deployment that
  mounts it. `dashboard.bootstrap.enabled` defaults to `true` when the dashboard
  is enabled.

**Delivery to the dashboard Pod:**

- The Secret is mounted read-only at `/etc/dashboard/local-admin`, exposing only
  the `password-hash` key. The plaintext `initial-password` is never mounted.
- `LOCAL_ADMIN_HASH_PATH` points at that file; `DASHBOARD_PUBLIC_URL` is the
  dashboard's canonical base URL.

## Backend (Go)

**File: `internal/dashboard/auth.go`**

- `authHandler` is authentication-method agnostic. It owns the session store,
  cookies, CSRF validation, host checks, and route authorization.
- The active OIDC configuration is held in an `atomic.Pointer[oidcRuntime]`, so
  it can be replaced at runtime without restarting the process. `nil` means OIDC
  is not configured.
- `NewLocalHandler` builds the handler and loads any stored OIDC configuration
  at startup; the local administrator is always available. `NewOIDCHandler`
  pins a provider directly and is used by tests.
- `dashboardSession` records `username` and `groups` (from the OIDC token) plus
  `authMethod`, which decides both impersonation and logout behavior. The local
  administrator session is not impersonated and uses the dashboard
  ServiceAccount directly (break-glass, fully privileged).

**File: `internal/dashboard/rbac.go`**

- `ImpersonatingClientFactory` builds a Kubernetes client that authenticates as
  the dashboard ServiceAccount but impersonates the user (`Impersonate-User`)
  and their groups (`Impersonate-Group`, always including
  `system:authenticated`). Clients are cached per identity.
- `capabilities` answers what the session may do with a
  `SelfSubjectAccessReview` performed as the impersonated user, so the UI flags
  (`isAdmin`, `canCreateApps`, `canDeleteApps`, `canUpdateApps`) always reflect
  the cluster's RBAC. There is no group-to-role mapping in the application.
- State-changing requests are executed with the impersonated client; reads (the
  catalog) use the dashboard's own client so every authenticated user can browse.

**File: `internal/dashboard/local_login.go`**

- `localLogin` reads the hash lazily (so the Pod may start before the bootstrap
  Job), verifies the password, and creates a session for user `admin`. Local
  sessions are treated as fully privileged (capabilities are reported as true)
  and are not impersonated.
- Fail-closed: a missing or invalid hash returns `503`; a wrong password returns
  `401`. A rejected login never issues a session cookie.

**File: `internal/dashboard/identity_provider.go`**

- Stores the OIDC configuration in the Secret
  `<release>-dashboard-identity-provider` (keys `issuer-url`, `client-id`,
  `client-secret`). The name is provided through
  `IDENTITY_PROVIDER_SECRET_NAME`.
- `loadIdentityProvider` / `saveIdentityProvider` / `deleteIdentityProvider`
  manage that Secret; a configuration is "enabled" only when all three keys are
  present.
- `buildOIDCRuntime` validates the issuer (HTTP allowed only for localhost),
  derives the redirect URL as `<publicURL>/auth/callback`, and performs OIDC
  discovery. Discovery failure rejects the save.
- On a successful save the new runtime is stored atomically and is active
  immediately; `DELETE` removes the Secret and falls back to local-only.

**Routes:**

| Method | Path | Behavior |
| --- | --- | --- |
| GET | `/healthz` | Liveness/readiness, public |
| GET | `/login` | Redirect to `/login.html`; redirects to `/auth/login` when only OIDC is available |
| GET | `/login.html`, `/login.js`, `/style.css`, `/theme-init.js` | Public assets |
| GET | `/api/auth-methods` | Public: `{local, oidc, redirectURL}` for the login page |
| POST | `/auth/local` | Verify password, set session cookie |
| GET | `/auth/login` | Begin OIDC login (PKCE); redirects to `/login` when OIDC is not configured |
| GET | `/auth/callback` | OIDC callback |
| GET | `/api/session` | Session + capability flags + CSRF token |
| POST | `/auth/logout` | Delete session; OIDC sessions return an RP-initiated logout URL |
| GET/PUT/DELETE | `/api/administration/identity-provider` | Initial local administrator only; read/save/remove the OIDC configuration |

`app.js` is deliberately not loaded by the login page, because it treats a
missing session as a redirect to the login page. Unauthenticated browser
requests redirect to `/login`; unauthenticated `/api/*` requests return JSON
`401`. The administration area (`/administration.html` and
`/api/administration/identity-provider`) is, for now, reserved to the initial
local administrator: OIDC users cannot see or use it. The client secret is
never returned.

## Frontend (JavaScript)

**Files: `internal/dashboard/web/login.html`, `login.js`**

- A minimal form posts the password to `/auth/local` with `fetch`, shows inline
  errors, and navigates to `/` on success.
- `login.js` calls `/api/auth-methods`; when OIDC is enabled it reveals a
  "Sign in with identity provider" button that starts `/auth/login`.
- `app.js` redirects to `/login` on `401` and the "Sign in again" link points to
  `/login`.

**File: `internal/dashboard/web/administration.html` (+ `app.js`)**

- The identity provider form reads and writes
  `/api/administration/identity-provider`, shows the redirect URL to register in
  the provider, and allows removing the OIDC configuration.

## Helm chart

- `dashboard.publicURL` is required when the dashboard is enabled; HTTP is
  accepted only for `localhost` hosts.
- The dashboard Deployment mounts the local-admin Secret (`optional: true`) and
  sets `DASHBOARD_PUBLIC_URL`, `LOCAL_ADMIN_HASH_PATH`, and
  `IDENTITY_PROVIDER_SECRET_NAME`.
- The dashboard Role may `create` Secrets and `get/update/patch/delete` only the
  identity provider Secret; it cannot read other Secrets.
- The dashboard ClusterRole grants `impersonate` on `users`/`groups`. This is
  powerful: in production restrict it with `resourceNames` or use constrained
  impersonation. Bind the shipped user-facing roles
  (`<release>-application-admin/editor/viewer-role`, etc., installed with
  `rbac.helpers.enabled=true`) to your OIDC groups to grant access.
- `dashboard.oidcLoopback` (default off) is a development-only aid: it adds a
  `hostAliases` entry and a socat loopback sidecar so the Pod can reach an
  identity provider exposed only on a localhost hostname. Never enable it in
  production.
- The bootstrap hook creates the local-admin Secret once; it is never
  overwritten on upgrade.

## Deferred to later phases

- Password rotation / forced first-login change and brute-force throttling.
- Per-user authorization on the managed clusters (identity propagation / target
  impersonation); today the controller deploys with its own kubeconfig.
- Reference authorization: RBAC does not check that an Application references a
  Cluster/Definition the user may access (see `ISSUE.md`); an admission webhook
  is needed.
