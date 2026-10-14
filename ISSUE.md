# Deferred issues

## Create environment-targeted Applications with kubectl

The first version resolves environment membership and freezes the target-cluster list when an Application is created **through the dashboard**. Direct Kubernetes API clients can still create single-cluster Applications. To create an environment Application directly, they must supply an explicit concrete Cluster name/UID snapshot; the controller validates that snapshot but cannot guarantee it matched environment labels at CREATE time. Direct clients do not get the dashboard's automatic resolution or preview/drift check.

Add a CREATE admission webhook so `kubectl` and other clients receive the same server-side environment snapshot: resolve all matching Cluster names and UIDs, reject an empty or changed set, and preserve the existing single-cluster API. Scaffold the webhook with Kubebuilder, then cover its TLS/deployment, admission failure policy, CRD validation, and create-time race cases in isolated tests. Do not silently resolve labels during later reconciliation.

## Restrict environment-label edits beyond the dashboard API

The dashboard endpoint will modify only the `platform.releaseyard.io/environment` label, but Kubernetes RBAC can grant `patch` on Clusters only as a resource-level permission—not on one metadata key. If the dashboard is exposed beyond the trusted local setup, add authentication and a server-side admission policy that restricts which labels its ServiceAccount can change. Same-origin checks are not authentication.
