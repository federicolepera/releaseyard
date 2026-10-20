package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
)

func resourceDefinition(spec platformv1alpha1.ResourceDefinitionSpec) *platformv1alpha1.ResourceDefinition {
	return &platformv1alpha1.ResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "bucket"},
		Spec:       spec,
	}
}

func cloudTarget(name string) *platformv1alpha1.CloudTarget {
	return &platformv1alpha1.CloudTarget{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: platformv1alpha1.CloudTargetSpec{
			Provider:          "aws",
			Region:            "eu-west-1",
			ProviderConfigRef: platformv1alpha1.ProviderConfigReference{Name: "localstack", Namespace: testNamespace},
		},
	}
}

func TestValidateResourceValues(t *testing.T) {
	minimum, maximum := int64(1), int64(5)
	definition := resourceDefinition(platformv1alpha1.ResourceDefinitionSpec{
		ConfigurableValues: []platformv1alpha1.ResourceDefinitionValue{
			{Path: "bucketName", Type: "string", Required: true},
			{Path: "versioning", Type: "boolean"},
			{Path: "settings.versioning", Type: "boolean"},
			{Path: "retention", Type: "integer", Minimum: &minimum, Maximum: &maximum},
		},
	})
	tests := []struct {
		name    string
		values  string
		wantErr bool
	}{
		{name: "valid values", values: `{"bucketName":"assets","versioning":true,"retention":3}`},
		{name: "optional values omitted", values: `{"bucketName":"assets"}`},
		{name: "nested values", values: `{"bucketName":"assets","settings":{"versioning":true}}`},
		{name: "undeclared nested value", values: `{"bucketName":"assets","settings":{"unknown":true}}`, wantErr: true},
		{name: "required value missing", values: `{}`, wantErr: true},
		{name: "required value null", values: `{"bucketName":null}`, wantErr: true},
		{name: "undeclared value", values: `{"bucketName":"assets","unknown":1}`, wantErr: true},
		{name: "wrong string type", values: `{"bucketName":42}`, wantErr: true},
		{name: "wrong boolean type", values: `{"bucketName":"assets","versioning":"yes"}`, wantErr: true},
		{name: "non integral number", values: `{"bucketName":"assets","retention":1.5}`, wantErr: true},
		{name: "below minimum", values: `{"bucketName":"assets","retention":0}`, wantErr: true},
		{name: "above maximum", values: `{"bucketName":"assets","retention":6}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values, err := decodeResourceValues(json.RawMessage(test.values))
			if err != nil {
				t.Fatalf("decodeResourceValues() error = %v", err)
			}
			err = validateResourceValues(definition, values)
			if test.wantErr && err == nil {
				t.Fatal("validateResourceValues() = nil, want an error")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateResourceValues() error = %v", err)
			}
		})
	}
}

func TestCreateListAndDeleteCloudResource(t *testing.T) {
	definition := resourceDefinition(platformv1alpha1.ResourceDefinitionSpec{
		ConfigurableValues: []platformv1alpha1.ResourceDefinitionValue{
			{Path: "bucketName", Type: "string", Required: true},
		},
	})
	target := cloudTarget("localstack")
	h, c := testHandler(t, definition, target)

	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/resources", strings.NewReader(body))
		r.Header.Set("Content-Type", testJSONContentType)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	if w := post(`{"name":"assets","definitionName":"bucket","targetName":"localstack","values":{"bucketName":"assets"}}`); w.Code != http.StatusCreated {
		t.Fatalf("POST status=%d body=%s", w.Code, w.Body)
	}
	stored := &platformv1alpha1.Resource{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "assets"}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.DefinitionRef.Name != "bucket" || stored.Spec.TargetRef.Name != "localstack" {
		t.Fatalf("unexpected references: %+v", stored.Spec)
	}
	if stored.Spec.Values == nil || string(stored.Spec.Values.Raw) != `{"bucketName":"assets"}` {
		t.Fatalf("values not preserved: %v", stored.Spec.Values)
	}

	if w := post(`{"name":"invalid","definitionName":"bucket","targetName":"localstack","values":{}}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST invalid status=%d body=%s", w.Code, w.Body)
	}
	if w := post(`{"name":"duplicate","definitionName":"bucket","targetName":"localstack","values":{"bucketName":"assets"}}`); w.Code != http.StatusCreated {
		t.Fatalf("POST duplicate setup status=%d body=%s", w.Code, w.Body)
	}

	listRequest := httptest.NewRequest(http.MethodGet, "/api/resources", nil)
	listRecorder := httptest.NewRecorder()
	h.ServeHTTP(listRecorder, listRequest)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", listRecorder.Code, listRecorder.Body)
	}
	var list platformv1alpha1.ResourceList
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("GET returned %d resources, want 2", len(list.Items))
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/api/resources/assets", nil)
	deleteRecorder := httptest.NewRecorder()
	h.ServeHTTP(deleteRecorder, deleteRequest)
	if deleteRecorder.Code != http.StatusNoContent {
		t.Fatalf("DELETE status=%d body=%s", deleteRecorder.Code, deleteRecorder.Body)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "assets"}, &platformv1alpha1.Resource{}); err == nil {
		t.Fatal("deleted resource still exists")
	}
}

func TestCreateCloudResourceValidatesReferences(t *testing.T) {
	h, _ := testHandler(t)
	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/resources", strings.NewReader(body))
		r.Header.Set("Content-Type", testJSONContentType)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post(`{"name":"assets","definitionName":"missing","targetName":"localstack","values":{}}`); w.Code != http.StatusNotFound {
		t.Fatalf("missing definition status=%d body=%s", w.Code, w.Body)
	}
	definition := resourceDefinition(platformv1alpha1.ResourceDefinitionSpec{
		ConfigurableValues: []platformv1alpha1.ResourceDefinitionValue{{Path: "bucketName", Type: "string"}},
	})
	h, _ = testHandler(t, definition)
	if w := post(`{"name":"assets","definitionName":"bucket","targetName":"missing","values":{"bucketName":"assets"}}`); w.Code != http.StatusNotFound {
		t.Fatalf("missing target status=%d body=%s", w.Code, w.Body)
	}
}

func TestCloudNamespaceFallsBackToTargetNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := authorizationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	allowed := true
	impersonated := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if review, ok := obj.(*authorizationv1.SelfSubjectAccessReview); ok {
				review.Status.Allowed = allowed
				return nil
			}
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
	s := &server{client: impersonated, namespace: testNamespace}
	request := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/resources", nil)
		ctx := withSession(r.Context(), dashboardSession{authMethod: authMethodOIDC, username: "admin"})
		ctx = withImpersonatingClient(ctx, impersonated)
		return r.WithContext(ctx)
	}

	if namespace, ok := s.cloudNamespace(request(), "", "releaseyard-team-platform"); !ok || namespace != "releaseyard-team-platform" {
		t.Fatalf("cloudNamespace() = %q, %v, want the target namespace", namespace, ok)
	}
	allowed = false
	if _, ok := s.cloudNamespace(request(), "", "releaseyard-team-platform"); ok {
		t.Fatal("cloudNamespace() allowed a namespace without permission")
	}

	r := httptest.NewRequest(http.MethodPost, "/api/resources", nil)
	ctx := withSession(r.Context(), dashboardSession{authMethod: authMethodOIDC, username: "member"})
	ctx = withTeams(ctx, []string{"releaseyard-team-platform"})
	ctx = withImpersonatingClient(ctx, impersonated)
	r = r.WithContext(ctx)
	if namespace, ok := s.cloudNamespace(r, "releaseyard-team-platform", ""); !ok || namespace != "releaseyard-team-platform" {
		t.Fatalf("cloudNamespace() = %q, %v, want the selected team", namespace, ok)
	}
}
