package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	testNamespace        = "idp-system"
	testApplicationsPath = "/api/applications"
	testClustersPath     = "/api/clusters"
	testClusterName      = "production"
	testDefinitionName   = "sample"
	testOtherNamespace   = "other"
	testOutsideName      = "outside"
	testEditableName     = "editable"
	testJSONContentType  = "application/json"
)

func testHandler(t *testing.T, objects ...client.Object) (http.Handler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := authorizationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return NewHandler(c, testNamespace), c
}

func validDefinition(name string, current bool) *platformv1alpha1.ApplicationDefinition {
	definition := &platformv1alpha1.ApplicationDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
		Spec: platformv1alpha1.ApplicationDefinitionSpec{
			Chart:   platformv1alpha1.ApplicationDefinitionChart{Repository: "https://charts.example", Name: testDefinitionName, Version: "1.2.3"},
			Release: platformv1alpha1.ApplicationDefinitionRelease{Namespace: "workloads"},
			ConfigurableValues: []platformv1alpha1.ApplicationDefinitionValue{
				{Path: "replica.count", Type: "integer", Required: true},
				{Path: "feature.enabled", Type: "boolean"},
			},
		},
	}
	if current {
		definition.Status.ObservedGeneration = definition.Generation
		definition.Status.Conditions = []metav1.Condition{{Type: "Validated", Status: metav1.ConditionTrue, ObservedGeneration: 1}}
	}
	return definition
}

func TestListsUseFixedScopes(t *testing.T) {
	definition := validDefinition("inside", true)
	definition.Labels = map[string]string{"platform.releaseyard.io/type": "template"}
	unlabeledDefinition := validDefinition("unlabeled", true)
	otherDefinition := validDefinition(testOutsideName, true)
	app := &platformv1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "inside", Namespace: testNamespace}}
	otherApp := &platformv1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: testOutsideName, Namespace: testOtherNamespace}}
	cluster := &platformv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "one"}}
	h, _ := testHandler(t, definition, unlabeledDefinition, otherDefinition, app, otherApp, cluster)
	for _, path := range []string{"/api/applicationdefinitions", testApplicationsPath, testClustersPath} {
		r := httptest.NewRequest(http.MethodGet, path+"?namespace=other", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
			t.Fatalf("GET %s: status=%d headers=%v body=%s", path, w.Code, w.Header(), w.Body)
		}
		var body struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Items == nil {
			t.Fatalf("GET %s returned invalid List JSON: %v", path, err)
		}
		wantItems := 1
		if path == "/api/applicationdefinitions" {
			wantItems = 3
		}
		if path != testClustersPath && len(body.Items) != wantItems {
			t.Fatalf("GET %s returned %d namespaced items, want %d", path, len(body.Items), wantItems)
		}
		if path == testClustersPath && len(body.Items) != 1 {
			t.Fatalf("GET clusters returned %d items, want cluster-scoped item", len(body.Items))
		}
		if path == "/api/applicationdefinitions" {
			var definitionList struct {
				Items []struct {
					Metadata struct {
						Name   string            `json:"name"`
						Labels map[string]string `json:"labels"`
					} `json:"metadata"`
				} `json:"items"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &definitionList); err != nil {
				t.Fatalf("GET %s returned invalid Definition metadata: %v", path, err)
			}
			foundTemplate, foundUnlabeled := false, false
			for _, item := range definitionList.Items {
				switch item.Metadata.Name {
				case "inside":
					foundTemplate = item.Metadata.Labels["platform.releaseyard.io/type"] == "template"
				case "unlabeled":
					_, hasReservedLabel := item.Metadata.Labels["platform.releaseyard.io/type"]
					foundUnlabeled = !hasReservedLabel
				}
			}
			if !foundTemplate || !foundUnlabeled {
				t.Fatalf("GET %s did not preserve template label and unlabeled Definition metadata: %+v", path, definitionList.Items)
			}
		}
	}
}

func TestUpdateClusterEnvironment(t *testing.T) {
	cluster := &platformv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "env-cluster", UID: "uid-1", ResourceVersion: "7", Labels: map[string]string{"keep": "yes", clusterEnvironmentLabel: "old"}},
		Spec:       platformv1alpha1.ClusterSpec{KubeconfigSecretRef: platformv1alpha1.KubeconfigSecretReference{Name: "credentials"}},
		Status:     platformv1alpha1.ClusterStatus{Conditions: []metav1.Condition{{Type: "Connected", Status: metav1.ConditionTrue}}},
	}
	h, c := testHandler(t, cluster)
	w := patchClusterEnvironment(h, `{"uid":"uid-1","resourceVersion":"7","environment":"production"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH status=%d body=%s", w.Code, w.Body)
	}
	stored := &platformv1alpha1.Cluster{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "env-cluster"}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Labels[clusterEnvironmentLabel] != "production" || stored.Labels["keep"] != "yes" || stored.Spec.KubeconfigSecretRef.Name != "credentials" || len(stored.Status.Conditions) != 1 {
		t.Fatalf("PATCH changed unexpected fields: %+v", stored)
	}
	if got := patchClusterEnvironment(h, `{"uid":"uid-1","resourceVersion":"7","environment":null}`); got.Code != http.StatusConflict {
		t.Fatalf("stale resourceVersion status=%d, want 409", got.Code)
	}
	stored.ResourceVersion = "8"
	if err := c.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "env-cluster"}, stored); err != nil {
		t.Fatal(err)
	}
	currentVersion := stored.ResourceVersion
	if got := patchClusterEnvironment(h, fmt.Sprintf(`{"uid":"wrong","resourceVersion":%q,"environment":null}`, currentVersion)); got.Code != http.StatusConflict {
		t.Fatalf("stale UID status=%d, want 409", got.Code)
	}
	if got := patchClusterEnvironment(h, fmt.Sprintf(`{"uid":"uid-1","resourceVersion":%q,"environment":null}`, currentVersion)); got.Code != http.StatusOK {
		t.Fatalf("remove environment status=%d body=%s", got.Code, got.Body)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "env-cluster"}, stored); err != nil {
		t.Fatal(err)
	}
	if _, exists := stored.Labels[clusterEnvironmentLabel]; exists || stored.Labels["keep"] != "yes" {
		t.Fatalf("null did not remove only the environment label: %v", stored.Labels)
	}
}

func TestClusterEnvironmentRequestValidation(t *testing.T) {
	cluster := &platformv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "env-cluster", UID: "uid-1", ResourceVersion: "7"}}
	h, _ := testHandler(t, cluster)
	for _, tc := range []struct {
		body, origin, contentType string
		want                      int
	}{
		{body: `{"uid":"uid-1","resourceVersion":"7","environment":"x"}`, origin: "https://evil.example", contentType: testJSONContentType, want: http.StatusForbidden},
		{body: `{"uid":"uid-1","resourceVersion":"7","environment":""}`, contentType: testJSONContentType, want: http.StatusBadRequest},
		{body: `{"uid":"uid-1","resourceVersion":"7"}`, contentType: testJSONContentType, want: http.StatusBadRequest},
		{body: `{"uid":"uid-1","resourceVersion":"7","environment":"bad value"}`, contentType: testJSONContentType, want: http.StatusBadRequest},
		{body: `{"uid":"uid-1","resourceVersion":"7","environment":"x","extra":true}`, contentType: testJSONContentType, want: http.StatusBadRequest},
		{body: `{"uid":"uid-1","resourceVersion":"7","environment":"x"} {}`, contentType: testJSONContentType, want: http.StatusBadRequest},
		{body: `{"uid":"uid-1","resourceVersion":"7","environment":"x"}`, contentType: "text/plain", want: http.StatusUnsupportedMediaType},
	} {
		r := httptest.NewRequest(http.MethodPatch, "/api/clusters/env-cluster/environment", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", tc.contentType)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body)
		}
	}
	large := fmt.Sprintf(`{"uid":"u","resourceVersion":"1","environment":"%s"}`, strings.Repeat("x", maxRequestBody))
	if w := patchClusterEnvironment(h, large); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status=%d want=413", w.Code)
	}
}

func TestPreviewEnvironmentTargets(t *testing.T) {
	makeCluster := func(name string, uid types.UID, env string, connected bool) *platformv1alpha1.Cluster {
		status := metav1.ConditionFalse
		if connected {
			status = metav1.ConditionTrue
		}
		return &platformv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid, Labels: map[string]string{clusterEnvironmentLabel: env}}, Status: platformv1alpha1.ClusterStatus{Conditions: []metav1.Condition{{Type: "Connected", Status: status}}}}
	}
	a := makeCluster("alpha", "uid-a", "prod", true)
	b := makeCluster("beta", "uid-b", "prod", false)
	now := metav1.Now()
	deleting := makeCluster("gamma", "uid-c", "prod", false)
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{"test.finalizer"}
	other := makeCluster("outside", "uid-x", "elsewhere", true)
	h, c := testHandler(t, b, other, a, deleting)
	request := func(handler http.Handler) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/environments/prod/targets", nil))
		return w
	}
	w := request(h)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", w.Code, w.Body)
	}
	var response environmentTargetsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Targets) != 3 || response.Targets[0].Name != "alpha" || response.Targets[1].Name != "beta" || response.Targets[2].Name != "gamma" || response.Targets[0].Connected != true || response.Targets[1].Connected != false || !response.Targets[2].Deleting {
		t.Fatalf("unexpected preview targets: %+v", response.Targets)
	}
	hash := sha256.New()
	writeDigestPart(hash, "prod")
	writeDigestPart(hash, "alpha")
	writeDigestPart(hash, "uid-a")
	writeDigestPart(hash, "beta")
	writeDigestPart(hash, "uid-b")
	writeDigestPart(hash, "gamma")
	writeDigestPart(hash, "uid-c")
	if response.Digest != hex.EncodeToString(hash.Sum(nil)) {
		t.Fatalf("digest=%q", response.Digest)
	}
	second, digest, err := resolveEnvironmentTargets(context.Background(), c, "prod")
	if err != nil || digest != response.Digest || second[0].Name != "alpha" {
		t.Fatalf("unstable preview: %v %q", err, digest)
	}
	changed := a.DeepCopy()
	changed.UID = "new-uid"
	if err := c.Update(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	_, changedDigest, err := resolveEnvironmentTargets(context.Background(), c, "prod")
	if err != nil || changedDigest == response.Digest {
		t.Fatalf("membership UID change did not change digest: %v", err)
	}
	changed.Labels = map[string]string{}
	if err := c.Update(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	_, membershipDigest, err := resolveEnvironmentTargets(context.Background(), c, "prod")
	if err != nil || membershipDigest == changedDigest {
		t.Fatalf("membership change did not change digest: %v", err)
	}
	emptyHandler, _ := testHandler(t)
	empty := httptest.NewRecorder()
	emptyHandler.ServeHTTP(empty, httptest.NewRequest(http.MethodGet, "/api/environments/prod/targets", nil))
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"targets":[]`) {
		t.Fatalf("empty preview status=%d body=%s", empty.Code, empty.Body)
	}
}

func TestPreviewEnvironmentTargetLimit(t *testing.T) {
	clusters := func(count int) []client.Object {
		objects := make([]client.Object, 0, count)
		for i := range count {
			name := fmt.Sprintf("target-%02d", i)
			objects = append(objects, &platformv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{
				Name: name, UID: types.UID("uid-" + name),
				Labels: map[string]string{clusterEnvironmentLabel: "prod"},
			}})
		}
		return objects
	}
	request := func(objects ...client.Object) *httptest.ResponseRecorder {
		h, _ := testHandler(t, objects...)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/environments/prod/targets", nil))
		return w
	}
	t.Run("exactly 32", func(t *testing.T) {
		w := request(clusters(32)...)
		if w.Code != http.StatusOK {
			t.Fatalf("GET status=%d, want 200 body=%s", w.Code, w.Body)
		}
		var response environmentTargetsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Targets) != 32 {
			t.Fatalf("preview returned %d targets, want 32", len(response.Targets))
		}
	})
	t.Run("33 exceeds limit", func(t *testing.T) {
		w := request(clusters(33)...)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("GET status=%d, want 422 body=%s", w.Code, w.Body)
		}
	})
}

func TestPreviewDigestChangesAfterClusterRecreation(t *testing.T) {
	oldCluster := &platformv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{
		Name: "recreated-target", UID: "old-uid", Labels: map[string]string{clusterEnvironmentLabel: "prod"},
	}}
	h, c := testHandler(t, oldCluster)
	previewDigest := func() string {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/environments/prod/targets", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET status=%d body=%s", w.Code, w.Body)
		}
		var response environmentTargetsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.Digest
	}
	oldDigest := previewDigest()
	if err := c.Delete(context.Background(), oldCluster); err != nil {
		t.Fatal(err)
	}
	newCluster := oldCluster.DeepCopy()
	newCluster.UID = "new-uid"
	newCluster.ResourceVersion = ""
	if err := c.Create(context.Background(), newCluster); err != nil {
		t.Fatal(err)
	}
	newDigest := previewDigest()
	if newDigest == oldDigest {
		t.Fatalf("digest did not change after same-name Cluster recreation: %q", newDigest)
	}
}

func patchClusterEnvironment(handler http.Handler, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPatch, "/api/clusters/env-cluster/environment", strings.NewReader(body))
	r.Header.Set("Content-Type", testJSONContentType)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestCreateApplication(t *testing.T) {
	definition := validDefinition(testDefinitionName, true)
	cluster := &platformv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: testClusterName}}
	h, c := testHandler(t, definition, cluster)
	w := postJSON(h, `{"name":"my-app","description":"Customer portal","definitionName":"sample","clusterName":"production","values":{"replica":{"count":3},"feature":{"enabled":true}}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST status=%d body=%s", w.Code, w.Body)
	}
	app := &platformv1alpha1.Application{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "my-app"}, app); err != nil {
		t.Fatal(err)
	}
	if app.Spec.Values == nil || string(app.Spec.Values.Raw) != `{"replica":{"count":3},"feature":{"enabled":true}}` {
		t.Fatalf("values not preserved as raw JSON: %s", app.Spec.Values)
	}
	if app.Spec.Description != "Customer portal" {
		t.Fatalf("description not preserved: %q", app.Spec.Description)
	}
	if app.Spec.ApplicationDefinitionRef.Name != testDefinitionName || app.Spec.ClusterRef == nil || app.Spec.ClusterRef.Name != testClusterName {
		t.Fatalf("unexpected references: %+v", app.Spec)
	}
	if app.Spec.ReleaseNamespace != nil {
		t.Fatalf("omitted release namespace should use the definition default, got %q", *app.Spec.ReleaseNamespace)
	}
}

func TestCreateApplicationForEnvironment(t *testing.T) {
	definition := validDefinition(testDefinitionName, true)
	makeCluster := func(name string, uid types.UID) *platformv1alpha1.Cluster {
		return &platformv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid, Labels: map[string]string{clusterEnvironmentLabel: "prod"}}}
	}

	t.Run("stores a dynamic environment selector", func(t *testing.T) {
		h, c := testHandler(t, definition, makeCluster("alpha", "uid-a"), makeCluster("beta", "uid-b"))
		body := `{"name":"env-app","definitionName":"sample","environmentName":"prod","values":{"replica":{"count":3}}}`
		w := postJSON(h, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("POST status=%d body=%s", w.Code, w.Body)
		}
		stored := &platformv1alpha1.Application{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "env-app"}, stored); err != nil {
			t.Fatal(err)
		}
		if stored.Spec.ClusterRef != nil || stored.Spec.Environment == nil || stored.Spec.Environment.Name != "prod" || len(stored.Spec.Environment.Targets) != 0 {
			t.Fatalf("unexpected dynamic environment selector: %+v", stored.Spec)
		}
	})

	t.Run("ignores a preview digest instead of pinning targets", func(t *testing.T) {
		h, c := testHandler(t, definition, makeCluster("alpha", "uid-a"))
		body := `{"name":"preview-app","definitionName":"sample","environmentName":"prod","previewDigest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","values":{"replica":{"count":1}}}`
		if w := postJSON(h, body); w.Code != http.StatusCreated {
			t.Fatalf("POST status=%d body=%s", w.Code, w.Body)
		}
		stored := &platformv1alpha1.Application{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "preview-app"}, stored); err != nil {
			t.Fatal(err)
		}
		if stored.Spec.Environment == nil || len(stored.Spec.Environment.Targets) != 0 {
			t.Fatalf("preview digest pinned environment targets: %+v", stored.Spec.Environment)
		}
	})

	t.Run("request modes", func(t *testing.T) {
		h, _ := testHandler(t, definition, makeCluster("alpha", "uid-a"))
		for _, body := range []string{
			`{"name":"bad-app","definitionName":"sample"}`,
			`{"name":"bad-app","definitionName":"sample","clusterName":"alpha","environmentName":"prod"}`,
			`{"name":"bad-app","definitionName":"sample","environmentName":"bad value"}`,
			`{"name":"bad-app","definitionName":"sample","environmentName":"prod","targets":[]}`,
		} {
			if w := postJSON(h, body); w.Code != http.StatusBadRequest {
				t.Fatalf("POST body=%s status=%d, want 400", body, w.Code)
			}
		}
	})
}

func TestCreateApplicationForExplicitClusters(t *testing.T) {
	definition := validDefinition(testDefinitionName, true)
	makeCluster := func(name string, uid types.UID) *platformv1alpha1.Cluster {
		return &platformv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}}
	}

	t.Run("stores the selected cluster identities", func(t *testing.T) {
		h, c := testHandler(t, definition, makeCluster("alpha", "uid-a"), makeCluster("beta", "uid-b"))
		body := `{"name":"selected-app","definitionName":"sample","clusters":[{"name":"beta","uid":"uid-b"},{"name":"alpha","uid":"uid-a"}],"values":{"replica":{"count":3}}}`
		w := postJSON(h, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("POST status=%d body=%s", w.Code, w.Body)
		}
		stored := &platformv1alpha1.Application{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "selected-app"}, stored); err != nil {
			t.Fatal(err)
		}
		if stored.Spec.ClusterRef != nil || stored.Spec.Environment != nil {
			t.Fatalf("explicit selection also set another target mode: %+v", stored.Spec)
		}
		if len(stored.Spec.Clusters) != 2 || stored.Spec.Clusters[0].Name != "beta" || stored.Spec.Clusters[0].UID != "uid-b" || stored.Spec.Clusters[1].Name != "alpha" || stored.Spec.Clusters[1].UID != "uid-a" {
			t.Fatalf("unexpected explicit cluster selection: %+v", stored.Spec.Clusters)
		}
	})

	t.Run("request validation", func(t *testing.T) {
		h, _ := testHandler(t, definition, makeCluster("alpha", "uid-a"), makeCluster("beta", "uid-b"))
		for _, tc := range []struct {
			body string
			want int
		}{
			{body: `{"name":"bad-app","definitionName":"sample","clusters":[]}`, want: http.StatusBadRequest},
			{body: `{"name":"bad-app","definitionName":"sample","clusterName":"alpha","clusters":[{"name":"beta","uid":"uid-b"}]}`, want: http.StatusBadRequest},
			{body: `{"name":"bad-app","definitionName":"sample","clusters":[{"name":"alpha","uid":"uid-a"},{"name":"alpha","uid":"uid-b"}]}`, want: http.StatusBadRequest},
			{body: `{"name":"bad-app","definitionName":"sample","clusters":[{"name":"alpha","uid":"stale"}]}`, want: http.StatusConflict},
			{body: `{"name":"bad-app","definitionName":"sample","clusters":[{"name":"missing","uid":"uid-x"}]}`, want: http.StatusNotFound},
		} {
			if w := postJSON(h, tc.body); w.Code != tc.want {
				t.Fatalf("POST body=%s status=%d, want %d: %s", tc.body, w.Code, tc.want, w.Body)
			}
		}
	})
}

func TestDeleteApplication(t *testing.T) {
	t.Run("accepted and removed", func(t *testing.T) {
		app := &platformv1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "delete-me", Namespace: testNamespace}}
		h, c := testHandler(t, app)
		w := deleteApplication(h, "/api/applications/delete-me")
		if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
			t.Fatalf("DELETE status=%d body=%q, want 204 with no body", w.Code, w.Body)
		}
		assertApplicationAbsent(t, c, "delete-me")
	})

	t.Run("query namespace cannot change scope", func(t *testing.T) {
		outside := &platformv1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: testOutsideName, Namespace: testOtherNamespace}}
		h, c := testHandler(t, outside)
		w := deleteApplication(h, "/api/applications/outside?namespace=other")
		if w.Code != http.StatusNotFound {
			t.Fatalf("DELETE status=%d, want 404 body=%s", w.Code, w.Body)
		}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: testOtherNamespace, Name: testOutsideName}, &platformv1alpha1.Application{}); err != nil {
			t.Fatalf("out-of-scope Application was changed: %v", err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		h, _ := testHandler(t)
		if w := deleteApplication(h, "/api/applications/missing"); w.Code != http.StatusNotFound {
			t.Fatalf("DELETE status=%d, want 404 body=%s", w.Code, w.Body)
		}
	})

	t.Run("invalid name", func(t *testing.T) {
		h, _ := testHandler(t)
		if w := deleteApplication(h, "/api/applications/bad_name"); w.Code != http.StatusBadRequest {
			t.Fatalf("DELETE status=%d, want 400 body=%s", w.Code, w.Body)
		}
	})

	t.Run("cross origin", func(t *testing.T) {
		app := &platformv1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "keep-me", Namespace: testNamespace}}
		h, c := testHandler(t, app)
		r := httptest.NewRequest(http.MethodDelete, "/api/applications/keep-me", nil)
		r.Header.Set("Origin", "https://evil.example")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("DELETE status=%d, want 403 body=%s", w.Code, w.Body)
		}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "keep-me"}, &platformv1alpha1.Application{}); err != nil {
			t.Fatalf("Application was deleted despite forbidden origin: %v", err)
		}
	})
}

func TestUpdateApplicationValues(t *testing.T) {
	t.Run("edits values for environment-selected application", func(t *testing.T) {
		app := &platformv1alpha1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: "environment-app", Namespace: testNamespace, Generation: 2},
			Spec: platformv1alpha1.ApplicationSpec{
				ApplicationDefinitionRef: platformv1alpha1.ApplicationReference{Name: testDefinitionName},
				Environment:              &platformv1alpha1.ApplicationEnvironmentSelection{Name: "prod", Targets: []platformv1alpha1.ApplicationEnvironmentTarget{{Name: "alpha", UID: "uid-a"}}},
			},
		}
		h, _ := testHandler(t, validDefinition(testDefinitionName, true), app)
		w := patchValues(h, "/api/applications/environment-app", `{"generation":2,"values":{"replica":{"count":2}}}`)
		if w.Code != http.StatusOK {
			t.Fatalf("PATCH status=%d body=%s", w.Code, w.Body)
		}
	})
	t.Run("updates values and preserves other fields", func(t *testing.T) {
		releaseNamespace := "custom-workloads"
		app := &platformv1alpha1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: testEditableName, Namespace: testNamespace, Generation: 4},
			Spec: platformv1alpha1.ApplicationSpec{
				ApplicationDefinitionRef: platformv1alpha1.ApplicationReference{Name: testDefinitionName},
				ClusterRef:               &platformv1alpha1.ApplicationReference{Name: testClusterName},
				ReleaseNamespace:         &releaseNamespace,
			},
			Status: platformv1alpha1.ApplicationStatus{ObservedGeneration: 2},
		}
		definition := validDefinition(testDefinitionName, true)
		definition.Spec.Release.AllowNamespaceOverride = true
		h, c := testHandler(t, definition, app)
		w := patchValues(h, "/api/applications/editable", `{"generation":4,"values":{"replica":{"count":5},"feature":{"enabled":true}}}`)
		if w.Code != http.StatusOK {
			t.Fatalf("PATCH status=%d body=%s", w.Code, w.Body)
		}
		var response platformv1alpha1.Application
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		stored := &platformv1alpha1.Application{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testEditableName}, stored); err != nil {
			t.Fatal(err)
		}
		for _, got := range []*platformv1alpha1.Application{stored, &response} {
			if got.Spec.ApplicationDefinitionRef.Name != testDefinitionName || got.Spec.ClusterRef == nil || got.Spec.ClusterRef.Name != testClusterName || got.Spec.ReleaseNamespace == nil || *got.Spec.ReleaseNamespace != releaseNamespace || got.Status.ObservedGeneration != 2 {
				t.Fatalf("PATCH changed fields outside values: %+v", got)
			}
			if got.Spec.Values == nil || string(got.Spec.Values.Raw) != `{"replica":{"count":5},"feature":{"enabled":true}}` {
				t.Fatalf("PATCH values=%v", got.Spec.Values)
			}
		}
	})

	t.Run("rejects invalid edits and stale generation", func(t *testing.T) {
		app := &platformv1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: testEditableName, Namespace: testNamespace, Generation: 4}, Spec: platformv1alpha1.ApplicationSpec{ApplicationDefinitionRef: platformv1alpha1.ApplicationReference{Name: testDefinitionName}}}
		definition := validDefinition(testDefinitionName, true)
		definition.Spec.ConfigurableValues[0].Minimum = new(int64(1))
		definition.Spec.ConfigurableValues[0].Maximum = new(int64(5))
		h, _ := testHandler(t, definition, app)
		for _, tc := range []struct {
			name string
			body string
			want int
		}{
			{name: "stale", body: `{"generation":3,"values":{"replica":{"count":5}}}`, want: http.StatusConflict},
			{name: "wrong type", body: `{"generation":4,"values":{"replica":{"count":"5"}}}`, want: http.StatusUnprocessableEntity},
			{name: "out of range", body: `{"generation":4,"values":{"replica":{"count":6}}}`, want: http.StatusUnprocessableEntity},
			{name: "required removal", body: `{"generation":4,"values":{"feature":{"enabled":true}}}`, want: http.StatusUnprocessableEntity},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if w := patchValues(h, "/api/applications/editable", tc.body); w.Code != tc.want {
					t.Fatalf("PATCH status=%d, want %d body=%s", w.Code, tc.want, w.Body)
				}
			})
		}
	})

	t.Run("deleting app conflicts and absent app is not found", func(t *testing.T) {
		now := metav1.Now()
		app := &platformv1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "deleting", Namespace: testNamespace, Generation: 1, DeletionTimestamp: &now, Finalizers: []string{"test.finalizer"}}, Spec: platformv1alpha1.ApplicationSpec{ApplicationDefinitionRef: platformv1alpha1.ApplicationReference{Name: testDefinitionName}}}
		h, _ := testHandler(t, validDefinition(testDefinitionName, true), app)
		body := `{"generation":1,"values":{"replica":{"count":1}}}`
		if w := patchValues(h, "/api/applications/deleting", body); w.Code != http.StatusConflict {
			t.Fatalf("deleting app status=%d, want 409 body=%s", w.Code, w.Body)
		}
		if w := patchValues(h, "/api/applications/missing", body); w.Code != http.StatusNotFound {
			t.Fatalf("missing app status=%d, want 404 body=%s", w.Code, w.Body)
		}
		unbound := &platformv1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "unbound", Namespace: testNamespace, Generation: 1}, Spec: platformv1alpha1.ApplicationSpec{ApplicationDefinitionRef: platformv1alpha1.ApplicationReference{Name: "missing"}}}
		h, _ = testHandler(t, unbound)
		if w := patchValues(h, "/api/applications/unbound", body); w.Code != http.StatusNotFound {
			t.Fatalf("missing definition status=%d, want 404 body=%s", w.Code, w.Body)
		}
	})

	t.Run("request security and fixed namespace", func(t *testing.T) {
		outside := &platformv1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "outside", Namespace: testOtherNamespace, Generation: 1}}
		h, c := testHandler(t, outside)
		body := `{"generation":1,"values":{}}`
		if w := patchValues(h, "/api/applications/outside?namespace=other", body); w.Code != http.StatusNotFound {
			t.Fatalf("out-of-scope app status=%d, want 404 body=%s", w.Code, w.Body)
		}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: testOtherNamespace, Name: "outside"}, &platformv1alpha1.Application{}); err != nil {
			t.Fatalf("out-of-scope Application was changed: %v", err)
		}
		for _, tc := range []struct {
			name       string
			body       string
			typeHeader string
			origin     string
			want       int
		}{
			{name: "cross origin", body: body, typeHeader: testJSONContentType, origin: "https://evil.example", want: http.StatusForbidden},
			{name: "unknown field", body: `{"generation":1,"values":{},"namespace":"other"}`, typeHeader: testJSONContentType, want: http.StatusBadRequest},
			{name: "invalid json", body: `{`, typeHeader: testJSONContentType, want: http.StatusBadRequest},
			{name: "unsupported type", body: body, typeHeader: "text/plain", want: http.StatusUnsupportedMediaType},
			{name: "missing values", body: `{"generation":1}`, typeHeader: testJSONContentType, want: http.StatusBadRequest},
		} {
			t.Run(tc.name, func(t *testing.T) {
				r := httptest.NewRequest(http.MethodPatch, "/api/applications/outside", strings.NewReader(tc.body))
				r.Header.Set("Content-Type", tc.typeHeader)
				if tc.origin != "" {
					r.Header.Set("Origin", tc.origin)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != tc.want {
					t.Fatalf("PATCH status=%d, want %d body=%s", w.Code, tc.want, w.Body)
				}
			})
		}
		large := fmt.Sprintf(`{"generation":1,"values":{"large":"%s"}}`, strings.Repeat("x", maxRequestBody))
		if w := patchValues(h, "/api/applications/outside", large); w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized body status=%d, want 413", w.Code)
		}
	})
}

func patchValues(handler http.Handler, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	r.Header.Set("Content-Type", testJSONContentType)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func deleteApplication(handler http.Handler, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodDelete, path, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestCreateReleaseNamespaceOverride(t *testing.T) {
	cluster := &platformv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: testClusterName}}
	t.Run("override disabled", func(t *testing.T) {
		definition := validDefinition(testDefinitionName, true)
		h, c := testHandler(t, definition, cluster)
		w := postJSON(h, `{"name":"reject-app","definitionName":"sample","clusterName":"production","releaseNamespace":"other","values":{"replica":{"count":3}}}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status=%d, want 422 body=%s", w.Code, w.Body)
		}
		assertApplicationAbsent(t, c, "reject-app")
	})
	t.Run("override enabled and listed", func(t *testing.T) {
		definition := validDefinition(testDefinitionName, true)
		definition.Spec.Release.AllowNamespaceOverride = true
		h, c := testHandler(t, definition, cluster)
		w := postJSON(h, `{"name":"override-app","definitionName":"sample","clusterName":"production","releaseNamespace":"custom-ns","values":{"replica":{"count":3}}}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		app := &platformv1alpha1.Application{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "override-app"}, app); err != nil {
			t.Fatal(err)
		}
		if app.Spec.ReleaseNamespace == nil || *app.Spec.ReleaseNamespace != "custom-ns" {
			t.Fatalf("stored releaseNamespace=%v, want custom-ns", app.Spec.ReleaseNamespace)
		}
		list := httptest.NewRecorder()
		h.ServeHTTP(list, httptest.NewRequest(http.MethodGet, testApplicationsPath, nil))
		if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"releaseNamespace":"custom-ns"`) {
			t.Fatalf("GET applications did not return override: status=%d body=%s", list.Code, list.Body)
		}
	})
	for _, value := range []string{"bad_namespace", strings.Repeat("a", 64)} {
		t.Run("invalid "+value, func(t *testing.T) {
			definition := validDefinition(testDefinitionName, true)
			definition.Spec.Release.AllowNamespaceOverride = true
			h, c := testHandler(t, definition, cluster)
			body, err := json.Marshal(map[string]any{
				"name": "invalid-app", "definitionName": testDefinitionName,
				"clusterName": testClusterName, "releaseNamespace": value,
				"values": map[string]any{"replica": map[string]any{"count": 3}},
			})
			if err != nil {
				t.Fatal(err)
			}
			w := postJSON(h, string(body))
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status=%d, want 422 body=%s", w.Code, w.Body)
			}
			assertApplicationAbsent(t, c, "invalid-app")
		})
	}
}

func assertApplicationAbsent(t *testing.T, c client.Client, name string) {
	t.Helper()
	app := &platformv1alpha1.Application{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, app); err == nil {
		t.Fatalf("Application %q unexpectedly exists", name)
	}
}

func TestCreateValidationAndConflicts(t *testing.T) {
	definition := validDefinition(testDefinitionName, true)
	cluster := &platformv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: testClusterName}}
	cases := []struct {
		name   string
		body   string
		setup  []client.Object
		status int
	}{
		{name: "required", body: `{"name":"new-app","definitionName":"sample","clusterName":"production","values":{"feature":{"enabled":true}}}`, status: http.StatusUnprocessableEntity},
		{name: "unknown path", body: `{"name":"new-app","definitionName":"sample","clusterName":"production","values":{"other":true}}`, status: http.StatusUnprocessableEntity},
		{name: "wrong type", body: `{"name":"new-app","definitionName":"sample","clusterName":"production","values":{"replica":{"count":"3"}}}`, status: http.StatusUnprocessableEntity},
		{name: "missing definition", body: `{"name":"new-app","definitionName":"missing","clusterName":"production","values":{"replica":{"count":3}}}`, setup: []client.Object{cluster}, status: http.StatusNotFound},
		{name: "unvalidated definition", body: `{"name":"new-app","definitionName":"sample","clusterName":"production","values":{"replica":{"count":3}}}`, setup: []client.Object{validDefinition(testDefinitionName, false), cluster}, status: http.StatusUnprocessableEntity},
		{name: "missing cluster", body: `{"name":"new-app","definitionName":"sample","clusterName":"missing","values":{"replica":{"count":3}}}`, setup: []client.Object{definition}, status: http.StatusNotFound},
		{name: "duplicate", body: `{"name":"existing","definitionName":"sample","clusterName":"production","values":{"replica":{"count":3}}}`, setup: []client.Object{definition, cluster, &platformv1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: testNamespace}}}, status: http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setup := tc.setup
			if setup == nil {
				setup = []client.Object{definition, cluster}
			}
			h, _ := testHandler(t, setup...)
			if got := postJSON(h, tc.body).Code; got != tc.status {
				t.Fatalf("status=%d, want %d", got, tc.status)
			}
		})
	}
}

func TestCreateRequestSecurityAndNoSecretEndpoint(t *testing.T) {
	h, _ := testHandler(t)
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
		header string
		origin string
		want   int
	}{
		{name: "form post", method: http.MethodPost, path: testApplicationsPath, body: "name=x", header: "application/x-www-form-urlencoded", want: http.StatusUnsupportedMediaType},
		{name: "cross origin", method: http.MethodPost, path: testApplicationsPath, body: `{}`, header: testJSONContentType, origin: "https://evil.example", want: http.StatusForbidden},
		{name: "unknown envelope field", method: http.MethodPost, path: testApplicationsPath, body: `{"name":"x","unknown":true}`, header: testJSONContentType, want: http.StatusBadRequest},
		{name: "no generic secret endpoint", method: http.MethodGet, path: "/api/secrets", want: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.header)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d, want %d body=%s", w.Code, tc.want, w.Body)
			}
		})
	}
	health := httptest.NewRecorder()
	h.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status=%d", health.Code)
	}
}

func postJSON(handler http.Handler, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, testApplicationsPath, strings.NewReader(body))
	r.Header.Set("Content-Type", testJSONContentType)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestActivityListsEventsForObject(t *testing.T) {
	now := time.Now()
	event := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "demo.1", Namespace: testNamespace},
		InvolvedObject: corev1.ObjectReference{Kind: "Application", Name: "demo"},
		Type:           "Normal", Reason: "Ready", Message: "Application release is ready", Count: 2,
		FirstTimestamp: metav1.NewTime(now.Add(-time.Hour)), LastTimestamp: metav1.NewTime(now),
	}
	other := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "other.1", Namespace: testNamespace},
		InvolvedObject: corev1.ObjectReference{Kind: "Application", Name: "other"},
		Type:           "Warning", Reason: "Degraded", Message: "Other application", LastTimestamp: metav1.NewTime(now),
	}
	handler, _ := testHandler(t, event, other)
	request := httptest.NewRequest(http.MethodGet, "/api/activity?kind=Application&name=demo", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/activity status=%d body=%s", response.Code, response.Body)
	}
	var body struct {
		Items []activityEntry `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || body.Items[0].Reason != "Ready" || body.Items[0].Count != 2 {
		t.Fatalf("unexpected activity entries: %#v", body.Items)
	}
}

func TestActivityRejectsUnsupportedKind(t *testing.T) {
	handler, _ := testHandler(t)
	request := httptest.NewRequest(http.MethodGet, "/api/activity?kind=Cluster&name=demo", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("GET /api/activity unsupported kind status=%d, want 400", response.Code)
	}
}

func TestActivityRequiresReadPermissionWhenImpersonating(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := authorizationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	// A fake impersonating client cannot authorize the SelfSubjectAccessReview,
	// so the request must be denied rather than falling back to the dashboard.
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	s := &server{client: c, namespace: testNamespace}
	request := httptest.NewRequest(http.MethodGet, "/api/activity?kind=Application&name=demo", nil)
	request = request.WithContext(withImpersonatingClient(request.Context(), c))
	response := httptest.NewRecorder()
	s.listActivity(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("impersonated activity without permission status=%d, want 403", response.Code)
	}
}

func TestDefinitionListFiltersByImpersonatedPermission(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := authorizationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	var reviewNamespaces []string
	allowReview := func(rules ...authorizationv1.ResourceRule) interceptor.Funcs {
		return interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if review, ok := obj.(*authorizationv1.SelfSubjectRulesReview); ok {
					reviewNamespaces = append(reviewNamespaces, review.Spec.Namespace)
					review.Status.ResourceRules = rules
					return nil
				}
				return c.Create(ctx, obj, opts...)
			},
		}
	}
	count := func(t *testing.T, c client.Client) int {
		t.Helper()
		s := &server{client: c, namespace: testNamespace}
		request := httptest.NewRequest(http.MethodGet, "/api/applicationdefinitions", nil)
		request = request.WithContext(withImpersonatingClient(request.Context(), c))
		response := httptest.NewRecorder()
		s.listDefinitions(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("listDefinitions status=%d body=%s", response.Code, response.Body)
		}
		var body struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return len(body.Items)
	}

	hidden := validDefinition("hidden", true)
	noRules := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hidden).Build()
	if got := count(t, noRules); got != 0 {
		t.Fatalf("impersonated user without rules saw %d definitions, want 0", got)
	}

	allowAll := fake.NewClientBuilder().WithScheme(scheme).WithObjects(validDefinition("visible", true), hidden).
		WithInterceptorFuncs(allowReview(authorizationv1.ResourceRule{Verbs: []string{"get"}, APIGroups: []string{"platform.releaseyard.io"}, Resources: []string{"applicationdefinitions"}})).Build()
	if got := count(t, allowAll); got != 2 {
		t.Fatalf("impersonated user with unrestricted get saw %d definitions, want 2", got)
	}

	scoped := fake.NewClientBuilder().WithScheme(scheme).WithObjects(validDefinition("visible", true), hidden).
		WithInterceptorFuncs(allowReview(authorizationv1.ResourceRule{Verbs: []string{"get"}, APIGroups: []string{"platform.releaseyard.io"}, Resources: []string{"applicationdefinitions"}, ResourceNames: []string{"visible"}})).Build()
	if got := count(t, scoped); got != 1 {
		t.Fatalf("impersonated user with resourceNames saw %d definitions, want 1", got)
	}

	if len(reviewNamespaces) == 0 {
		t.Fatal("expected at least one SelfSubjectRulesReview to be issued")
	}
	for _, namespace := range reviewNamespaces {
		if namespace != metav1.NamespaceDefault {
			t.Fatalf("SelfSubjectRulesReview namespace=%q, want %q", namespace, metav1.NamespaceDefault)
		}
	}
}

func TestTeamApplicationUsesImpersonatedClient(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	const teamNamespace = "releaseyard-team-platform"
	app := &platformv1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "test-2", Namespace: teamNamespace}}
	impersonated := fake.NewClientBuilder().WithScheme(scheme).WithObjects(app).Build()
	// The dashboard ServiceAccount client deliberately does not know the app, so
	// the handler must resolve and read it through the impersonating client.
	saClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	s := &server{client: saClient, namespace: testNamespace}

	ctx := withSession(context.Background(), dashboardSession{authMethod: authMethodOIDC, username: "u"})
	ctx = withTeams(ctx, []string{teamNamespace})
	ctx = withImpersonatingClient(ctx, impersonated)

	resolveRequest := httptest.NewRequest(http.MethodDelete, "/api/applications/test-2", nil).WithContext(ctx)
	namespace, found, err := s.resolveApplicationNamespace(resolveRequest, "test-2")
	if err != nil || !found || namespace != teamNamespace {
		t.Fatalf("resolve namespace=%q found=%v err=%v", namespace, found, err)
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/api/applications/test-2", nil).WithContext(ctx)
	deleteRequest.SetPathValue("name", "test-2")
	response := httptest.NewRecorder()
	s.deleteApplication(response, deleteRequest)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", response.Code, response.Body)
	}
	if err := impersonated.Get(context.Background(), client.ObjectKey{Namespace: teamNamespace, Name: "test-2"}, &platformv1alpha1.Application{}); err == nil {
		t.Fatal("application still present after delete")
	}
}
