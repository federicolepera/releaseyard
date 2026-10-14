package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
)

const (
	testClusterName      = "remote"
	testSecretName       = "remote-kubeconfig"
	testManagerNamespace = "idp-system"
)

func TestReconcileMissingKubeconfigSecretSetsDisconnectedCondition(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &platformv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Generation: 4},
		Spec:       platformv1alpha1.ClusterSpec{KubeconfigSecretRef: platformv1alpha1.KubeconfigSecretReference{Name: testSecretName}},
		Status: platformv1alpha1.ClusterStatus{Conditions: []metav1.Condition{{
			Type: "Ready", Status: metav1.ConditionTrue, Reason: "ExistingCondition", Message: "preserve me", LastTransitionTime: metav1.Now(),
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&platformv1alpha1.Cluster{}).WithObjects(cluster).Build()
	r := &ClusterReconciler{Client: c, APIReader: c, CredentialsNamespace: testManagerNamespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: testClusterName}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got platformv1alpha1.Cluster
	if err := c.Get(context.Background(), types.NamespacedName{Name: testClusterName}, &got); err != nil {
		t.Fatal(err)
	}
	assertClusterCondition(t, &got, metav1.ConditionFalse, "CredentialsUnavailable")
	if ready := meta.FindStatusCondition(got.Status.Conditions, "Ready"); ready == nil || ready.Message != "preserve me" {
		t.Fatalf("unrelated Ready condition was not preserved: %#v", ready)
	}
	if got.Status.LastProbeTime != nil {
		t.Fatalf("LastProbeTime must remain unset before a remote probe, got %v", got.Status.LastProbeTime)
	}
}

func TestClusterNodeInventory(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &platformv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Generation: 4},
		Spec:       platformv1alpha1.ClusterSpec{KubeconfigSecretRef: platformv1alpha1.KubeconfigSecretReference{Name: testSecretName}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&platformv1alpha1.Cluster{}).WithObjects(cluster).Build()
	r := &ClusterReconciler{Client: c, APIReader: c, CredentialsNamespace: testManagerNamespace}
	var got platformv1alpha1.Cluster

	listFailure := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"status":{"userInfo":{"username":"alice"}}}`))
			return
		}
		if listFailure {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","status":"Failure"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(&corev1.NodeList{Items: []corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "z-unknown"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionUnknown}}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "b-not-ready"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "a-ready"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "c-missing-ready"}},
		}})
	}))
	defer server.Close()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testSecretName, Namespace: testManagerNamespace}, Data: map[string][]byte{"kubeconfig": testKubeconfig(server)}}
	if err := c.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: testClusterName}}); err != nil {
		t.Fatalf("reconcile connected: %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: testClusterName}, &got); err != nil {
		t.Fatal(err)
	}
	assertClusterCondition(t, &got, metav1.ConditionTrue, "Connected")
	if len(got.Status.Nodes) != 4 {
		t.Fatalf("expected four summarized nodes, got %#v", got.Status.Nodes)
	}
	wantNodes := []platformv1alpha1.ClusterNode{{Name: "a-ready", Ready: "True"}, {Name: "b-not-ready", Ready: "False"}, {Name: "c-missing-ready", Ready: "Unknown"}, {Name: "z-unknown", Ready: "Unknown"}}
	for index, want := range wantNodes {
		if got.Status.Nodes[index] != want {
			t.Fatalf("node summary[%d] = %#v, want %#v", index, got.Status.Nodes[index], want)
		}
	}
	if got.Status.NodesObservedAt == nil {
		t.Fatal("successful node inventory did not set NodesObservedAt")
	}
	if nodesReady := meta.FindStatusCondition(got.Status.Conditions, "NodesReady"); nodesReady == nil || nodesReady.Status != metav1.ConditionTrue {
		t.Fatalf("NodesReady condition was not true after inventory refresh: %#v", nodesReady)
	}
	if got.Status.LastProbeTime == nil {
		t.Fatal("successful remote probe did not set LastProbeTime")
	}
	checkClusterInventoryFailure(t, r, c, &got, secret, &listFailure, len(wantNodes))
}

func checkClusterInventoryFailure(t *testing.T, r *ClusterReconciler, c client.Client, got *platformv1alpha1.Cluster, secret *corev1.Secret, listFailure *bool, nodeCount int) {
	t.Helper()
	lastCompletedProbe := got.Status.LastProbeTime.DeepCopy()
	lastInventoryTime := got.Status.NodesObservedAt.DeepCopy()
	*listFailure = true
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: testClusterName}}); err != nil {
		t.Fatalf("reconcile inventory failure: %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: testClusterName}, got); err != nil {
		t.Fatal(err)
	}
	assertClusterCondition(t, got, metav1.ConditionTrue, "Connected")
	if nodesReady := meta.FindStatusCondition(got.Status.Conditions, "NodesReady"); nodesReady == nil || nodesReady.Status != metav1.ConditionFalse {
		t.Fatalf("NodesReady condition did not report inventory failure: %#v", nodesReady)
	}
	if got.Status.NodesObservedAt == nil || !got.Status.NodesObservedAt.Equal(lastInventoryTime) || len(got.Status.Nodes) != nodeCount {
		t.Fatalf("transient inventory failure did not preserve snapshot: nodes=%#v observedAt=%v", got.Status.Nodes, got.Status.NodesObservedAt)
	}
	got.Generation = 5
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: testClusterName}}); err != nil {
		t.Fatalf("reconcile changed generation: %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: testClusterName}, got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Nodes) != 0 || got.Status.NodesObservedAt != nil {
		t.Fatalf("inventory for prior generation was retained after refresh failure: nodes=%#v observedAt=%v", got.Status.Nodes, got.Status.NodesObservedAt)
	}
	*listFailure = false

	secret.Data["kubeconfig"] = []byte("not a kubeconfig")
	if err := c.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: testClusterName}}); err != nil {
		t.Fatalf("reconcile disconnected: %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: testClusterName}, got); err != nil {
		t.Fatal(err)
	}
	assertClusterCondition(t, got, metav1.ConditionFalse, "InvalidCredentials")
	if !got.Status.LastProbeTime.Equal(lastCompletedProbe) {
		t.Fatalf("invalid local config changed LastProbeTime: got %v, want %v", got.Status.LastProbeTime, lastCompletedProbe)
	}
}

func assertClusterCondition(t *testing.T, cluster *platformv1alpha1.Cluster, status metav1.ConditionStatus, reason string) {
	t.Helper()
	condition := meta.FindStatusCondition(cluster.Status.Conditions, "Connected")
	if condition == nil {
		t.Fatalf("Connected condition was not set: %#v", cluster.Status.Conditions)
	}
	if condition.Type != "Connected" || condition.Status != status || condition.Reason != reason || condition.ObservedGeneration != cluster.Generation {
		t.Fatalf("unexpected condition: %#v", condition)
	}
}
