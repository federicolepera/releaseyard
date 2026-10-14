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

func newDefinitionAccessHandler(t *testing.T) (*authHandler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	return &authHandler{k8s: c, namespace: testNamespace}, c
}

func TestDefinitionAccessCreatesRoleAndBinding(t *testing.T) {
	a, c := newDefinitionAccessHandler(t)
	body := `{"group":"platform","definitions":["redis","nginx"]}`
	request := httptest.NewRequest(http.MethodPut, "http://dashboard/api/administration/definition-access", strings.NewReader(body))
	request.Header.Set("Origin", "http://dashboard")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	a.putDefinitionAccess(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", response.Code, response.Body)
	}

	name := definitionAccessName("platform")
	var role rbacv1.ClusterRole
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, &role); err != nil {
		t.Fatal(err)
	}
	if len(role.Rules) != 1 || !slices.Equal(role.Rules[0].ResourceNames, []string{"nginx", "redis"}) {
		t.Fatalf("unexpected role rules: %#v", role.Rules)
	}
	if !slices.Contains(role.Rules[0].Verbs, "get") || !slices.Equal(role.Rules[0].Resources, []string{"applicationdefinitions"}) {
		t.Fatalf("unexpected role rule: %#v", role.Rules[0])
	}
	var binding rbacv1.ClusterRoleBinding
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, &binding); err != nil {
		t.Fatal(err)
	}
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != name {
		t.Fatalf("unexpected roleRef: %#v", binding.RoleRef)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "Group" || binding.Subjects[0].Name != "platform" {
		t.Fatalf("unexpected subjects: %#v", binding.Subjects)
	}

	listRequest := httptest.NewRequest(http.MethodGet, "http://dashboard/api/administration/definition-access", nil)
	listResponse := httptest.NewRecorder()
	a.getDefinitionAccess(listResponse, listRequest)
	var listed definitionAccessResponse
	if err := json.Unmarshal(listResponse.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Rules) != 1 || listed.Rules[0].Group != "platform" || !slices.Equal(listed.Rules[0].Definitions, []string{"nginx", "redis"}) {
		t.Fatalf("unexpected listed rules: %#v", listed.Rules)
	}

	// Saving an empty definition set removes the rule.
	empty := `{"group":"platform","definitions":[]}`
	emptyRequest := httptest.NewRequest(http.MethodPut, "http://dashboard/api/administration/definition-access", strings.NewReader(empty))
	emptyRequest.Header.Set("Origin", "http://dashboard")
	emptyRequest.Header.Set("Content-Type", "application/json")
	emptyResponse := httptest.NewRecorder()
	a.putDefinitionAccess(emptyResponse, emptyRequest)
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

func TestDefinitionAccessRejectsInvalidInput(t *testing.T) {
	a, _ := newDefinitionAccessHandler(t)
	request := httptest.NewRequest(http.MethodPut, "http://dashboard/api/administration/definition-access", strings.NewReader(`{"group":"","definitions":["nginx"]}`))
	request.Header.Set("Origin", "http://dashboard")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	a.putDefinitionAccess(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("empty group status=%d, want 400", response.Code)
	}

	bad := httptest.NewRequest(http.MethodPut, "http://dashboard/api/administration/definition-access", strings.NewReader(`{"group":"platform","definitions":["Not A Name"]}`))
	bad.Header.Set("Origin", "http://dashboard")
	bad.Header.Set("Content-Type", "application/json")
	badResponse := httptest.NewRecorder()
	a.putDefinitionAccess(badResponse, bad)
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid definition status=%d, want 400", badResponse.Code)
	}
}
