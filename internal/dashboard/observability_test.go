package dashboard

import (
	"testing"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestApplicationScopeOfUsesBindingAndResources(t *testing.T) {
	app := &platformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: testNamespace},
		Spec: platformv1alpha1.ApplicationSpec{
			ClusterRef: &platformv1alpha1.ApplicationReference{Name: "managed-a"},
		},
		Status: platformv1alpha1.ApplicationStatus{
			Binding: &platformv1alpha1.ApplicationBinding{ReleaseNamespace: "applications", ReleaseName: "team-demo"},
			Resources: []platformv1alpha1.ApplicationResource{
				{APIVersion: "v1", Kind: "Pod", Name: "team-demo-abc"},
				{APIVersion: "apps/v1", Kind: "Deployment", Name: "team-demo"},
			},
		},
	}
	scope, err := applicationScopeOf(app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if scope.clusterName != "managed-a" || scope.namespace != "applications" || scope.release != "team-demo" {
		t.Fatalf("unexpected scope: %#v", scope)
	}
	if !scope.belongsTo("team-demo-abc") {
		t.Fatal("pod should belong to the application")
	}
	if scope.belongsTo("other-pod") {
		t.Fatal("unrelated pod must not belong to the application")
	}
	if pods := scope.podNames(); len(pods) != 1 || pods[0] != "team-demo-abc" {
		t.Fatalf("unexpected pod names: %#v", pods)
	}
}

func TestApplicationScopeOfRequiresObservedTarget(t *testing.T) {
	app := &platformv1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: testNamespace}}
	if _, err := applicationScopeOf(app); err == nil {
		t.Fatal("expected an error when there is no observed target")
	}
}

func TestSumMetricsContainers(t *testing.T) {
	item := &unstructured.Unstructured{Object: map[string]any{
		"containers": []any{
			map[string]any{"usage": map[string]any{"cpu": "250m", "memory": "64Mi"}},
			map[string]any{"usage": map[string]any{"cpu": "750m", "memory": "64Mi"}},
		},
	}}
	cpu, memory := sumMetricsContainers(item)
	if cpu.MilliValue() != 1000 {
		t.Fatalf("cpu milli=%d, want 1000", cpu.MilliValue())
	}
	if memory.Value() != 128<<20 {
		t.Fatalf("memory bytes=%d, want %d", memory.Value(), 128<<20)
	}
}

func TestSummarizeResource(t *testing.T) {
	pod := &unstructured.Unstructured{Object: map[string]any{
		"kind": "Pod",
		"status": map[string]any{
			"phase": "Running",
			"containerStatuses": []any{
				map[string]any{"ready": true},
				map[string]any{"ready": false},
			},
		},
	}}
	status, tone, ready := summarizeResource(pod)
	if status != "Running" || tone != "good" || ready != "1/2" {
		t.Fatalf("pod summary = %q/%q/%q, want Running/good/1/2", status, tone, ready)
	}

	deployment := &unstructured.Unstructured{Object: map[string]any{
		"kind":   "Deployment",
		"spec":   map[string]any{"replicas": int64(2)},
		"status": map[string]any{"readyReplicas": int64(1)},
	}}
	status, tone, ready = summarizeResource(deployment)
	if status != "Progressing" || tone != "muted" || ready != "1/2" {
		t.Fatalf("deployment summary = %q/%q/%q, want Progressing/muted/1/2", status, tone, ready)
	}

	pvc := &unstructured.Unstructured{Object: map[string]any{
		"kind":   "PersistentVolumeClaim",
		"status": map[string]any{"phase": "Bound"},
	}}
	status, tone, ready = summarizeResource(pvc)
	if status != "Bound" || tone != "good" || ready != "—" {
		t.Fatalf("pvc summary = %q/%q/%q, want Bound/good/—", status, tone, ready)
	}
}
