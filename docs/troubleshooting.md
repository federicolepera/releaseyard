# Troubleshooting

## Dashboard

### Login fails or the page is blank

Check the dashboard Pod and its logs:

```bash
kubectl -n idp-system get pods
kubectl -n idp-system logs deploy/releaseyard-dashboard
```

Confirm `dashboard.publicURL` matches the URL in the browser, including scheme
and port. The dashboard rejects unexpected hosts.

### `CSRF validation failed`

The page is usually holding a token from before a dashboard rollout. Sign out
and sign in again, or hard refresh the page. With the default in-memory session
store, a rollout invalidates every existing session. Configure the Redis session
store to keep sessions across rollouts.

### OIDC discovery fails

The dashboard Pod must reach the issuer's discovery and token endpoints. The
browser must reach the authorization endpoint. Check:

```bash
kubectl -n idp-system run oidc-check --rm -it --restart=Never \
  --image=curlimages/curl -- \
  curl -sS <issuer>/.well-known/openid-configuration
```

The redirect URL is `<dashboard.publicURL>/auth/callback`. Register it with the
identity provider.

For a Keycloak exposed only on `keycloak.localhost`, use the development-only
`dashboard.oidcLoopback` settings. Never enable them in production.

### A user sees no teams

Team namespaces are derived from OIDC groups as
`releaseyard-team-<sanitized-group>`. The user must be able to list Applications
in that namespace. Check the group membership and the RoleBinding created by
**Access applications**.

### Administration is unavailable

The panel requires the local administrator or OIDC RBAC that allows `create` on
`clusterrolebindings`. Add the group to `dashboard.adminGroups` with
`rbac.helpers.enabled=true`.

## Applications

### Chart fetch fails

Check `manager.allowedChartHosts` and that the controller can reach the chart
repository. The host must match exactly. The binary default is
`127.0.0.1:18080`; set it to your HTTP(S) Helm repository authority.

```bash
kubectl -n idp-system logs deploy/idp-controller-manager -c manager
kubectl get applicationdefinitions
```

### Application is not ready

Common causes:

- the target kubeconfig Secret is missing or invalid;
- the target namespace does not exist;
- the definition is not `Validated=True`;
- the chart digest changed for the same repository/name/version;
- the chart uses unsupported features (dependencies, hooks, CRDs,
  `values.schema.json`);
- the controller refuses to adopt a release it does not own.

```bash
kubectl -n <team-namespace> describe application <name>
kubectl -n <team-namespace> get application <name> -o yaml
kubectl -n idp-system get cluster <name> -o yaml
```

### Delete is stuck

The Application finalizer must uninstall the Helm release and remove its remote
claim. If the target cluster is unreachable or ownership is ambiguous, the
controller stops instead of deleting blindly. Restore connectivity and inspect
the conditions.

## Cloud resources

### Cloud page says provisioning is unavailable

Crossplane was not detected at dashboard startup. Install Crossplane, then
restart the dashboard Deployment.

### `ProviderConfig` not found

For namespaced Crossplane providers, the `ProviderConfig` must be in the same
namespace as the `Resource`. `CloudTarget.spec.providerConfigRef.namespace` must
match. The controller sets `providerConfigRef` by name only.

### Provider calls real AWS instead of LocalStack

Check the `ProviderConfig` endpoint:

- `endpoint.services` must include the service;
- `endpoint.source` must be `Custom`;
- use path-style S3;
- the skip flags from `examples/cloud-bucket/02-providerconfig.yaml` must be set.

### AWS authentication fails

Check the Secret referenced by the `ProviderConfig`:

```bash
kubectl -n <namespace> get secret <secret-name> -o yaml
kubectl -n <namespace> get providerconfig <name> -o yaml
```

The `creds` key must contain an AWS credentials file. Verify the IAM user or
role has the required permissions. Rotate any key that was exposed.

### Controller cannot create a provider resource

The controller ClusterRole needs the provider's exact API group, for example
`ec2.aws.m.upbound.io`. Kubernetes RBAC does not support suffix wildcards such
as `*.aws.m.upbound.io`. Add the rule or install the generated/aggregated
permissions.

### Deleting a resource leaves the cloud object

Check the rendered managed resource and its `spec.managementPolicies`. With
Crossplane v2, `["*"]` manages the full lifecycle. The examples set this in the
definition `base`.

## Metrics and logs

### CPU/memory are unavailable

Install `metrics-server` on the workload cluster:

```bash
bash hack/setup-managed-metrics.sh
```

On Kind the helper adds `--kubelet-insecure-tls`; do not use that flag in
production.

### No events

Kubernetes Events have a short, cluster-defined retention. The Activity tab
shows only what the API server still has.

## Useful commands

```bash
kubectl -n idp-system get pods
kubectl -n idp-system get deploy
kubectl -n idp-system logs deploy/idp-controller-manager -c manager
kubectl -n idp-system logs deploy/releaseyard-dashboard
kubectl get applications.platform.releaseyard.io -A
kubectl get resources.platform.releaseyard.io -A
kubectl get providers.pkg.crossplane.io
kubectl get providerconfigs.aws.m.upbound.io -A
```
