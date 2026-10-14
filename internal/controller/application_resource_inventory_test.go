package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
)

const (
	appsAPIGroup   = "apps"
	appsAPIVersion = "apps/v1"
)

func TestCollectApplicationResourcesIncludesChartRootsAndOwnedDescendants(t *testing.T) {
	deployment := inventoryObject("apps/v1", "Deployment", "demo", "web", "deploy-uid", nil)
	rs := inventoryObject(appsAPIVersion, "ReplicaSet", "demo", "web-rs", "rs-uid", owner("Deployment", "web", "deploy-uid"))
	pod := inventoryObject("v1", "Pod", "demo", "web-pod", "pod-uid", owner("ReplicaSet", "web-rs", "rs-uid"))
	unrelated := inventoryObject(appsAPIVersion, "ReplicaSet", "demo", "web-other-rs", "other-rs", owner("Deployment", "web", "some-other-uid"))
	unrelatedPod := inventoryObject("v1", "Pod", "demo", "web-other-pod", "other-pod", owner("ReplicaSet", "web-rs", "wrong-uid"))
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Group: appsAPIGroup, Version: "v1", Resource: "replicasets"}: "ReplicaSetList",
		{Version: "v1", Resource: "pods"}:                             "PodList",
	}, deployment,
		inventoryObject("v1", "ConfigMap", "demo", "settings", "config-uid", nil), rs, pod, unrelated, unrelatedPod)
	mapper := inventoryTestMapper()
	manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n"

	first, err := collectApplicationResourcesWithClients(context.Background(), client, mapper, manifest, "demo")
	if err != nil {
		t.Fatalf("collect resources: %v", err)
	}
	if len(first) != 4 {
		t.Fatalf("got %d resources, want 4: %#v", len(first), first)
	}
	wantNames := []string{"web", "web-rs", "settings", "web-pod"}
	for i, name := range wantNames {
		if first[i].Name != name {
			t.Errorf("resource[%d].Name = %q, want %q", i, first[i].Name, name)
		}
	}
	for _, resource := range first {
		if resource.Kind == applicationWorkloadPodKind && resource.ParentUID != types.UID("rs-uid") {
			t.Errorf("Pod ParentUID = %q, want rs-uid", resource.ParentUID)
		}
	}
	second, err := collectApplicationResourcesWithClients(context.Background(), client, mapper, manifest, "demo")
	if err != nil {
		t.Fatalf("collect resources second time: %v", err)
	}
	for i := range first {
		if !reflect.DeepEqual(first[i], second[i]) {
			t.Fatalf("result ordering/content changed at index %d", i)
		}
	}
}

func TestCollectApplicationResourcesKeepsMissingChartRootWithoutUID(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	resources, err := collectApplicationResourcesWithClients(context.Background(), client, inventoryTestMapper(), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: removed\n", "demo")
	if err != nil {
		t.Fatalf("collect resources: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("got %d resources, want 1", len(resources))
	}
	if resources[0].Name != "removed" || resources[0].UID != "" || resources[0].Namespace != "demo" {
		t.Fatalf("unexpected missing root identity: %#v", resources[0])
	}
}

func TestCollectApplicationResourcesProjectsStatefulSetPods(t *testing.T) {
	statefulSet := inventoryObject("apps/v1", "StatefulSet", "demo", "database", "stateful-uid", nil)
	pod := inventoryObject("v1", "Pod", "demo", "database-0", "pod-uid", owner("StatefulSet", "database", "stateful-uid"))
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{{Version: "v1", Resource: "pods"}: "PodList"}, statefulSet, pod)
	resources, err := collectApplicationResourcesWithClients(context.Background(), client, inventoryTestMapper(), "apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: database\n", "demo")
	if err != nil {
		t.Fatalf("collect resources: %v", err)
	}
	for _, resource := range resources {
		if resource.Kind == applicationWorkloadPodKind && resource.Pod == nil {
			t.Fatalf("StatefulSet Pod health projection missing: %#v", resource)
		}
	}
	if len(resources) != 2 {
		t.Fatalf("got %d inventory resources, want root and Pod metadata", len(resources))
	}
}

func TestCollectApplicationResourcesUsesOwnerUIDAndNamespaceForWorkloadHealth(t *testing.T) {
	statefulSet := inventoryObject("apps/v1", "StatefulSet", "demo", "database", "stateful-uid", nil)
	statefulSet.SetGeneration(1)
	statefulSet.Object[applicationSpecField] = map[string]any{applicationDefinitionReplicasPath: int64(1)}
	statefulSet.Object["status"] = map[string]any{applicationObservedGenerationField: int64(1), "replicas": int64(1), applicationReadyReplicasField: int64(1)}
	validPod := inventoryObject("v1", "Pod", "demo", "database-0", "valid-pod", owner("StatefulSet", "database", "stateful-uid"))
	validPod.Object["status"] = map[string]any{applicationPhaseField: "Failed"}
	wrongUIDPod := inventoryObject("v1", "Pod", "demo", "database-wrong", "wrong-uid-pod", owner("StatefulSet", "database", "other-stateful-uid"))
	otherNamespacePod := inventoryObject("v1", "Pod", "other", "database-foreign", "foreign-pod", owner("StatefulSet", "database", "stateful-uid"))
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{{Version: "v1", Resource: "pods"}: "PodList"}, statefulSet, validPod, wrongUIDPod, otherNamespacePod)
	resources, err := collectApplicationResourcesWithClients(context.Background(), client, inventoryTestMapper(), "apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: database\n", "demo")
	if err != nil {
		t.Fatalf("collect resources: %v", err)
	}
	if len(resources) != 2 {
		t.Fatalf("got %d resources, want root and its owned Pod", len(resources))
	}
	for _, resource := range resources {
		if resource.Kind == applicationWorkloadPodKind && (resource.Name != "database-0" || resource.Pod == nil) {
			t.Fatalf("unexpected projected Pod: %#v", resource)
		}
		if resource.Kind == applicationWorkloadStatefulSetKind && (resource.StatefulSet == nil || resource.StatefulSet.State != applicationWorkloadDegraded || resource.StatefulSet.Reason != "PodFailure") {
			t.Fatalf("Pod failure was not folded into StatefulSet projection: %#v", resource)
		}
	}
	conditionStatus, conditionReason, _ := aggregateWorkloadHealth(resources)
	if conditionStatus != metav1.ConditionFalse || conditionReason != applicationWorkloadDegraded {
		t.Fatalf("folded StatefulSet Pod failure did not affect aggregate: %s/%s", conditionStatus, conditionReason)
	}
}

func TestCronJobOwnedJobsHaveProjectionButDoNotAffectApplicationAggregation(t *testing.T) {
	cron := inventoryObject("batch/v1", "CronJob", "demo", "cleanup", "cron-uid", nil)
	controller := true
	job := inventoryObject("batch/v1", "Job", "demo", "cleanup-123", "job-uid", []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "CronJob", Name: "cleanup", UID: "cron-uid", Controller: &controller}})
	job.Object["status"] = map[string]any{"failed": int64(1), applicationConditionsField: []any{map[string]any{applicationDefinitionTypeField: "Failed", applicationStatusField: applicationWorkloadTrue}}}
	jobPod := inventoryObject("v1", "Pod", "demo", "cleanup-123-pod", "job-pod", []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "cleanup-123", UID: "job-uid", Controller: &controller}})
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{{Group: "batch", Version: "v1", Resource: "jobs"}: "JobList", {Version: "v1", Resource: "pods"}: "PodList"}, cron, job, jobPod)
	resources, err := collectApplicationResourcesWithClients(context.Background(), client, inventoryTestMapper(), "apiVersion: batch/v1\nkind: CronJob\nmetadata:\n  name: cleanup\n", "demo")
	if err != nil {
		t.Fatalf("collect resources: %v", err)
	}
	var observedJob *platformv1alpha1.ApplicationResource
	var observedPod *platformv1alpha1.ApplicationResource
	for i := range resources {
		if resources[i].Kind == "Job" {
			observedJob = &resources[i]
		}
		if resources[i].Kind == applicationWorkloadPodKind {
			observedPod = &resources[i]
		}
	}
	if observedJob == nil || observedJob.Job == nil || observedJob.Job.State != applicationWorkloadDegraded {
		t.Fatalf("CronJob child Job projection missing: %#v", resources)
	}
	if observedPod == nil || observedPod.Pod == nil {
		t.Fatalf("Job-owned Pod projection missing: %#v", resources)
	}
	status, reason, _ := aggregateWorkloadHealth(resources)
	if status != metav1.ConditionUnknown || reason != "NoWorkloads" {
		t.Fatalf("CronJob history entered application aggregation: %s/%s", status, reason)
	}
}

func TestAggregateWorkloadHealth(t *testing.T) {
	dep := func(uid types.UID, desired, ready int32, state string) platformv1alpha1.ApplicationResource {
		return platformv1alpha1.ApplicationResource{APIVersion: "apps/v1", Kind: applicationWorkloadDeploymentKind, UID: uid, Deployment: &platformv1alpha1.ApplicationDeploymentHealth{DesiredReplicas: desired, ReadyReplicas: ready, State: state}}
	}
	pod := func(parent types.UID, state string) platformv1alpha1.ApplicationResource {
		return platformv1alpha1.ApplicationResource{APIVersion: "v1", Kind: applicationWorkloadPodKind, ParentUID: parent, Pod: &platformv1alpha1.ApplicationPodHealth{Ready: applicationWorkloadTrue, State: state}}
	}
	tests := []struct {
		name      string
		resources []platformv1alpha1.ApplicationResource
		status    metav1.ConditionStatus
		reason    string
	}{
		{name: "healthy deployment", resources: []platformv1alpha1.ApplicationResource{dep("d", 1, 1, applicationWorkloadHealthy), pod("rs", applicationWorkloadHealthy), {APIVersion: "apps/v1", Kind: applicationWorkloadReplicaSetKind, UID: "rs", ParentUID: "d"}}, status: metav1.ConditionTrue, reason: applicationWorkloadHealthy},
		{name: "initial rollout", resources: []platformv1alpha1.ApplicationResource{dep("d", 1, 0, applicationWorkloadProgressing)}, status: metav1.ConditionFalse, reason: applicationWorkloadProgressing},
		{name: "degraded crash", resources: []platformv1alpha1.ApplicationResource{dep("d", 1, 1, applicationWorkloadHealthy), pod("rs", applicationWorkloadDegraded), {APIVersion: "apps/v1", Kind: applicationWorkloadReplicaSetKind, UID: "rs", ParentUID: "d"}}, status: metav1.ConditionFalse, reason: applicationWorkloadDegraded},
		{name: "unschedulable", resources: []platformv1alpha1.ApplicationResource{{APIVersion: "v1", Kind: applicationWorkloadPodKind, Pod: &platformv1alpha1.ApplicationPodHealth{State: applicationWorkloadDegraded}}}, status: metav1.ConditionFalse, reason: applicationWorkloadDegraded},
		{name: "zero desired", resources: []platformv1alpha1.ApplicationResource{dep("d", 0, 0, applicationWorkloadHealthy)}, status: metav1.ConditionTrue, reason: applicationWorkloadHealthy},
		{name: "missing root", resources: []platformv1alpha1.ApplicationResource{dep("", 1, 1, applicationWorkloadProgressing)}, status: metav1.ConditionFalse, reason: applicationWorkloadProgressing},
		{name: "unrelated pod", resources: []platformv1alpha1.ApplicationResource{{APIVersion: "v1", Kind: applicationWorkloadPodKind, ParentUID: "elsewhere", Pod: &platformv1alpha1.ApplicationPodHealth{State: applicationWorkloadDegraded}}}, status: metav1.ConditionUnknown, reason: "NoWorkloads"},
		{name: "statefulset pod failure", resources: []platformv1alpha1.ApplicationResource{{APIVersion: "apps/v1", Kind: "StatefulSet", UID: "ss", StatefulSet: &platformv1alpha1.ApplicationStatefulSetHealth{State: applicationWorkloadHealthy}}, {APIVersion: "v1", Kind: applicationWorkloadPodKind, ParentUID: "ss", Pod: &platformv1alpha1.ApplicationPodHealth{State: applicationWorkloadDegraded}}}, status: metav1.ConditionFalse, reason: applicationWorkloadDegraded},
		{name: "daemonset pod failure", resources: []platformv1alpha1.ApplicationResource{{APIVersion: "apps/v1", Kind: "DaemonSet", UID: "ds", DaemonSet: &platformv1alpha1.ApplicationDaemonSetHealth{State: applicationWorkloadHealthy}}, {APIVersion: "v1", Kind: applicationWorkloadPodKind, ParentUID: "ds", Pod: &platformv1alpha1.ApplicationPodHealth{State: applicationWorkloadDegraded}}}, status: metav1.ConditionFalse, reason: applicationWorkloadDegraded},
		{name: "terminating statefulset pod ignored", resources: []platformv1alpha1.ApplicationResource{{APIVersion: "apps/v1", Kind: "StatefulSet", UID: "ss", StatefulSet: &platformv1alpha1.ApplicationStatefulSetHealth{State: applicationWorkloadHealthy}}, {APIVersion: "v1", Kind: applicationWorkloadPodKind, ParentUID: "ss", Pod: &platformv1alpha1.ApplicationPodHealth{State: applicationWorkloadProgressing, Reason: "PodTerminating"}}}, status: metav1.ConditionTrue, reason: applicationWorkloadHealthy},
		{name: "root job failed retry count is nonterminal", resources: []platformv1alpha1.ApplicationResource{{APIVersion: "batch/v1", Kind: "Job", UID: "j", Job: &platformv1alpha1.ApplicationJobHealth{Failed: int32Pointer(2), State: applicationWorkloadProgressing}}}, status: metav1.ConditionFalse, reason: applicationWorkloadProgressing},
		{name: "job-owned failed pod does not aggregate separately", resources: []platformv1alpha1.ApplicationResource{{APIVersion: "batch/v1", Kind: "Job", UID: "j", Job: &platformv1alpha1.ApplicationJobHealth{State: applicationWorkloadProgressing}}, {APIVersion: "v1", Kind: applicationWorkloadPodKind, ParentUID: "j", Pod: &platformv1alpha1.ApplicationPodHealth{State: applicationWorkloadDegraded}}}, status: metav1.ConditionFalse, reason: applicationWorkloadProgressing},
		{name: "cronjob history excluded", resources: []platformv1alpha1.ApplicationResource{{APIVersion: "batch/v1", Kind: "Job", UID: "j", ParentUID: "cron", Job: &platformv1alpha1.ApplicationJobHealth{State: applicationWorkloadDegraded}}}, status: metav1.ConditionUnknown, reason: "NoWorkloads"},
		{name: "no workloads", status: metav1.ConditionUnknown, reason: "NoWorkloads"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, reason, _ := aggregateWorkloadHealth(tt.resources)
			if status != tt.status || reason != tt.reason {
				t.Fatalf("got %s/%s, want %s/%s", status, reason, tt.status, tt.reason)
			}
		})
	}
}

func int32Pointer(value int32) *int32 { return &value }

func TestDeploymentHealthProjection(t *testing.T) {
	tests := []struct {
		name          string
		spec          map[string]any
		status        map[string]any
		generation    int64
		uid           types.UID
		state, reason string
	}{
		{name: "healthy deployment", spec: map[string]any{applicationDefinitionReplicasPath: int64(2)}, status: map[string]any{applicationObservedGenerationField: int64(3), applicationDefinitionReplicasPath: int64(2), applicationUpdatedReplicasField: int64(2), applicationReadyReplicasField: int64(2), "availableReplicas": int64(2)}, generation: 3, uid: "d", state: applicationWorkloadHealthy},
		{name: "zero scale", spec: map[string]any{applicationDefinitionReplicasPath: int64(0)}, status: map[string]any{applicationObservedGenerationField: int64(1), applicationDefinitionReplicasPath: int64(0), applicationUpdatedReplicasField: int64(0), applicationReadyReplicasField: int64(0), "availableReplicas": int64(0)}, generation: 1, uid: "d", state: applicationWorkloadHealthy},
		{name: "zero desired with omitted status counters", spec: map[string]any{applicationDefinitionReplicasPath: int64(0)}, status: map[string]any{applicationObservedGenerationField: int64(1)}, generation: 1, uid: "d", state: applicationWorkloadHealthy},
		{name: "default desired remains pending", status: map[string]any{applicationObservedGenerationField: int64(1)}, generation: 1, uid: "d", state: applicationWorkloadProgressing, reason: applicationRolloutInProgressReason},
		{name: "incomplete", spec: map[string]any{applicationDefinitionReplicasPath: int64(1)}, status: map[string]any{applicationObservedGenerationField: int64(2), applicationDefinitionReplicasPath: int64(1), applicationUpdatedReplicasField: int64(1), applicationReadyReplicasField: int64(1)}, generation: 2, uid: "d", state: applicationWorkloadProgressing, reason: applicationRolloutInProgressReason},
		{name: "deadline exceeded", spec: map[string]any{applicationDefinitionReplicasPath: int64(1)}, status: map[string]any{applicationObservedGenerationField: int64(2), applicationConditionsField: []any{map[string]any{applicationDefinitionTypeField: applicationWorkloadProgressing, applicationStatusField: applicationWorkloadFalse, applicationReasonField: "ProgressDeadlineExceeded"}}}, generation: 2, uid: "d", state: applicationWorkloadDegraded, reason: "ProgressDeadlineExceeded"},
		{name: "replica failure uses safe reason", spec: map[string]any{applicationDefinitionReplicasPath: int64(1)}, status: map[string]any{applicationObservedGenerationField: int64(2), applicationConditionsField: []any{map[string]any{applicationDefinitionTypeField: "ReplicaFailure", applicationStatusField: applicationWorkloadTrue, applicationReasonField: "sensitive detail"}}}, generation: 2, uid: "d", state: applicationWorkloadDegraded, reason: "ReplicaFailure"},
		{name: "absent status", spec: map[string]any{applicationDefinitionReplicasPath: int64(0)}, generation: 2, uid: "d", state: applicationWorkloadProgressing, reason: applicationRolloutInProgressReason},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{Object: map[string]any{applicationSpecField: tt.spec}}
			obj.SetGeneration(tt.generation)
			obj.SetUID(tt.uid)
			if tt.status != nil {
				obj.Object["status"] = tt.status
			}
			health := deploymentHealth(obj)
			if health.State != tt.state || health.Reason != tt.reason {
				t.Fatalf("got %s/%s, want %s/%s", health.State, health.Reason, tt.state, tt.reason)
			}
			if tt.name == "zero scale" && health.DesiredReplicas != 0 {
				t.Fatalf("desired replicas = %d, want 0", health.DesiredReplicas)
			}
		})
	}
}

func TestStatefulSetHealthProjection(t *testing.T) {
	tests := []struct {
		name          string
		exists        bool
		spec          map[string]any
		status        map[string]any
		generation    int64
		uid           types.UID
		state, reason string
	}{
		{name: "healthy", exists: true, spec: map[string]any{applicationDefinitionReplicasPath: int64(2), "updateStrategy": map[string]any{"type": "OnDelete"}}, status: map[string]any{applicationObservedGenerationField: int64(1), "replicas": int64(2), applicationReadyReplicasField: int64(2)}, generation: 1, uid: "ss", state: applicationWorkloadHealthy},
		{name: "initial", exists: true, spec: map[string]any{}, generation: 1, uid: "ss", state: applicationWorkloadProgressing, reason: applicationRolloutInProgressReason},
		{name: "current but positive counts omitted", exists: true, spec: map[string]any{applicationDefinitionReplicasPath: int64(1)}, status: map[string]any{applicationObservedGenerationField: int64(1)}, generation: 1, uid: "ss", state: applicationWorkloadProgressing, reason: applicationRolloutInProgressReason},
		{name: "mismatch", exists: true, spec: map[string]any{applicationDefinitionReplicasPath: int64(2)}, status: map[string]any{applicationObservedGenerationField: int64(1), "replicas": int64(2), applicationReadyReplicasField: int64(1)}, generation: 1, uid: "ss", state: applicationWorkloadProgressing, reason: applicationRolloutInProgressReason},
		{name: "missing root", state: applicationWorkloadProgressing, reason: "ResourceMissing"},
		{name: "zero desired with omitted counters", exists: true, spec: map[string]any{applicationDefinitionReplicasPath: int64(0)}, status: map[string]any{applicationObservedGenerationField: int64(2)}, generation: 2, uid: "ss", state: applicationWorkloadHealthy},
		{name: "stale", exists: true, spec: map[string]any{applicationDefinitionReplicasPath: int64(1)}, status: map[string]any{applicationObservedGenerationField: int64(1), "replicas": int64(1), applicationReadyReplicasField: int64(1)}, generation: 2, uid: "ss", state: applicationWorkloadProgressing, reason: applicationRolloutInProgressReason},
		{name: "malformed", exists: true, status: map[string]any{applicationObservedGenerationField: int64(1), "replicas": "bad"}, generation: 1, uid: "ss", state: applicationWorkloadUnknown, reason: "MalformedStatus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := workloadObject(tt.spec, tt.status, tt.generation, tt.uid)
			health := statefulSetHealth(obj, tt.exists, tt.uid != "")
			assertHealth(t, health.State, health.Reason, tt.state, tt.reason)
			if tt.name == "zero desired with omitted counters" && (health.DesiredReplicas == nil || *health.DesiredReplicas != 0 || health.ReadyReplicas == nil || *health.ReadyReplicas != 0) {
				t.Fatalf("live zero StatefulSet counts were not retained: %#v", health)
			}
		})
	}
}

func TestDaemonSetHealthProjection(t *testing.T) {
	tests := []struct {
		name          string
		exists        bool
		status        map[string]any
		generation    int64
		uid           types.UID
		state, reason string
	}{
		{name: "healthy", exists: true, status: map[string]any{applicationObservedGenerationField: int64(1), "desiredNumberScheduled": int64(2), "currentNumberScheduled": int64(2), "numberReady": int64(2), "numberMisscheduled": int64(0)}, generation: 1, uid: "ds", state: applicationWorkloadHealthy},
		{name: "zero eligible with omitted counters", exists: true, status: map[string]any{applicationObservedGenerationField: int64(1)}, generation: 1, uid: "ds", state: applicationWorkloadHealthy, reason: "NoEligibleNodes"},
		{name: "misscheduled nodes prevent healthy", exists: true, status: map[string]any{applicationObservedGenerationField: int64(1), "desiredNumberScheduled": int64(2), "currentNumberScheduled": int64(2), "numberReady": int64(2), "numberMisscheduled": int64(1)}, generation: 1, uid: "ds", state: applicationWorkloadProgressing, reason: applicationRolloutInProgressReason},
		{name: "malformed misscheduled count", exists: true, status: map[string]any{applicationObservedGenerationField: int64(1), "desiredNumberScheduled": int64(1), "currentNumberScheduled": int64(1), "numberReady": int64(1), "numberMisscheduled": "bad"}, generation: 1, uid: "ds", state: applicationWorkloadUnknown, reason: "MalformedStatus"},
		{name: "initial", exists: true, generation: 1, uid: "ds", state: applicationWorkloadProgressing, reason: applicationRolloutInProgressReason},
		{name: "mismatch", exists: true, status: map[string]any{applicationObservedGenerationField: int64(1), "desiredNumberScheduled": int64(2), "currentNumberScheduled": int64(2), "numberReady": int64(1)}, generation: 1, uid: "ds", state: applicationWorkloadProgressing, reason: applicationRolloutInProgressReason},
		{name: "missing root", state: applicationWorkloadProgressing, reason: "ResourceMissing"},
		{name: "stale", exists: true, status: map[string]any{applicationObservedGenerationField: int64(1), "desiredNumberScheduled": int64(1), "currentNumberScheduled": int64(1), "numberReady": int64(1)}, generation: 2, uid: "ds", state: applicationWorkloadProgressing, reason: applicationRolloutInProgressReason},
		{name: "malformed", exists: true, status: map[string]any{applicationObservedGenerationField: int64(1), "desiredNumberScheduled": "bad"}, generation: 1, uid: "ds", state: applicationWorkloadUnknown, reason: "MalformedStatus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			health := daemonSetHealth(workloadObject(nil, tt.status, tt.generation, tt.uid), tt.exists, tt.uid != "")
			assertHealth(t, health.State, health.Reason, tt.state, tt.reason)
			if tt.name == "zero eligible with omitted counters" && (health.DesiredNumberScheduled == nil || *health.DesiredNumberScheduled != 0 || health.NumberReady == nil || *health.NumberReady != 0) {
				t.Fatalf("zero desired count was not retained: %#v", health)
			}
		})
	}
}

func TestJobHealthProjection(t *testing.T) {
	tests := []struct {
		name          string
		exists        bool
		spec, status  map[string]any
		uid           types.UID
		state, reason string
	}{
		{name: "complete with failed attempts", exists: true, spec: map[string]any{"completions": int64(3)}, status: map[string]any{"failed": int64(2), applicationConditionsField: []any{map[string]any{applicationDefinitionTypeField: "Complete", applicationStatusField: applicationWorkloadTrue}}}, uid: "job", state: applicationWorkloadHealthy, reason: "Complete"},
		{name: "malformed counts fail closed despite complete", exists: true, status: map[string]any{"active": "bad", applicationConditionsField: []any{map[string]any{applicationDefinitionTypeField: "Complete", applicationStatusField: applicationWorkloadTrue}}}, uid: "job", state: applicationWorkloadUnknown, reason: "MalformedStatus"},
		{name: "failed retries are not terminal", exists: true, status: map[string]any{"failed": int64(3), "active": int64(1)}, uid: "job", state: applicationWorkloadProgressing, reason: "Running"},
		{name: "failed condition", exists: true, status: map[string]any{applicationConditionsField: []any{map[string]any{applicationDefinitionTypeField: "Failed", applicationStatusField: applicationWorkloadTrue}}}, uid: "job", state: applicationWorkloadDegraded, reason: "Failed"},
		{name: "suspended", exists: true, spec: map[string]any{"suspend": true}, uid: "job", state: applicationWorkloadProgressing, reason: "Suspended"},
		{name: "paused", exists: true, spec: map[string]any{"parallelism": int64(0)}, uid: "job", state: applicationWorkloadProgressing, reason: "Paused"},
		{name: "finishing", exists: true, status: map[string]any{applicationConditionsField: []any{map[string]any{applicationDefinitionTypeField: "SuccessCriteriaMet", applicationStatusField: applicationWorkloadTrue}}}, uid: "job", state: applicationWorkloadProgressing, reason: "Finishing"},
		{name: "missing root", state: applicationWorkloadProgressing, reason: "ResourceMissing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			health := jobHealth(workloadObject(tt.spec, tt.status, 1, tt.uid), tt.exists, tt.uid != "")
			assertHealth(t, health.State, health.Reason, tt.state, tt.reason)
			if tt.name == "complete with failed attempts" && (health.Completions == nil || *health.Completions != 3 || health.Failed == nil || *health.Failed != 2) {
				t.Fatalf("Job counters were not projected: %#v", health)
			}
		})
	}
	zeroCounts := jobHealth(workloadObject(nil, map[string]any{"active": int64(0), "succeeded": int64(0), "failed": int64(0)}, 1, "job"), true, true)
	if zeroCounts.Active == nil || *zeroCounts.Active != 0 || zeroCounts.Succeeded == nil || *zeroCounts.Succeeded != 0 || zeroCounts.Failed == nil || *zeroCounts.Failed != 0 {
		t.Fatalf("live zero Job counts were not retained: %#v", zeroCounts)
	}
	encoded, err := json.Marshal(&platformv1alpha1.ApplicationJobHealth{State: applicationWorkloadProgressing})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["completions"]; exists {
		t.Fatalf("nil completions should be omitted: %s", encoded)
	}
}

func TestFoldOwnedPodHealthIntoParentProjection(t *testing.T) {
	tests := []struct {
		name                  string
		pod                   *platformv1alpha1.ApplicationPodHealth
		podStates             []string
		wantState, wantReason string
	}{
		{name: "progressing pod", pod: &platformv1alpha1.ApplicationPodHealth{State: applicationWorkloadProgressing}, wantState: applicationWorkloadProgressing, wantReason: "PodProgressing"},
		{name: "degraded pod", pod: &platformv1alpha1.ApplicationPodHealth{State: applicationWorkloadDegraded}, wantState: applicationWorkloadDegraded, wantReason: "PodFailure"},
		{name: "terminating pod ignored", pod: &platformv1alpha1.ApplicationPodHealth{State: applicationWorkloadProgressing, Reason: "PodTerminating"}, wantState: applicationWorkloadHealthy},
		{name: "unknown after progressing", podStates: []string{applicationWorkloadProgressing, applicationWorkloadUnknown}, wantState: applicationWorkloadUnknown, wantReason: applicationMalformedStatusReason},
		{name: "unknown does not override degraded", podStates: []string{applicationWorkloadDegraded, applicationWorkloadUnknown}, wantState: applicationWorkloadDegraded, wantReason: "PodFailure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.podStates) > 0 {
				parent := platformv1alpha1.ApplicationResource{Deployment: &platformv1alpha1.ApplicationDeploymentHealth{State: applicationWorkloadHealthy}}
				for _, podState := range tt.podStates {
					foldPodStateIntoParent(&parent, podState)
				}
				assertHealth(t, parent.Deployment.State, parent.Deployment.Reason, tt.wantState, tt.wantReason)
				return
			}
			resources := map[string]platformv1alpha1.ApplicationResource{
				"deployment": {APIVersion: "apps/v1", Kind: applicationWorkloadDeploymentKind, UID: "d", Deployment: &platformv1alpha1.ApplicationDeploymentHealth{State: applicationWorkloadHealthy}},
				"replicaset": {APIVersion: "apps/v1", Kind: applicationWorkloadReplicaSetKind, UID: "rs", ParentUID: "d"},
				"pod":        {APIVersion: "v1", Kind: applicationWorkloadPodKind, ParentUID: "rs", Pod: tt.pod},
			}
			foldOwnedPodHealth(resources)
			parent := resources["deployment"].Deployment
			assertHealth(t, parent.State, parent.Reason, tt.wantState, tt.wantReason)
			status, reason, _ := aggregateWorkloadHealth(mapResourcesToSlice(resources))
			if tt.wantState == applicationWorkloadDegraded && (status != metav1.ConditionFalse || reason != applicationWorkloadDegraded) {
				t.Fatalf("degraded Pod did not affect aggregate condition: %s/%s", status, reason)
			}
		})
	}
}

func mapResourcesToSlice(resources map[string]platformv1alpha1.ApplicationResource) []platformv1alpha1.ApplicationResource {
	result := make([]platformv1alpha1.ApplicationResource, 0, len(resources))
	for _, resource := range resources {
		result = append(result, resource)
	}
	return result
}

func TestRootHealthProjectionRequiresSupportedAPIVersion(t *testing.T) {
	obj := workloadObject(map[string]any{applicationDefinitionReplicasPath: int64(0)}, map[string]any{applicationObservedGenerationField: int64(1), "replicas": int64(0), applicationReadyReplicasField: int64(0)}, 1, "uid")
	root := inventoryIdentity{apiVersion: "apps/v1", kind: applicationWorkloadStatefulSetKind, uid: "uid"}
	projectRootHealth(&root, obj, true)
	if root.statefulSet == nil || root.statefulSet.State != applicationWorkloadHealthy {
		t.Fatalf("supported StatefulSet projection missing: %#v", root)
	}
	wrongGroup := inventoryIdentity{apiVersion: "example.test/v1", kind: applicationWorkloadStatefulSetKind, uid: "uid"}
	projectRootHealth(&wrongGroup, obj, true)
	if wrongGroup.statefulSet != nil {
		t.Fatalf("unsupported group received StatefulSet health: %#v", wrongGroup)
	}
	wrongVersion := inventoryIdentity{apiVersion: "batch/v1", kind: applicationWorkloadJobKind, uid: "uid"}
	projectRootHealth(&wrongVersion, obj, true)
	if wrongVersion.job == nil || wrongVersion.statefulSet != nil {
		t.Fatalf("batch/v1 Job was not independently projected: %#v", wrongVersion)
	}
	wrongJobGroup := inventoryIdentity{apiVersion: "example.test/v1", kind: applicationWorkloadJobKind, uid: "uid"}
	projectRootHealth(&wrongJobGroup, obj, true)
	if wrongJobGroup.job != nil {
		t.Fatalf("unsupported group received Job health: %#v", wrongJobGroup)
	}
}

func workloadObject(spec, status map[string]any, generation int64, uid types.UID) *unstructured.Unstructured {
	object := map[string]any{}
	if spec != nil {
		object[applicationSpecField] = spec
	}
	if status != nil {
		object["status"] = status
	}
	result := &unstructured.Unstructured{Object: object}
	result.SetGeneration(generation)
	result.SetUID(uid)
	return result
}

func assertHealth(t *testing.T, state, reason, wantState, wantReason string) {
	t.Helper()
	if state != wantState || reason != wantReason {
		t.Fatalf("got %s/%s, want %s/%s", state, reason, wantState, wantReason)
	}
}

func TestPodHealthProjection(t *testing.T) {
	ready := []any{map[string]any{applicationDefinitionTypeField: applicationReadyConditionType, applicationStatusField: applicationWorkloadTrue}}
	unschedulable := []any{map[string]any{"type": "PodScheduled", "status": applicationWorkloadFalse, applicationReasonField: "Unschedulable"}}
	tests := []struct {
		name          string
		status        map[string]any
		state, reason string
	}{
		{name: "healthy pod", status: map[string]any{applicationPhaseField: "Running", applicationConditionsField: ready}, state: applicationWorkloadHealthy},
		{name: "failed", status: map[string]any{applicationPhaseField: "Failed"}, state: applicationWorkloadDegraded, reason: "PodFailed"},
		{name: "pending", status: map[string]any{applicationPhaseField: "Pending"}, state: applicationWorkloadProgressing, reason: "PodStarting"},
		{name: "unschedulable", status: map[string]any{applicationConditionsField: unschedulable}, state: applicationWorkloadDegraded, reason: "Unschedulable"},
		{name: "init crash", status: map[string]any{"initContainerStatuses": []any{map[string]any{"state": map[string]any{"waiting": map[string]any{applicationReasonField: "CrashLoopBackOff"}}}}}, state: applicationWorkloadDegraded, reason: "CrashLoopBackOff"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{Object: map[string]any{"status": tt.status}}
			health := podHealth(obj)
			if health.State != tt.state || health.Reason != tt.reason {
				t.Fatalf("got %s/%s, want %s/%s", health.State, health.Reason, tt.state, tt.reason)
			}
			if strings.Contains(health.Message, "sensitive") || strings.Contains(health.Message, "raw") {
				t.Fatalf("unsafe message: %q", health.Message)
			}
		})
	}
}

func TestCollectApplicationResourcesMapsClusterScopedRoot(t *testing.T) {
	ns := ""
	clusterRoot := inventoryObject("v1", "Namespace", "", "sample", "namespace-uid", nil)
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), clusterRoot)
	resources, err := collectApplicationResourcesWithClients(context.Background(), client, inventoryTestMapper(), "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: sample\n", "demo")
	if err != nil {
		t.Fatalf("collect resources: %v", err)
	}
	if len(resources) != 1 || resources[0].Namespace != ns || resources[0].UID != types.UID("namespace-uid") {
		t.Fatalf("unexpected cluster-scoped resource: %#v", resources)
	}
}

func inventoryTestMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Version: "v1"}, {Group: appsAPIGroup, Version: "v1"}})
	mapper.AddSpecific(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, schema.GroupVersionResource{Version: "v1", Resource: "configmap"}, meta.RESTScopeNamespace)
	mapper.AddSpecific(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, schema.GroupVersionResource{Version: "v1", Resource: "namespace"}, meta.RESTScopeRoot)
	mapper.AddSpecific(schema.GroupVersionKind{Group: appsAPIGroup, Version: "v1", Kind: "Deployment"}, schema.GroupVersionResource{Group: appsAPIGroup, Version: "v1", Resource: "deployments"}, schema.GroupVersionResource{Group: appsAPIGroup, Version: "v1", Resource: "deployment"}, meta.RESTScopeNamespace)
	mapper.AddSpecific(schema.GroupVersionKind{Group: appsAPIGroup, Version: "v1", Kind: "StatefulSet"}, schema.GroupVersionResource{Group: appsAPIGroup, Version: "v1", Resource: "statefulsets"}, schema.GroupVersionResource{Group: appsAPIGroup, Version: "v1", Resource: "statefulset"}, meta.RESTScopeNamespace)
	mapper.AddSpecific(schema.GroupVersionKind{Group: appsAPIGroup, Version: "v1", Kind: "DaemonSet"}, schema.GroupVersionResource{Group: appsAPIGroup, Version: "v1", Resource: "daemonsets"}, schema.GroupVersionResource{Group: appsAPIGroup, Version: "v1", Resource: "daemonset"}, meta.RESTScopeNamespace)
	mapper.AddSpecific(schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}, schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "job"}, meta.RESTScopeNamespace)
	mapper.AddSpecific(schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "CronJob"}, schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}, schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjob"}, meta.RESTScopeNamespace)
	return mapper
}

func inventoryObject(apiVersion, kind, namespace, name string, uid types.UID, owners []metav1.OwnerReference) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{}}
	obj.SetAPIVersion(apiVersion)
	obj.SetKind(kind)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	obj.SetUID(uid)
	obj.SetOwnerReferences(owners)
	return obj
}

func owner(kind, name string, uid types.UID) []metav1.OwnerReference {
	controller := true
	return []metav1.OwnerReference{{APIVersion: appsAPIVersion, Kind: kind, Name: name, UID: uid, Controller: &controller}}
}
