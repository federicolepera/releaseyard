package controller

import (
	"context"
	"errors"
	"testing"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	"github.com/federicolepera/idp/internal/releaseclaim"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type perTargetDeleteClaims struct {
	deleted map[types.UID]bool
	deletes map[types.UID]int
}

func (m *perTargetDeleteClaims) Acquire(_ context.Context, identity releaseclaim.Identity) (releaseclaim.Claim, error) {
	return releaseclaim.Claim{Namespace: identity.ReleaseNamespace, Name: releaseclaim.ClaimName(identity.ReleaseName), UID: types.UID("claim-" + string(identity.ClusterUID))}, nil
}

func (m *perTargetDeleteClaims) Verify(_ context.Context, identity releaseclaim.Identity, claim releaseclaim.Claim) error {
	if m.deleted[identity.ClusterUID] || claim.UID != types.UID("claim-"+string(identity.ClusterUID)) {
		return releaseclaim.ErrOwnershipConflict
	}
	return nil
}

func (m *perTargetDeleteClaims) Delete(_ context.Context, identity releaseclaim.Identity, _ releaseclaim.Claim) error {
	m.deletes[identity.ClusterUID]++
	if m.deleted[identity.ClusterUID] {
		return releaseclaim.ErrClaimNotFound
	}
	m.deleted[identity.ClusterUID] = true
	return nil
}

type environmentDeleteConflictClient struct {
	client.Client
	conflict bool
}

func (c *environmentDeleteConflictClient) Status() client.SubResourceWriter {
	return environmentDeleteConflictWriter{SubResourceWriter: c.Client.Status(), owner: c}
}

type environmentDeleteConflictWriter struct {
	client.SubResourceWriter
	owner *environmentDeleteConflictClient
}

func (w environmentDeleteConflictWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if w.owner.conflict {
		w.owner.conflict = false
		return apierrors.NewConflict(schema.GroupResource{Group: platformv1alpha1.GroupVersion.Group, Resource: "applications"}, obj.GetName(), nil)
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func prepareBoundEnvironmentDeletion(t *testing.T, e *appTestEnvironment, targets []platformv1alpha1.ApplicationEnvironmentTarget, claimUID types.UID) {
	t.Helper()
	secret := &corev1.Secret{}
	if err := e.reader.Get(context.Background(), types.NamespacedName{Namespace: testManagerNamespace, Name: testSecretName}, secret); err != nil {
		t.Fatal(err)
	}
	_, targetIdentity, err := parseRemoteClusterConfig(secret.Data["kubeconfig"], appTestReleaseNamespace)
	if err != nil {
		t.Fatal(err)
	}
	app := e.getApp(t)
	app.Spec.ClusterRef = nil
	app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "production", Targets: targets}
	if err := e.c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	app = e.getApp(t)
	app.Status.EnvironmentBinding = &platformv1alpha1.ApplicationEnvironmentBinding{ApplicationDefinitionUID: e.definition.UID, ReleaseNamespace: appTestReleaseNamespace}
	app.Status.Instances = make([]platformv1alpha1.ApplicationInstanceStatus, 0, len(targets))
	for _, target := range targets {
		app.Status.Instances = append(app.Status.Instances, platformv1alpha1.ApplicationInstanceStatus{
			ClusterName: target.Name, ClusterUID: target.UID,
			Binding: &platformv1alpha1.ApplicationBinding{ApplicationDefinitionUID: e.definition.UID, ClusterUID: target.UID, TargetAPIServer: targetIdentity.Server, TargetTrustHash: trustHash(targetIdentity.TrustSHA256), ReleaseNamespace: appTestReleaseNamespace, ReleaseName: appTestName, RemoteClaimUID: claimUID},
			Applied: &platformv1alpha1.ApplicationAppliedIdentity{ApplicationGeneration: app.Generation}, LastAttemptedIdentity: "attempt",
		})
	}
	if err := e.c.Status().Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(context.Background(), e.getApp(t)); err != nil {
		t.Fatal(err)
	}
}

func TestEnvironmentDeletionUninstallsThenDeletesClaimCheckpointed(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	app := e.getApp(t)
	app.Spec.ClusterRef = nil
	app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "production", Targets: []platformv1alpha1.ApplicationEnvironmentTarget{{Name: appTestCluster, UID: e.cluster.UID}}}
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
	app = e.getApp(t)
	app.Status.EnvironmentBinding = &platformv1alpha1.ApplicationEnvironmentBinding{ApplicationDefinitionUID: e.definition.UID, ReleaseNamespace: appTestReleaseNamespace}
	app.Status.Instances = []platformv1alpha1.ApplicationInstanceStatus{{ClusterName: appTestCluster, ClusterUID: e.cluster.UID, Binding: &platformv1alpha1.ApplicationBinding{ApplicationDefinitionUID: e.definition.UID, ClusterUID: e.cluster.UID, TargetAPIServer: remoteTarget.Server, TargetTrustHash: trustHash(remoteTarget.TrustSHA256), ReleaseNamespace: appTestReleaseNamespace, ReleaseName: appTestName, RemoteClaimUID: e.claims.claim.UID}, Applied: &platformv1alpha1.ApplicationAppliedIdentity{ApplicationGeneration: app.Generation}, LastAttemptedIdentity: "attempt"}}
	if err := e.c.Status().Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(context.Background(), e.getApp(t)); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	checkpoint := e.getApp(t).Status.Instances[0]
	if !releaseAbsent(&checkpoint) || checkpoint.CleanupComplete || len(e.helm.uninstall) != 1 || e.claims.deletes != 0 {
		t.Fatalf("release absence was not checkpointed before claim deletion: instance=%#v uninstall=%d deletes=%d", checkpoint, len(e.helm.uninstall), e.claims.deletes)
	}
	e.reconcile(t)
	if e.claims.deletes != 1 || len(e.helm.uninstall) != 1 || !e.claims.deleted {
		t.Fatalf("claim deletion retry repeated release operation: deletes=%d uninstall=%d", e.claims.deletes, len(e.helm.uninstall))
	}
}

func TestClusterEventMapsEnvironmentSnapshotTargetWithoutLabels(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	app := e.getApp(t)
	app.Spec.ClusterRef = nil
	app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "snapshot", Targets: []platformv1alpha1.ApplicationEnvironmentTarget{{Name: e.cluster.Name, UID: e.cluster.UID}}}
	if err := e.c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	requests := e.r.mapCluster(context.Background(), e.cluster)
	if len(requests) != 1 || requests[0].NamespacedName != e.request.NamespacedName {
		t.Fatalf("snapshot Cluster event mapped to %#v, want %s", requests, e.request.NamespacedName)
	}
}

func TestEnvironmentDeleteStatusConflictStopsBeforeLaterTarget(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	second := &platformv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "zz-target", UID: types.UID("second-cluster-uid"), Generation: 1}, Spec: platformv1alpha1.ClusterSpec{KubeconfigSecretRef: platformv1alpha1.KubeconfigSecretReference{Name: testSecretName}}}
	if err := e.c.Create(context.Background(), second); err != nil {
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
	app := e.getApp(t)
	app.Spec.ClusterRef = nil
	app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "production", Targets: []platformv1alpha1.ApplicationEnvironmentTarget{{Name: appTestCluster, UID: e.cluster.UID}, {Name: second.Name, UID: second.UID}}}
	if err := e.c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	app = e.getApp(t)
	app.Status.EnvironmentBinding = &platformv1alpha1.ApplicationEnvironmentBinding{ApplicationDefinitionUID: e.definition.UID, ReleaseNamespace: appTestReleaseNamespace}
	makeBinding := func(clusterUID types.UID) *platformv1alpha1.ApplicationBinding {
		return &platformv1alpha1.ApplicationBinding{ApplicationDefinitionUID: e.definition.UID, ClusterUID: clusterUID, TargetAPIServer: remoteTarget.Server, TargetTrustHash: trustHash(remoteTarget.TrustSHA256), ReleaseNamespace: appTestReleaseNamespace, ReleaseName: appTestName, RemoteClaimUID: e.claims.claim.UID}
	}
	app.Status.Instances = []platformv1alpha1.ApplicationInstanceStatus{
		{ClusterName: appTestCluster, ClusterUID: e.cluster.UID, Binding: makeBinding(e.cluster.UID), Applied: &platformv1alpha1.ApplicationAppliedIdentity{ApplicationGeneration: app.Generation}, LastAttemptedIdentity: "attempt"},
		{ClusterName: second.Name, ClusterUID: second.UID, Binding: makeBinding(second.UID), Applied: &platformv1alpha1.ApplicationAppliedIdentity{ApplicationGeneration: app.Generation}, LastAttemptedIdentity: "attempt"},
	}
	if err := e.c.Status().Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(context.Background(), e.getApp(t)); err != nil {
		t.Fatal(err)
	}
	conflicting := &environmentDeleteConflictClient{Client: e.c, conflict: true}
	e.r.Client = conflicting
	e.reconcile(t)
	if conflicting.conflict || len(e.helm.uninstall) != 1 || e.claims.verifies != 1 || e.claims.deletes != 0 {
		t.Fatalf("stale cleanup continued to later target: conflictRemaining=%t uninstalls=%d verifies=%d deletes=%d", conflicting.conflict, len(e.helm.uninstall), e.claims.verifies, e.claims.deletes)
	}
	if !containsString(e.getApp(t).Finalizers, applicationFinalizer) {
		t.Fatal("status conflict removed the Application finalizer")
	}
}

func TestEnvironmentDeleteCleanupCompleteRequiresReleaseAndClaimCheckpoint(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	app := e.getApp(t)
	app.Spec.ClusterRef = nil
	app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "production", Targets: []platformv1alpha1.ApplicationEnvironmentTarget{{Name: "cluster-a", UID: types.UID("cluster-a-uid")}}}
	app.Status.EnvironmentBinding = &platformv1alpha1.ApplicationEnvironmentBinding{ApplicationDefinitionUID: e.definition.UID, ReleaseNamespace: appTestReleaseNamespace}
	app.Status.Instances = []platformv1alpha1.ApplicationInstanceStatus{{ClusterName: "cluster-a", ClusterUID: "cluster-a-uid", Binding: &platformv1alpha1.ApplicationBinding{ApplicationDefinitionUID: e.definition.UID, ClusterUID: "cluster-a-uid", ReleaseNamespace: appTestReleaseNamespace, ReleaseName: appTestName, RemoteClaimUID: e.claims.claim.UID}, CleanupComplete: true}}
	if allEnvironmentBindingsCleaned(app) || len(e.helm.uninstall) != 0 || e.claims.deletes != 0 {
		t.Fatalf("CleanupComplete without ReleaseAbsent checkpoint was treated as clean: %#v absent=%t", app.Status.Instances, releaseAbsent(&app.Status.Instances[0]))
	}
}

func TestEnvironmentDeleteAcquiresPersistsAndReselectsClaimBinding(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	prepareBoundEnvironmentDeletion(t, e, []platformv1alpha1.ApplicationEnvironmentTarget{{Name: appTestCluster, UID: e.cluster.UID}}, "")
	e.reconcile(t)
	checkpoint := e.getApp(t).Status.Instances[0]
	if checkpoint.Binding == nil || checkpoint.Binding.RemoteClaimUID != e.claims.claim.UID || !releaseAbsent(&checkpoint) || checkpoint.CleanupComplete {
		t.Fatalf("claim UID/ReleaseAbsent checkpoint was not persisted: %#v", checkpoint)
	}
	e.reconcile(t)
	if e.claims.acquires != 1 || e.claims.deletes != 1 || len(e.helm.uninstall) != 1 {
		t.Fatalf("retry did not resume after ReleaseAbsent: acquire=%d delete=%d uninstall=%d", e.claims.acquires, e.claims.deletes, len(e.helm.uninstall))
	}
	if err := e.c.Get(context.Background(), e.request.NamespacedName, &platformv1alpha1.Application{}); err == nil {
		t.Fatal("finalizer was not removed after claim cleanup")
	}
}

func TestEnvironmentDeleteClaimDeleteFailureAndStatusConflictResumeFromReleaseAbsent(t *testing.T) {
	t.Run("claim delete failure", func(t *testing.T) {
		e := newAppTestEnvironment(t, nil)
		prepareBoundEnvironmentDeletion(t, e, []platformv1alpha1.ApplicationEnvironmentTarget{{Name: appTestCluster, UID: e.cluster.UID}}, e.claims.claim.UID)
		app := e.getApp(t)
		app.Status.Instances[0].Conditions = []metav1.Condition{{Type: "ReleaseAbsent", Status: metav1.ConditionTrue, Reason: "Uninstalled", ObservedGeneration: app.Generation}}
		if err := e.c.Status().Update(context.Background(), app); err != nil {
			t.Fatal(err)
		}
		e.claims.deleteErr = errors.New("temporary claim deletion failure")
		e.reconcile(t)
		if !containsString(e.getApp(t).Finalizers, applicationFinalizer) || len(e.helm.uninstall) != 0 || e.claims.acquires != 0 {
			t.Fatal("claim deletion failure did not preserve checkpoint and finalizer")
		}
		e.claims.deleteErr = nil
		e.reconcile(t)
		if e.claims.deletes != 2 || len(e.helm.uninstall) != 0 {
			t.Fatalf("claim deletion retry repeated Helm cleanup: deletes=%d uninstall=%d", e.claims.deletes, len(e.helm.uninstall))
		}
	})

	t.Run("status conflict after successful claim deletion", func(t *testing.T) {
		e := newAppTestEnvironment(t, nil)
		prepareBoundEnvironmentDeletion(t, e, []platformv1alpha1.ApplicationEnvironmentTarget{{Name: appTestCluster, UID: e.cluster.UID}}, e.claims.claim.UID)
		app := e.getApp(t)
		app.Status.Instances[0].Conditions = []metav1.Condition{{Type: "ReleaseAbsent", Status: metav1.ConditionTrue, Reason: "Uninstalled", ObservedGeneration: app.Generation}}
		if err := e.c.Status().Update(context.Background(), app); err != nil {
			t.Fatal(err)
		}
		conflicting := &environmentDeleteConflictClient{Client: e.c, conflict: true}
		e.r.Client = conflicting
		e.reconcile(t)
		if !containsString(e.getApp(t).Finalizers, applicationFinalizer) || !e.claims.deleted || len(e.helm.uninstall) != 0 || e.claims.acquires != 0 {
			t.Fatal("post-delete status conflict did not retain a resumable finalizer checkpoint")
		}
		e.r.Client = e.c
		e.reconcile(t)
		if e.claims.deletes != 2 || e.claims.acquires != 0 || len(e.helm.uninstall) != 0 {
			t.Fatalf("resume after claim-delete conflict repeated prior work: deletes=%d acquire=%d uninstall=%d", e.claims.deletes, e.claims.acquires, len(e.helm.uninstall))
		}
		if err := e.c.Get(context.Background(), e.request.NamespacedName, &platformv1alpha1.Application{}); err == nil {
			t.Fatal("finalizer did not clear after idempotent claim-delete retry")
		}
	})
}

func TestEnvironmentDeletePartialTargetRecoveryPreservesSiblingStatus(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	claims := &perTargetDeleteClaims{deleted: map[types.UID]bool{}, deletes: map[types.UID]int{}}
	e.r.ClaimManagerFactory = func(kubernetes.Interface) releaseClaimManager { return claims }
	reachable := &platformv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "a-reachable", UID: types.UID("reachable-uid"), Generation: 1}, Spec: platformv1alpha1.ClusterSpec{KubeconfigSecretRef: platformv1alpha1.KubeconfigSecretReference{Name: testSecretName}}}
	if err := e.c.Create(context.Background(), reachable); err != nil {
		t.Fatal(err)
	}
	missing := platformv1alpha1.ApplicationEnvironmentTarget{Name: "z-unavailable", UID: types.UID("unavailable-uid")}
	prepareBoundEnvironmentDeletion(t, e, []platformv1alpha1.ApplicationEnvironmentTarget{{Name: reachable.Name, UID: reachable.UID}, missing}, "")
	e.reconcile(t)
	first := e.getApp(t)
	if !containsString(first.Finalizers, applicationFinalizer) || len(e.helm.uninstall) != 1 || !releaseAbsent(&first.Status.Instances[0]) {
		t.Fatalf("reachable target was not checkpointed while sibling unavailable: instances=%#v uninstall=%d", first.Status.Instances, len(e.helm.uninstall))
	}
	if first.Status.Instances[0].ClusterName != reachable.Name || first.Status.Instances[1].ClusterName != missing.Name {
		t.Fatalf("sibling instances were lost/reordered unexpectedly: %#v", first.Status.Instances)
	}
	e.reconcile(t)
	completed := e.getApp(t)
	if claims.deletes[reachable.UID] != 1 || len(e.helm.uninstall) != 1 || len(completed.Status.Instances) != 2 || !completed.Status.Instances[0].CleanupComplete || completed.Status.Instances[1].Binding == nil || completed.Status.Instances[1].ClusterUID != missing.UID {
		t.Fatalf("reachable target was not completed independently: deletes=%d uninstall=%d", claims.deletes[reachable.UID], len(e.helm.uninstall))
	}
	restored := &platformv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: missing.Name, UID: missing.UID, Generation: 1}, Spec: platformv1alpha1.ClusterSpec{KubeconfigSecretRef: platformv1alpha1.KubeconfigSecretReference{Name: testSecretName}}}
	if err := e.c.Create(context.Background(), restored); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	e.reconcile(t)
	if claims.deletes[reachable.UID] != 1 || claims.deletes[missing.UID] != 1 || len(e.helm.uninstall) != 2 {
		t.Fatalf("recovered target cleanup calls deletes=%#v uninstalls=%d", claims.deletes, len(e.helm.uninstall))
	}
	if err := e.c.Get(context.Background(), e.request.NamespacedName, &platformv1alpha1.Application{}); err == nil {
		t.Fatal("finalizer remained after all snapshot targets were cleaned")
	}
}
