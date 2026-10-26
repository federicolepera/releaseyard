# Hardening backlog

Security and reliability work that is not yet implemented. Keep this list
current when an item is completed or its risk changes.

Last reviewed: 2026-10-26

Status:

- `[ ]` open
- `[~]` in progress
- `[x]` done

## Baseline already in place

- `clusterRef` and `environment` are immutable; explicit `spec.clusters` targets can change only while keeping each Cluster UID.
- Admission checks readable `ApplicationDefinition` and explicitly selected
  Clusters (`clusterRef`, `spec.clusters`, and pinned environment targets).
- OIDC uses discovery, PKCE, state, nonce, and a short-lived login transaction.
- Dashboard cookies are `HttpOnly`, `Secure` on HTTPS, and `SameSite=Lax`;
  state-changing requests require a CSRF token and same-origin validation.
- OIDC/local login and write APIs emit JSON audit events with request IDs and
  without passwords, tokens, secrets, or request bodies.
- Request bodies are size-limited, unknown JSON fields are rejected, and the
  dashboard serves restrictive CSP/frame/referrer headers.
- Dashboard, manager, and bootstrap pods run non-root with a read-only root
  filesystem, dropped capabilities, and the `RuntimeDefault` seccomp profile.
- Remote Cluster connections reject insecure TLS, redirects, proxies, and
  kubeconfigs without CA data.
- CI workflows pin Actions by commit SHA and use minimal `permissions`.

## P0 — Dashboard process

- [ ] **Add HTTP server timeouts and graceful shutdown.**
      `cmd/dashboard/main.go` uses `http.ListenAndServe` without timeouts.
      Use an explicit `http.Server` with `ReadHeaderTimeout`, `ReadTimeout`,
      `WriteTimeout`, `IdleTimeout`, and `MaxHeaderBytes`; handle SIGTERM.
      Acceptance: a slow-header client cannot hold a connection indefinitely and
      the pod drains cleanly during rollout.

- [ ] **Rate-limit authentication.**
      There is no brute-force protection on the local admin or OIDC login paths.
      Add a per-IP and per-username limiter, shared through Redis when replicas
      are greater than one, returning `429` with audit events.
      Acceptance: repeated failures are throttled across replicas and a
      successful login does not inherit an unlimited budget.

- [ ] **Allow disabling local administrator login in production.**
      The bootstrap Job creates a local admin Secret by default.
      Provide a supported way to disable local login while keeping emergency
      recovery documented.
      Acceptance: OIDC-only deployments do not expose `/auth/local`.

## P0 — Least privilege

- [ ] **Reduce dashboard ServiceAccount privileges.**
      `dist/chart/templates/dashboard/rbac.yaml` grants read access to every
      Secret in the release namespace, broad ClusterRole/ClusterRoleBinding
      management, namespace creation, and impersonation of any user or group.
      Split privileged administration into a separate ServiceAccount or process,
      move managed-cluster kubeconfigs to a dedicated credentials namespace, and
      constrain impersonation where the Kubernetes version supports it.
      Acceptance: compromising the read-only dashboard path does not grant
      cluster-wide RBAC or Secret access.

- [ ] **Authorize dynamic environment placement per environment.**
      Creating an Application in a dynamic environment is currently authorized
      only by the ability to create the Application; the controller then resolves
      matching Clusters with its own credentials.
      Introduce an `Environment` resource with RBAC, or an equivalent
      authorization check that requires access to the environment and its
      resolved Clusters before reconciliation.
      Acceptance: a user cannot deploy into an environment they cannot read or
      manage, including through direct CRD creation.

## P1 — Network and sessions

- [ ] **Add chart-managed NetworkPolicies.**
      The chart currently ships only a metrics ingress policy and leaves
      `networkPolicy.enabled=false`. Add default-deny ingress/egress with
      explicit allows for DNS, the Kubernetes API, Redis, ingress, and metrics.
      Add an isolated Redis policy.
      Acceptance: dashboard, manager, Redis, and metrics continue to work with
      default-deny enabled.

- [ ] **Harden Redis session storage.**
      Redis is external and optional; TLS support currently sets only a minimum
      version. Require authentication and TLS for production, support a custom
      CA and server name, document that session data includes CSRF material and
      OIDC state, and add Redis network isolation and monitoring.
      Acceptance: a documented production profile cannot start Redis sessions
      without authentication and verified TLS.

- [ ] **Harden ingress and admin exposure.**
      Document and enforce HTTPS, HSTS, redirects, and optional IP allowlists for
      administration pages. Keep the existing localhost-only HTTP development
      exception.
      Acceptance: a production public URL cannot serve cookies or admin pages
      over plain HTTP.

## P1 — Supply chain and runtime

- [ ] **Pin deployed images by digest.**
      Controller, dashboard, and OIDC loopback images are configured by tag.
      Support `repository@sha256:...` consistently and prefer digests in the
      production values example.
      Acceptance: a chart install can pin every container image by digest.

- [ ] **Add image assurance to CI.**
      Generate an SBOM, scan images, and sign published images. Keep the current
      SHA-pinned Actions and minimal permissions.
      Acceptance: release images have verifiable provenance and a failing
      vulnerability gate for high-severity findings.

- [ ] **Apply Pod Security Standards and resource limits.**
      Add `pod-security.kubernetes.io/enforce=restricted` guidance or labels for
      platform and team namespaces, and add CPU/memory requests and limits for the
      dashboard containers.
      Acceptance: namespaces can be labeled `restricted` and all shipped pods
      pass; the dashboard has bounded resources.

## P2 — Cloud, audit, and operations

- [ ] **Close cloud RBAC wildcard gaps (issue #7).**
      Generate provider/region-scoped Crossplane RBAC instead of broad wildcard
      permissions. Keep Crossplane as an external dependency.
      Acceptance: cloud users can create only the resource kinds and scopes they
      are authorized for.

- [ ] **Ship audit logs to an immutable sink.**
      JSON audit lines currently go to stdout. Forward them to a retained,
      access-controlled log sink with documented retention and alerting.
      Acceptance: audit records survive pod and node loss and are not editable by
      dashboard users.

- [ ] **Add security alerts.**
      Alert on repeated login failures, rate-limit saturation, session-store
      failures, reconciliation errors, stale targets, and failed uninstalls.
      Acceptance: each signal has a dashboard/query and a runbook entry.

- [ ] **Expand negative end-to-end tests.**
      Cover unreadable explicitly selected Clusters, unauthorized dynamic
      environments, disabled local admin, Redis outage, and expired sessions.
      Acceptance: the e2e suite fails if any authorization check regresses.

## Deferred operational checks

- [ ] Rotate the AWS access key used by the local demo.
- [ ] Re-verify the SSM and cloud-resource delete paths after credential rotation.
- [ ] Remove the local administrator Secret from any long-lived demo cluster after
      OIDC is working.
