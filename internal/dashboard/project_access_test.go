package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
)

func newProjectAccessHandler(t *testing.T) (*authHandler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	project := &platformv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       platformv1alpha1.ProjectSpec{Namespace: "releaseyard-team-payments"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(project).Build()
	return &authHandler{
		k8s:               c,
		projectRolePrefix: "releaseyard-project-",
	}, c
}

func TestProjectAccessCreatesAndRemovesBinding(t *testing.T) {
	a, c := newProjectAccessHandler(t)
	body := `{"project":"payments","role":"requester","subjectKind":"Group","subject":"payments-devs"}`
	request := httptest.NewRequest(http.MethodPut, "http://dashboard/api/administration/project-access", strings.NewReader(body))
	request.Header.Set("Origin", "http://dashboard")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	a.putProjectAccess(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", response.Code, response.Body)
	}

	bindings := &rbacv1.RoleBindingList{}
	if err := c.List(context.Background(), bindings); err != nil {
		t.Fatal(err)
	}
	if len(bindings.Items) != 1 {
		t.Fatalf("bindings=%#v", bindings.Items)
	}
	binding := bindings.Items[0]
	if binding.Namespace != "releaseyard-team-payments" || binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != "releaseyard-project-requester" {
		t.Fatalf("unexpected binding: %#v", binding)
	}
	if binding.Labels[projectAccessProjectLabel] != "payments" || binding.Labels[projectAccessRoleLabel] != "requester" {
		t.Fatalf("unexpected labels: %#v", binding.Labels)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "Group" || binding.Subjects[0].Name != "payments-devs" {
		t.Fatalf("unexpected subjects: %#v", binding.Subjects)
	}

	listRequest := httptest.NewRequest(http.MethodGet, "http://dashboard/api/administration/project-access", nil)
	listResponse := httptest.NewRecorder()
	a.getProjectAccess(listResponse, listRequest)
	var listed projectAccessResponse
	if err := json.Unmarshal(listResponse.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Projects) != 1 || len(listed.Roles) != 4 || len(listed.Rules) != 1 {
		t.Fatalf("unexpected response: %#v", listed)
	}
	if listed.Rules[0].Role != "requester" || listed.Rules[0].Subject != "payments-devs" {
		t.Fatalf("unexpected rule: %#v", listed.Rules[0])
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "http://dashboard/api/administration/project-access?project=payments&binding="+binding.Name, nil)
	deleteRequest.Header.Set("Origin", "http://dashboard")
	deleteResponse := httptest.NewRecorder()
	a.deleteProjectAccess(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("DELETE status=%d body=%s", deleteResponse.Code, deleteResponse.Body)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: binding.Namespace, Name: binding.Name}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("binding still present: %v", err)
	}
}

func TestProjectAccessRejectsInvalidRoleAndSubject(t *testing.T) {
	a, _ := newProjectAccessHandler(t)
	for _, body := range []string{
		`{"project":"payments","role":"unknown","subjectKind":"Group","subject":"devs"}`,
		`{"project":"payments","role":"viewer","subjectKind":"Robot","subject":"devs"}`,
		`{"project":"payments","role":"viewer","subjectKind":"Group","subject":""}`,
	} {
		request := httptest.NewRequest(http.MethodPut, "http://dashboard/api/administration/project-access", strings.NewReader(body))
		request.Header.Set("Origin", "http://dashboard")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		a.putProjectAccess(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d, want 400", body, response.Code)
		}
	}
}
