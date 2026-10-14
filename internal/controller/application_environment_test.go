package controller

import (
	"context"
	"testing"
	"time"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRunEnvironmentTargetsStopsAtStaleInputAndSortsCopy(t *testing.T) {
	targets := []platformv1alpha1.ApplicationEnvironmentTarget{{Name: "zeta", UID: "z"}, {Name: "alpha", UID: "a"}}
	var called []string
	stale := ctrlResultForEnvironmentTest()
	sorted, result, stopped, err := runEnvironmentTargets(targets, func(target platformv1alpha1.ApplicationEnvironmentTarget) (ctrl.Result, bool, error) {
		called = append(called, target.Name)
		return stale, false, nil
	})
	if err != nil || !stopped || result.RequeueAfter != stale.RequeueAfter {
		t.Fatalf("stale pass result = (%#v, %v, %v), want stopped with original result", result, stopped, err)
	}
	if len(called) != 1 || called[0] != "alpha" || sorted[0].Name != "alpha" || targets[0].Name != "zeta" {
		t.Fatalf("sorted/stopped processing called=%v sorted=%v original=%v", called, sorted, targets)
	}
}

func TestAggregateEnvironmentStatusSeparatesReleaseInventoryAndHealth(t *testing.T) {
	chart := platformv1alpha1.ApplicationChartIdentity{Repository: "https://charts.example", Name: "demo", Version: "1.0.0", Digest: "sha256:abc"}
	targets := []platformv1alpha1.ApplicationEnvironmentTarget{{Name: "one", UID: "uid-one"}, {Name: "two", UID: "uid-two"}}
	app := environmentAggregateFixture(targets, chart)
	setInstanceCondition(&app.Status.Instances[1], "WorkloadHealthy", metav1.ConditionFalse, applicationWorkloadDegraded, "Degraded")
	aggregateEnvironmentStatus(app, targets, "desired", chart, 3)
	if got := metaCondition(app, "Ready"); got == nil || got.Status != metav1.ConditionTrue {
		t.Fatalf("degraded workload must not change successful release readiness: %#v", got)
	}
	if got := metaCondition(app, "WorkloadHealthy"); got == nil || got.Status != metav1.ConditionFalse {
		t.Fatalf("expected aggregate unhealthy workload: %#v", got)
	}

	app = environmentAggregateFixture(targets, chart)
	setInstanceCondition(&app.Status.Instances[1], "InventoryReady", metav1.ConditionFalse, "CollectionFailed", "Failed")
	setInstanceCondition(&app.Status.Instances[1], "WorkloadHealthy", metav1.ConditionUnknown, "ObservationUnavailable", "Unknown")
	aggregateEnvironmentStatus(app, targets, "desired", chart, 3)
	if got := metaCondition(app, "Ready"); got == nil || got.Status != metav1.ConditionTrue {
		t.Fatalf("inventory failure must not change successful release readiness: %#v", got)
	}
	if got := metaCondition(app, "InventoryReady"); got == nil || got.Status != metav1.ConditionFalse {
		t.Fatalf("expected aggregate inventory failure: %#v", got)
	}
	if got := metaCondition(app, "WorkloadHealthy"); got == nil || got.Status != metav1.ConditionUnknown {
		t.Fatalf("expected aggregate workload unknown: %#v", got)
	}
}

func TestAggregateEnvironmentMissingInstanceIsNotHealthy(t *testing.T) {
	chart := platformv1alpha1.ApplicationChartIdentity{Repository: "https://charts.example", Name: "demo", Version: "1.0.0", Digest: "sha256:abc"}
	targets := []platformv1alpha1.ApplicationEnvironmentTarget{{Name: "one", UID: "uid-one"}, {Name: "missing", UID: "uid-missing"}}
	app := environmentAggregateFixture(targets[:1], chart)
	aggregateEnvironmentStatus(app, targets, "desired", chart, 3)
	if got := metaCondition(app, "Ready"); got == nil || got.Status != metav1.ConditionFalse {
		t.Fatalf("missing instance must not be Ready: %#v", got)
	}
	if got := metaCondition(app, "WorkloadHealthy"); got == nil || got.Status != metav1.ConditionUnknown {
		t.Fatalf("missing instance must not be healthy: %#v", got)
	}
}

func TestAggregateEnvironmentFutureObservationIsUnknown(t *testing.T) {
	chart := platformv1alpha1.ApplicationChartIdentity{Repository: "https://charts.example", Name: "demo", Version: "1.0.0", Digest: "sha256:abc"}
	targets := []platformv1alpha1.ApplicationEnvironmentTarget{{Name: "one", UID: "uid-one"}}
	app := environmentAggregateFixture(targets, chart)
	future := metav1.NewTime(time.Now().Add(time.Minute))
	app.Status.Instances[0].WorkloadObservedAt = &future
	aggregateEnvironmentStatus(app, targets, "desired", chart, 3)
	if got := metaCondition(app, "WorkloadHealthy"); got == nil || got.Status != metav1.ConditionUnknown {
		t.Fatalf("future observation timestamp must be unknown: %#v", got)
	}
	if got := metaCondition(app, "InventoryReady"); got == nil || got.Status != metav1.ConditionFalse {
		t.Fatalf("future observation must not leave inventory current: %#v", got)
	}
}

func TestAggregateEnvironmentInventoryRequiresFreshObservation(t *testing.T) {
	chart := platformv1alpha1.ApplicationChartIdentity{Repository: "https://charts.example", Name: "demo", Version: "1.0.0", Digest: "sha256:abc"}
	targets := []platformv1alpha1.ApplicationEnvironmentTarget{{Name: "one", UID: "uid-one"}}
	for _, test := range []struct {
		name string
		when time.Time
	}{
		{name: "expired", when: time.Now().Add(-6 * time.Minute)},
		{name: "future", when: time.Now().Add(time.Minute)},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := environmentAggregateFixture(targets, chart)
			observed := metav1.NewTime(test.when)
			app.Status.Instances[0].WorkloadObservedAt = &observed
			aggregateEnvironmentStatus(app, targets, "desired", chart, 3)
			if got := metaCondition(app, "InventoryReady"); got == nil || got.Status != metav1.ConditionFalse {
				t.Fatalf("stale observation must invalidate inventory: %#v", got)
			}
		})
	}
}

func TestAggregateEnvironmentNoWorkloadsIsUnknown(t *testing.T) {
	status, reason, _ := aggregateWorkloadHealth(nil)
	if status != metav1.ConditionUnknown || reason != "NoWorkloads" {
		t.Fatalf("empty resource observation should remain unknown, got %s/%s", status, reason)
	}
	status, reason, _ = aggregateEnvironmentHealth([]metav1.Condition{{Status: metav1.ConditionUnknown, Reason: "NoWorkloads"}})
	if status != metav1.ConditionUnknown || reason != "ObservationUnavailable" {
		t.Fatalf("environment no-workloads observation should aggregate unknown, got %s/%s", status, reason)
	}
}

func TestEnvironmentPreflightRejectsRecreatedClusterUID(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &platformv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "target", UID: types.UID("replacement")}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	r := &ApplicationReconciler{APIReader: c}
	err := r.preflightEnvironmentTargets(context.Background(), []platformv1alpha1.ApplicationEnvironmentTarget{{Name: "target", UID: types.UID("snapshot")}})
	if err == nil {
		t.Fatal("preflight adopted recreated Cluster UID")
	}
	if err := r.preflightEnvironmentTargets(context.Background(), []platformv1alpha1.ApplicationEnvironmentTarget{{Name: "not-present", UID: "missing"}}); err != nil {
		t.Fatalf("missing target should remain target-local, got %v", err)
	}
}

func environmentAggregateFixture(targets []platformv1alpha1.ApplicationEnvironmentTarget, chart platformv1alpha1.ApplicationChartIdentity) *platformv1alpha1.Application {
	app := &platformv1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Generation: 3}}
	for _, target := range targets {
		instance := platformv1alpha1.ApplicationInstanceStatus{ClusterName: target.Name, ClusterUID: target.UID, ObservedGeneration: 3,
			Applied: &platformv1alpha1.ApplicationAppliedIdentity{ApplicationDefinitionGeneration: 3, ApplicationGeneration: 3, Chart: chart, DesiredInputIdentity: "desired"}}
		now := metav1.NewTime(time.Now())
		instance.WorkloadObservedAt = &now
		setInstanceCondition(&instance, "Ready", metav1.ConditionTrue, "Ready", "Ready")
		setInstanceCondition(&instance, "InventoryReady", metav1.ConditionTrue, "Collected", "Collected")
		setInstanceCondition(&instance, "WorkloadHealthy", metav1.ConditionTrue, applicationWorkloadHealthy, "Healthy")
		app.Status.Instances = append(app.Status.Instances, instance)
	}
	return app
}

func metaCondition(app *platformv1alpha1.Application, typ string) *metav1.Condition {
	for i := range app.Status.Conditions {
		if app.Status.Conditions[i].Type == typ {
			return &app.Status.Conditions[i]
		}
	}
	return nil
}

func ctrlResultForEnvironmentTest() ctrl.Result { return ctrl.Result{RequeueAfter: time.Second} }
