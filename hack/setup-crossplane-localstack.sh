#!/usr/bin/env bash
#
# Development-only helper: wire Crossplane to a local LocalStack instance so the
# AWS provider can create real resources without an AWS account or credentials.
#
# Crossplane is NOT installed by this script. It must already be installed on the
# control plane; this script only installs the AWS S3 provider package, the
# credentials Secret and the ProviderConfig that points the provider at
# LocalStack. LocalStack accepts any access key, so the credentials are fake.
#
# Never use the LocalStack endpoint, fake credentials or the skip_* settings in
# production. See .dev/cloud-provisioning-with-crossplane.md.

set -Eeuo pipefail

TEAM_NAMESPACE="${TEAM_NAMESPACE:-releaseyard-team-platform}"
LOCALSTACK_NAMESPACE="${LOCALSTACK_NAMESPACE:-localstack}"
PROVIDER_NAME="${PROVIDER_NAME:-crossplane-contrib-provider-aws-s3-s3}"
PROVIDER_PACKAGE="${PROVIDER_PACKAGE:-xpkg.crossplane.io/crossplane-contrib/provider-aws-s3:v2.0.0}"
PROVIDER_CONFIG="${PROVIDER_CONFIG:-localstack}"
CREDS_SECRET="${CREDS_SECRET:-localstack-creds}"
ENDPOINT="${ENDPOINT:-http://localstack.${LOCALSTACK_NAMESPACE}.svc.cluster.local:4566}"

for tool in kubectl; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		printf 'Required command not found: %s\n' "$tool" >&2
		exit 1
	fi
done

if ! kubectl get crd compositions.apiextensions.crossplane.io >/dev/null 2>&1; then
	printf 'Crossplane is not installed on this cluster.\n' >&2
	printf 'Install it separately; this script does not manage Crossplane.\n' >&2
	exit 1
fi

printf 'Installing LocalStack in namespace %s\n' "$LOCALSTACK_NAMESPACE"
kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ${LOCALSTACK_NAMESPACE}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: localstack
  namespace: ${LOCALSTACK_NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: localstack
  template:
    metadata:
      labels:
        app: localstack
    spec:
      containers:
        - name: localstack
          image: localstack/localstack:3
          ports:
            - containerPort: 4566
          env:
            - name: SERVICES
              value: s3
          readinessProbe:
            httpGet:
              path: /_localstack/health
              port: 4566
            initialDelaySeconds: 5
            periodSeconds: 5
          resources:
            requests:
              cpu: 100m
              memory: 256Mi
            limits:
              memory: 1Gi
---
apiVersion: v1
kind: Service
metadata:
  name: localstack
  namespace: ${LOCALSTACK_NAMESPACE}
spec:
  selector:
    app: localstack
  ports:
    - port: 4566
      targetPort: 4566
EOF
kubectl -n "$LOCALSTACK_NAMESPACE" rollout status deployment/localstack --timeout=240s

printf 'Waiting for LocalStack S3 to become available\n'
for _ in $(seq 1 60); do
	if kubectl -n "$LOCALSTACK_NAMESPACE" exec deployment/localstack -- awslocal s3 ls >/dev/null 2>&1; then
		break
	fi
	sleep 2
done
kubectl -n "$LOCALSTACK_NAMESPACE" exec deployment/localstack -- awslocal s3 ls >/dev/null

printf 'Installing AWS S3 provider %s\n' "$PROVIDER_PACKAGE"
kubectl apply -f - <<EOF
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: ${PROVIDER_NAME}
spec:
  package: ${PROVIDER_PACKAGE}
EOF
kubectl wait --for=condition=Healthy "provider/${PROVIDER_NAME}" --timeout=300s

kubectl get namespace "$TEAM_NAMESPACE" >/dev/null 2>&1 || kubectl create namespace "$TEAM_NAMESPACE"

printf 'Creating fake credentials in namespace %s\n' "$TEAM_NAMESPACE"
kubectl -n "$TEAM_NAMESPACE" apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${CREDS_SECRET}
stringData:
  creds: |
    [default]
    aws_access_key_id = test
    aws_secret_access_key = test
EOF

printf 'Creating ProviderConfig %s pointing at %s\n' "$PROVIDER_CONFIG" "$ENDPOINT"
kubectl -n "$TEAM_NAMESPACE" apply -f - <<EOF
apiVersion: aws.m.upbound.io/v1beta1
kind: ProviderConfig
metadata:
  name: ${PROVIDER_CONFIG}
spec:
  credentials:
    source: Secret
    secretRef:
      namespace: ${TEAM_NAMESPACE}
      name: ${CREDS_SECRET}
      key: creds
  endpoint:
    hostnameImmutable: true
    source: Custom
    services:
      - s3
    url:
      type: Static
      static: ${ENDPOINT}
  s3_use_path_style: true
  skip_credentials_validation: true
  skip_metadata_api_check: true
  skip_region_validation: true
  skip_requesting_account_id: true
EOF

printf '\nCrossplane -> LocalStack is ready.\n'
printf '  provider:       %s\n' "$PROVIDER_NAME"
printf '  namespace:      %s\n' "$TEAM_NAMESPACE"
printf '  ProviderConfig: %s\n' "$PROVIDER_CONFIG"
printf '  endpoint:       %s\n' "$ENDPOINT"
printf '\nCreate a test bucket with:\n'
printf '  kubectl -n %s apply -f - <<YAML\n' "$TEAM_NAMESPACE"
printf '  apiVersion: s3.aws.m.upbound.io/v1beta1\n'
printf '  kind: Bucket\n'
printf '  metadata:\n'
printf '    name: demo-bucket\n'
printf '  spec:\n'
printf '    forProvider:\n'
printf '      region: us-east-1\n'
printf '    providerConfigRef:\n'
printf '      name: %s\n' "$PROVIDER_CONFIG"
printf '      kind: ProviderConfig\n'
printf '    managementPolicies: ["*"]\n'
printf '  YAML\n'
