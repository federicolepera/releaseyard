package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newClusterAccessHandler(t *testing.T) (*authHandler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	return &authHandler{k8s: c, namespace: testNamespace}, c
}

func TestClusterAccessCreatesRoleAndBinding(t *testing.T) {
	a, c := newClusterAccessHandler(t)
	body := `{"group":"platform","clusters":["managed-b","managed-a"]}`
	request := httptest.NewRequest(http.MethodPut, "http://dashboard/api/administration/cluster-access", strings.NewReader(body))
	request.Header.Set("Origin", "http://dashboard")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	a.putClusterAccess(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", response.Code, response.Body)
	}

	name := clusterAccessName("platform")
	var role rbacv1.ClusterRole
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, &role); err != nil {
		t.Fatal(err)
	}
	if len(role.Rules) != 1 || !slices.Equal(role.Rules[0].ResourceNames, []string{"managed-a", "managed-b"}) {
		t.Fatalf("unexpected role rules: %#v", role.Rules)
	}
	if !slices.Contains(role.Rules[0].Verbs, "get") || !slices.Equal(role.Rules[0].Resources, []string{"clusters"}) {
		t.Fatalf("unexpected role rule: %#v", role.Rules[0])
	}
	var binding rbacv1.ClusterRoleBinding
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, &binding); err != nil {
		t.Fatal(err)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "Group" || binding.Subjects[0].Name != "platform" {
		t.Fatalf("unexpected subjects: %#v", binding.Subjects)
	}

	listRequest := httptest.NewRequest(http.MethodGet, "http://dashboard/api/administration/cluster-access", nil)
	listResponse := httptest.NewRecorder()
	a.getClusterAccess(listResponse, listRequest)
	var listed clusterAccessResponse
	if err := json.Unmarshal(listResponse.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Rules) != 1 || listed.Rules[0].Group != "platform" || !slices.Equal(listed.Rules[0].Clusters, []string{"managed-a", "managed-b"}) {
		t.Fatalf("unexpected listed rules: %#v", listed.Rules)
	}

	// Saving an empty cluster set removes the rule.
	empty := `{"group":"platform","clusters":[]}`
	emptyRequest := httptest.NewRequest(http.MethodPut, "http://dashboard/api/administration/cluster-access", strings.NewReader(empty))
	emptyRequest.Header.Set("Origin", "http://dashboard")
	emptyRequest.Header.Set("Content-Type", "application/json")
	emptyResponse := httptest.NewRecorder()
	a.putClusterAccess(emptyResponse, emptyRequest)
	if emptyResponse.Code != http.StatusOK {
		t.Fatalf("empty PUT status=%d body=%s", emptyResponse.Code, emptyResponse.Body)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, &rbacv1.ClusterRole{}); !apierrors.IsNotFound(err) {
		t.Fatalf("role still present after empty save: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, &rbacv1.ClusterRoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("binding still present after empty save: %v", err)
	}
}

func TestClusterAccessRejectsInvalidInput(t *testing.T) {
	a, _ := newClusterAccessHandler(t)
	request := httptest.NewRequest(http.MethodPut, "http://dashboard/api/administration/cluster-access", strings.NewReader(`{"group":"","clusters":["managed-a"]}`))
	request.Header.Set("Origin", "http://dashboard")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	a.putClusterAccess(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("empty group status=%d, want 400", response.Code)
	}

	bad := httptest.NewRequest(http.MethodPut, "http://dashboard/api/administration/cluster-access", strings.NewReader(`{"group":"platform","clusters":["Not A Name"]}`))
	bad.Header.Set("Origin", "http://dashboard")
	bad.Header.Set("Content-Type", "application/json")
	badResponse := httptest.NewRecorder()
	a.putClusterAccess(badResponse, bad)
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid cluster status=%d, want 400", badResponse.Code)
	}
}
