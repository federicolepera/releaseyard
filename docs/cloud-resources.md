# Cloud resources

ReleaseYard can expose cloud resources through the same self-service model used
for Applications. The integration is optional and separate:

- **ReleaseYard** is the front door and the API.
- **Crossplane** reconciles the cloud resources.
- **The cloud provider** creates the actual objects.

> [!IMPORTANT]
> The ReleaseYard Helm chart never installs Crossplane. Install Crossplane and
> its providers separately, as platform infrastructure.

## Prerequisites

1. Crossplane is installed on the management cluster.
2. The required provider is installed, for example
   `crossplane-contrib-provider-aws-ec2:v2.0.0`.
3. A namespaced Crossplane `ProviderConfig` exists with credentials.
4. The controller has RBAC for the provider's managed-resource API group.
5. The dashboard was started or restarted after Crossplane was installed.

Crossplane provider CRDs and the provider's `ProviderConfig` are external
objects. ReleaseYard only reads and writes the managed resources described by a
`ResourceDefinition`.

## Objects

| Object | Scope | Role |
|--------|-------|------|
| `CloudTarget` | Cluster | Provider, region, and `ProviderConfig` reference. |
| `ResourceDefinition` | Cluster | Managed-resource templates, mappings, and outputs. |
| `Resource` | Namespaced | A team request with values. |

### CloudTarget

```yaml
apiVersion: platform.releaseyard.io/v1alpha1
kind: CloudTarget
metadata:
  name: aws-prod
spec:
  provider: aws
  region: eu-south-1
  providerConfigRef:
    name: aws-prod
    namespace: releaseyard-team-platform
```

`provider` is currently limited to `aws`. The `ProviderConfig` must be in the
same namespace as the `Resource` that uses the target.

### ResourceDefinition

```yaml
apiVersion: platform.releaseyard.io/v1alpha1
kind: ResourceDefinition
metadata:
  name: aws-bucket
spec:
  description: An S3 bucket managed by Crossplane.
  configurableValues:
    - path: bucketName
      type: string
      required: true
  resources:
    - name: bucket
      apiVersion: s3.aws.m.upbound.io/v1beta1
      kind: Bucket
      base:
        spec:
          managementPolicies:
            - "*"
      mappings:
        - from: values.bucketName
          to: "metadata.annotations[crossplane.io/external-name]"
        - from: target.region
          to: spec.forProvider.region
  outputs:
    - name: arn
      template: bucket
      from: status.atProvider.arn
```

- `base` is merged into every rendered managed resource.
- `mappings` copy `values.*` or `target.*` into a field path.
- `outputs` copy a field from a composed resource status into
  `Resource.status.outputs`.
- `template` selects the resource template used by an output. When omitted, the
  first template is used.
- `managementPolicies: ["*"]` is the Crossplane v2 way to manage creation and
  deletion. Without deletion in the policy, removing the `Resource` may leave the
  cloud object behind.

### Resource

```yaml
apiVersion: platform.releaseyard.io/v1alpha1
kind: Resource
metadata:
  name: demo-bucket
  namespace: releaseyard-team-platform
spec:
  definitionRef:
    name: aws-bucket
  targetRef:
    name: localstack
  values:
    bucketName: demo-bucket
```

The controller validates values against the definition, renders the managed
resources, and records their names in `status.composed`.

## Real AWS EC2 example

[`examples/aws-ec2/`](../examples/aws-ec2/) contains a complete EC2 definition:

```text
00-provider.yaml        Crossplane AWS EC2 provider
01-providerconfig.yaml  credentials reference
02-cloudtarget.yaml     CloudTarget aws-prod
03-resourcedefinition.yaml  ResourceDefinition aws-ec2-milan
```

The definition exposes `instanceName` and `instanceType`. The AMI, subnet, IAM
instance profile, root volume, and public-IP setting are fixed in the golden
path. Adjust them for your account before applying.

Apply in order:

```bash
kubectl apply -f examples/aws-ec2/00-provider.yaml
kubectl apply -f examples/aws-ec2/01-providerconfig.yaml
kubectl apply -f examples/aws-ec2/02-cloudtarget.yaml
kubectl apply -f examples/aws-ec2/03-resourcedefinition.yaml
```

Create the credentials Secret separately. It must be in the same namespace as
the `ProviderConfig`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: aws-prod-creds
  namespace: releaseyard-team-platform
stringData:
  creds: |
    [default]
    aws_access_key_id = <key>
    aws_secret_access_key = <secret>
```

Never commit real credentials. Use a dedicated IAM user or, preferably,
workload identity/IRSA. Rotate any key that was pasted into a terminal or chat.

## LocalStack S3 example

[`examples/cloud-bucket/`](../examples/cloud-bucket/) uses LocalStack:

```bash
bash hack/setup-crossplane-localstack.sh
kubectl apply -f examples/cloud-bucket/03-cloudtarget.yaml
kubectl apply -f examples/cloud-bucket/04-resourcedefinition.yaml
kubectl apply -f examples/cloud-bucket/05-resource.yaml
```

The setup script installs LocalStack, the S3 provider, the fake credentials
Secret, and the `ProviderConfig` (manifests `00`–`02`). LocalStack accepts any
credentials. Its `ProviderConfig` also needs
`endpoint.services`, `endpoint.source: Custom`, path-style S3, and the
validation-skip flags shown in the example. Never use that endpoint or those
flags in production.

## Dashboard

When Crossplane is detected, the **Cloud** page lists resources, definitions,
targets, and recent activity. Create a resource by selecting a definition, a
target, and values. The detail page currently shows the Overview; the remaining
tabs are placeholders.

Detection runs at dashboard startup. Install Crossplane first, or restart the
dashboard afterwards.

## Deletion

Deleting a `Resource` cascades through the owner references to the managed
resources. Crossplane then applies the definition's `managementPolicies`. With
`["*"]`, the cloud object is deleted.

There is no ReleaseYard finalizer on cloud resources. If a managed resource is
orphaned, check the Crossplane object and the provider configuration.

## RBAC

The controller needs permission for each managed-resource API group, for
example `ec2.aws.m.upbound.io`. Kubernetes RBAC has no suffix wildcard for API
groups; adding a provider may require updating the controller ClusterRole.

The long-term plan is to generate or aggregate these permissions from the
installed provider CRDs. See the repository issues for the tracking item.

## Limitations

- `CloudTarget.spec.provider` is currently `aws` only.
- Outputs are strings read from `status.atProvider`.
- Per-composed-resource status is not yet exposed in `status.composed`.
- Cloud observability (CPU, memory, provider events) is not implemented.
- The Cloud detail tabs beyond Overview are placeholders.

## Related docs

- [Architecture](architecture.md)
- [Configuration](configuration.md)
- [Troubleshooting](troubleshooting.md)
