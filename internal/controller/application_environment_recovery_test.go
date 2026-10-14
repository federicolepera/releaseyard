package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestEnvironmentSuccessfulHelmActionRecoversStaleInputs(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		mutate func(*testing.T, *appTestEnvironment)
		setup  func(*testing.T, *appTestEnvironment)
	}{
		{
			name:   "application changed during apply",
			reason: "StaleOperation",
			mutate: func(t *testing.T, e *appTestEnvironment) {
				bumpApplicationGeneration(t, e)
			},
		},
		{
			name:   "definition changed during apply",
			reason: "StaleOperation",
			mutate: func(t *testing.T, e *appTestEnvironment) {
				definition := e.definition.DeepCopy()
				definition.Generation++
				if err := e.c.Update(context.Background(), definition); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "claim ownership changed during apply",
			reason: "OwnershipChanged",
			mutate: func(_ *testing.T, e *appTestEnvironment) {
				e.claims.verifyErr = errors.New("claim ownership changed")
			},
		},
		{
			name:   "Applied checkpoint conflict recovers successful revision",
			reason: "StaleOperation",
			setup: func(_ *testing.T, e *appTestEnvironment) {
				writer := &environmentStatusHookClient{Client: e.c}
				writer.beforeStatusUpdate = func(object client.Object) error {
					app, ok := object.(*platformv1alpha1.Application)
					if !ok || app.Status.Instances == nil {
						return nil
					}
					for _, instance := range app.Status.Instances {
						if instance.ClusterName == appTestCluster && instance.Applied != nil && !writer.failed {
							writer.failed = true
							return apierrors.NewConflict(schema.GroupResource{Group: platformv1alpha1.GroupVersion.Group, Resource: "applications"}, app.Name, errors.New("injected Applied checkpoint conflict"))
						}
					}
					return nil
				}
				e.r.Client = writer
			},
		},
		{
			name:   "authoritative recovery read failure stops safely",
			reason: "ApplyPending",
			setup: func(_ *testing.T, e *appTestEnvironment) {
				failReads := false
				e.r.APIReader = &environmentFailApplicationReader{Reader: e.r.APIReader, fail: &failReads}
				e.helm.afterApply = func() { failReads = true }
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newAppTestEnvironment(t, nil)
			app := e.getApp(t)
			app.Spec.ClusterRef = nil
			app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "production", Targets: []platformv1alpha1.ApplicationEnvironmentTarget{
				{Name: appTestCluster, UID: e.cluster.UID},
				{Name: "z-target", UID: types.UID("z-cluster-uid")},
			}}
			if err := e.c.Update(context.Background(), app); err != nil {
				t.Fatal(err)
			}
			secret := &corev1.Secret{}
			if err := e.reader.Get(context.Background(), types.NamespacedName{Namespace: testManagerNamespace, Name: testSecretName}, secret); err != nil {
				t.Fatal(err)
			}
			_, remoteTarget, err := parseRemoteClusterConfig(secret.Data["kubeconfig"], appTestReleaseNamespace)
			if err != nil {
				t.Fatal(err)
			}

			sibling := platformv1alpha1.ApplicationInstanceStatus{
				ClusterName:           "z-target",
				ClusterUID:            "z-cluster-uid",
				LastAttemptedIdentity: "sibling-attempt",
				Applied: &platformv1alpha1.ApplicationAppliedIdentity{
					ApplicationGeneration: 4,
					DesiredInputIdentity:  "sibling-applied",
					HelmRevision:          8,
				},
			}
			app = e.getApp(t)
			app.Status.EnvironmentBinding = &platformv1alpha1.ApplicationEnvironmentBinding{ApplicationDefinitionUID: e.definition.UID, ReleaseNamespace: appTestReleaseNamespace}
			app.Status.Instances = []platformv1alpha1.ApplicationInstanceStatus{sibling, {
				ClusterName:           appTestCluster,
				ClusterUID:            e.cluster.UID,
				Binding:               &platformv1alpha1.ApplicationBinding{ApplicationDefinitionUID: e.definition.UID, ClusterUID: e.cluster.UID, TargetAPIServer: remoteTarget.Server, TargetTrustHash: trustHash(remoteTarget.TrustSHA256), ReleaseNamespace: appTestReleaseNamespace, ReleaseName: app.Name, RemoteClaimUID: e.claims.claim.UID},
				LastAttemptedIdentity: "prior-attempt",
			}}
			if err := e.c.Status().Update(context.Background(), app); err != nil {
				t.Fatal(err)
			}
			if tt.setup != nil {
				tt.setup(t, e)
			}
			if tt.mutate != nil {
				e.helm.afterApply = func() { tt.mutate(t, e) }
			}

			result, err := e.r.Reconcile(context.Background(), e.request)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if result.RequeueAfter != staleInputRequeue {
				t.Fatalf("stale successful operation result = %#v, want short requeue", result)
			}
			if len(e.helm.apply) != 1 {
				t.Fatalf("later target was mutated after stale operation: Helm applies=%d", len(e.helm.apply))
			}

			got := e.getApp(t)
			if len(got.Status.Instances) != 2 {
				t.Fatalf("got %d instances, want target and sibling", len(got.Status.Instances))
			}
			var target, gotSibling *platformv1alpha1.ApplicationInstanceStatus
			for i := range got.Status.Instances {
				instance := &got.Status.Instances[i]
				switch instance.ClusterName {
				case appTestCluster:
					target = instance
				case "z-target":
					gotSibling = instance
				}
			}
			if tt.name == "authoritative recovery read failure stops safely" {
				if target == nil || target.Applied != nil {
					t.Fatalf("failed authoritative read unexpectedly wrote Applied: %#v", target)
				}
				if !applicationAggregateIsInvalid(got) {
					t.Fatalf("recovery read failure left root conditions green: %#v", got.Status.Conditions)
				}
			} else {
				if target == nil || target.Applied == nil || target.Applied.ApplicationGeneration != 7 || target.Applied.ApplicationDefinitionGeneration != 3 || target.Applied.HelmRevision != 1 {
					t.Fatalf("successful historical Helm result was not retained: target=%+v", target)
				}
				if target.LastAttemptedIdentity == "" {
					t.Fatal("target attempt identity was lost during recovery")
				}
				ready := conditionForEnvironmentRecovery(target.Conditions, "Ready")
				inventory := conditionForEnvironmentRecovery(target.Conditions, "InventoryReady")
				workload := conditionForEnvironmentRecovery(target.Conditions, "WorkloadHealthy")
				if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != tt.reason {
					t.Fatalf("target Ready condition does not identify stale operation: %#v", ready)
				}
				if inventory == nil || inventory.Status != metav1.ConditionUnknown || workload == nil || workload.Status != metav1.ConditionUnknown {
					t.Fatalf("stale operation was reported with current observation: inventory=%#v workload=%#v", inventory, workload)
				}
			}
			if gotSibling == nil || !reflect.DeepEqual(*gotSibling, sibling) {
				t.Fatalf("recovery changed sibling status: got %#v, want %#v", gotSibling, sibling)
			}
		})
	}
}

func applicationAggregateIsInvalid(app *platformv1alpha1.Application) bool {
	for _, typ := range []string{"Ready", "InventoryReady", "WorkloadHealthy"} {
		condition := conditionForEnvironmentRecovery(app.Status.Conditions, typ)
		if condition == nil || condition.Status == metav1.ConditionTrue {
			return false
		}
	}
	return true
}

type environmentStatusHookClient struct {
	client.Client
	beforeStatusUpdate func(client.Object) error
	afterStatusUpdate  func(client.Object)
	failed             bool
}

func (c *environmentStatusHookClient) Status() client.SubResourceWriter {
	return &environmentStatusHookWriter{SubResourceWriter: c.Client.Status(), client: c}
}

type environmentStatusHookWriter struct {
	client.SubResourceWriter
	client *environmentStatusHookClient
}

func (w *environmentStatusHookWriter) Update(ctx context.Context, object client.Object, options ...client.SubResourceUpdateOption) error {
	if w.client.beforeStatusUpdate != nil {
		if err := w.client.beforeStatusUpdate(object); err != nil {
			return err
		}
	}
	if err := w.SubResourceWriter.Update(ctx, object, options...); err != nil {
		return err
	}
	if w.client.afterStatusUpdate != nil {
		w.client.afterStatusUpdate(object)
	}
	return nil
}

type environmentFailApplicationReader struct {
	client.Reader
	fail *bool
}

func (r *environmentFailApplicationReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if r.fail != nil && *r.fail {
		if _, ok := object.(*platformv1alpha1.Application); ok {
			return errors.New("injected authoritative Application read failure")
		}
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func conditionForEnvironmentRecovery(conditions []metav1.Condition, typ string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == typ {
			return &conditions[i]
		}
	}
	return nil
}
