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
)

func newPlatformAccessHandler(t *testing.T) (*authHandler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	bootstrap := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "install-admin"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "releaseyard-platform-admin"},
		Subjects:   []rbacv1.Subject{{Kind: "Group", APIGroup: rbacv1.GroupName, Name: "administrators"}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(bootstrap).Build()
	return &authHandler{k8s: c, platformAdminRole: "releaseyard-platform-admin"}, c
}

func TestPlatformAccessCreatesListsAndRemovesBinding(t *testing.T) {
	a, c := newPlatformAccessHandler(t)
	body := `{"subjectKind":"Group","subject":"platform-team"}`
	request := httptest.NewRequest(http.MethodPut, "http://dashboard/api/administration/platform-access", strings.NewReader(body))
	request.Header.Set("Origin", "http://dashboard")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	a.putPlatformAccess(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", response.Code, response.Body)
	}

	bindings := &rbacv1.ClusterRoleBindingList{}
	if err := c.List(context.Background(), bindings); err != nil {
		t.Fatal(err)
	}
	var managed *rbacv1.ClusterRoleBinding
	for i := range bindings.Items {
		if bindings.Items[i].Labels[platformAccessLabel] == platformAccessManagedBy {
			managed = &bindings.Items[i]
		}
	}
	if managed == nil || managed.RoleRef.Name != "releaseyard-platform-admin" {
		t.Fatalf("managed binding not created: %#v", bindings.Items)
	}
	if len(managed.Subjects) != 1 || managed.Subjects[0].Name != "platform-team" || managed.Subjects[0].Kind != "Group" {
		t.Fatalf("unexpected subjects: %#v", managed.Subjects)
	}

	listRequest := httptest.NewRequest(http.MethodGet, "http://dashboard/api/administration/platform-access", nil)
	listResponse := httptest.NewRecorder()
	a.getPlatformAccess(listResponse, listRequest)
	var listed platformAdminResponse
	if err := json.Unmarshal(listResponse.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Role != "releaseyard-platform-admin" || len(listed.Rules) != 2 {
		t.Fatalf("unexpected response: %#v", listed)
	}
	var foundManaged, foundBootstrap bool
	for _, rule := range listed.Rules {
		if rule.Managed {
			foundManaged = true
		}
		if rule.Subject == "administrators" && !rule.Managed {
			foundBootstrap = true
		}
	}
	if !foundManaged || !foundBootstrap {
		t.Fatalf("unexpected rules: %#v", listed.Rules)
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "http://dashboard/api/administration/platform-access?binding="+managed.Name, nil)
	deleteRequest.Header.Set("Origin", "http://dashboard")
	deleteResponse := httptest.NewRecorder()
	a.deletePlatformAccess(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("DELETE status=%d body=%s", deleteResponse.Code, deleteResponse.Body)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: managed.Name}, &rbacv1.ClusterRoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("binding still present: %v", err)
	}
}

func TestPlatformAccessRejectsInvalidSubject(t *testing.T) {
	a, _ := newPlatformAccessHandler(t)
	for _, body := range []string{
		`{"subjectKind":"Robot","subject":"platform-team"}`,
		`{"subjectKind":"Group","subject":""}`,
		`{"subjectKind":"Group","subject":"!!!"}`,
	} {
		request := httptest.NewRequest(http.MethodPut, "http://dashboard/api/administration/platform-access", strings.NewReader(body))
		request.Header.Set("Origin", "http://dashboard")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		a.putPlatformAccess(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d, want 400", body, response.Code)
		}
	}
}
