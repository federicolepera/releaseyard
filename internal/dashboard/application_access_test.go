package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newApplicationAccessHandler(t *testing.T) (*authHandler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	return &authHandler{
		k8s:                   c,
		namespace:             testNamespace,
		teamNamespacePrefix:   "releaseyard-team-",
		applicationEditorRole: "releaseyard-application-editor-role",
	}, c
}

func TestApplicationAccessCreatesNamespaceAndBinding(t *testing.T) {
	a, c := newApplicationAccessHandler(t)
	body := `{"group":"platform"}`
	request := httptest.NewRequest(http.MethodPut, "http://dashboard/api/administration/application-access", strings.NewReader(body))
	request.Header.Set("Origin", "http://dashboard")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	a.putApplicationAccess(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", response.Code, response.Body)
	}

	var namespace corev1.Namespace
	if err := c.Get(context.Background(), client.ObjectKey{Name: "releaseyard-team-platform"}, &namespace); err != nil {
		t.Fatal(err)
	}
	if namespace.Labels[teamNamespaceLabel] != "true" {
		t.Fatalf("unexpected namespace labels: %#v", namespace.Labels)
	}

	var binding rbacv1.RoleBinding
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "releaseyard-team-platform", Name: "releaseyard-application-editor"}, &binding); err != nil {
		t.Fatal(err)
	}
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != "releaseyard-application-editor-role" {
		t.Fatalf("unexpected roleRef: %#v", binding.RoleRef)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "Group" || binding.Subjects[0].Name != "platform" {
		t.Fatalf("unexpected subjects: %#v", binding.Subjects)
	}

	listRequest := httptest.NewRequest(http.MethodGet, "http://dashboard/api/administration/application-access", nil)
	listResponse := httptest.NewRecorder()
	a.getApplicationAccess(listResponse, listRequest)
	var listed applicationAccessResponse
	if err := json.Unmarshal(listResponse.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Rules) != 1 || listed.Rules[0].Group != "platform" || listed.Rules[0].Namespace != "releaseyard-team-platform" {
		t.Fatalf("unexpected listed rules: %#v", listed.Rules)
	}

	// Revoking removes the binding but keeps the namespace and its Applications.
	deleteRequest := httptest.NewRequest(http.MethodDelete, "http://dashboard/api/administration/application-access?group=platform", nil)
	deleteRequest.Header.Set("Origin", "http://dashboard")
	deleteResponse := httptest.NewRecorder()
	a.deleteApplicationAccess(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("DELETE status=%d body=%s", deleteResponse.Code, deleteResponse.Body)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "releaseyard-team-platform", Name: "releaseyard-application-editor"}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("binding still present after revoke: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "releaseyard-team-platform"}, &corev1.Namespace{}); err != nil {
		t.Fatalf("namespace should be kept after revoke: %v", err)
	}
}

func TestApplicationAccessRejectsInvalidInput(t *testing.T) {
	a, _ := newApplicationAccessHandler(t)
	request := httptest.NewRequest(http.MethodPut, "http://dashboard/api/administration/application-access", strings.NewReader(`{"group":""}`))
	request.Header.Set("Origin", "http://dashboard")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	a.putApplicationAccess(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("empty group status=%d, want 400", response.Code)
	}

	bad := httptest.NewRequest(http.MethodPut, "http://dashboard/api/administration/application-access", strings.NewReader(`{"group":"!!!"}`))
	bad.Header.Set("Origin", "http://dashboard")
	bad.Header.Set("Content-Type", "application/json")
	badResponse := httptest.NewRecorder()
	a.putApplicationAccess(badResponse, bad)
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("unmappable group status=%d, want 400", badResponse.Code)
	}
}
