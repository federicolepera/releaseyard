package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	"github.com/federicolepera/idp/internal/helmrelease"
	"github.com/federicolepera/idp/internal/releaseclaim"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	appTestNamespace        = "tenant"
	appTestName             = "demo"
	appTestDefName          = "demo-definition"
	appTestCluster          = "demo-cluster"
	appTestReleaseNamespace = "workloads"
	testChartVersion        = "1.2.3"
	testNextChartVersion    = "2.0.0"
)

type appTestPuller struct {
	archive   []byte
	digest    string
	err       error
	calls     int
	afterPull func()
}

func (p *appTestPuller) PullVerified(context.Context, platformv1alpha1.ApplicationDefinitionChart) ([]byte, string, error) {
	p.calls++
	if p.afterPull != nil {
		p.afterPull()
	}
	return append([]byte(nil), p.archive...), p.digest, p.err
}

type appTestHelm struct {
	t              *testing.T
	client         client.Client
	result         helmrelease.Result
	err            error
	apply          []helmrelease.Request
	uninstall      []helmrelease.Request
	check          func(helmrelease.Request)
	afterApply     func()
	afterUninstall func()
}

func (h *appTestHelm) Apply(_ context.Context, request helmrelease.Request) (helmrelease.Result, error) {
	h.apply = append(h.apply, request)
	if h.check != nil {
		h.check(request)
	}
	if h.afterApply != nil {
		h.afterApply()
	}
	result := h.result
	if result.DesiredIdentity == "" {
		result.DesiredIdentity, _ = helmrelease.DesiredIdentity(request.Repository, request.ChartName, request.ChartVersion, request.Digest, request.Values)
	}
	return result, h.err
}

func (h *appTestHelm) Uninstall(_ context.Context, request helmrelease.Request) (helmrelease.Result, error) {
	h.uninstall = append(h.uninstall, request)
	if h.afterUninstall != nil {
		h.afterUninstall()
	}
	return h.result, h.err
}

type appTestClaims struct {
	claim                            releaseclaim.Claim
	identities                       []releaseclaim.Identity
	acquireErr, verifyErr, deleteErr error
	acquires, verifies, deletes      int
	deleted                          bool
}

func (m *appTestClaims) Acquire(_ context.Context, identity releaseclaim.Identity) (releaseclaim.Claim, error) {
	m.acquires++
	m.identities = append(m.identities, identity)
	return m.claim, m.acquireErr
}

func TestApplicationReleaseNamespaceOverridePolicy(t *testing.T) {
	for _, test := range []struct {
		name       string
		allowed    bool
		namespace  *string
		want       string
		wantDenied bool
	}{
		{name: "default namespace", want: appTestReleaseNamespace},
		{name: "override denied", namespace: new("custom"), wantDenied: true},
		{name: "override allowed", allowed: true, namespace: new("custom"), want: "custom"},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := newAppTestEnvironment(t, func(e *appTestEnvironment) {
				e.definition.Spec.Release.AllowNamespaceOverride = test.allowed
				e.app.Spec.ReleaseNamespace = test.namespace
				if err := e.c.Update(context.Background(), e.definition); err != nil {
					t.Fatal(err)
				}
				if err := e.c.Update(context.Background(), e.app); err != nil {
					t.Fatal(err)
				}
			})
			e.reconcile(t)
			got := e.getApp(t)
			if test.wantDenied {
				ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
				if ready == nil || ready.Status != metav1.ConditionFalse || len(e.helm.apply) != 0 || got.Status.Binding != nil {
					t.Fatalf("denied override was not rejected before binding/Helm: status=%#v apply=%d", got.Status, len(e.helm.apply))
				}
				return
			}
			if got.Status.Binding == nil || got.Status.Binding.ReleaseNamespace != test.want || len(e.claims.identities) != 1 || e.claims.identities[0].ReleaseNamespace != test.want || len(e.helm.apply) != 1 || e.helm.apply[0].Namespace != test.want {
				t.Fatalf("effective namespace was not consistently used: binding=%#v claims=%#v Helm=%#v", got.Status.Binding, e.claims.identities, e.helm.apply)
			}
		})
	}
}

func TestApplicationBoundReleaseNamespaceCannotSwitch(t *testing.T) {
	e := newAppTestEnvironment(t, func(e *appTestEnvironment) { e.definition.Spec.Release.AllowNamespaceOverride = true })
	e.reconcile(t)
	previous := e.getApp(t).Status.Applied.DeepCopy()
	app := e.getApp(t)
	app.Spec.ReleaseNamespace = new("other") // Fake client bypasses API immutability admission.
	if err := e.c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	got := e.getApp(t)
	ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	if len(e.helm.apply) != 1 || got.Status.Applied == nil || got.Status.Applied.DesiredInputIdentity != previous.DesiredInputIdentity || ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("post-bind target switch changed release: status=%#v applies=%d", got.Status, len(e.helm.apply))
	}
}

func TestApplicationDeletionUsesStoredReleaseNamespaceAfterDefinitionChange(t *testing.T) {
	e := newAppTestEnvironment(t, func(e *appTestEnvironment) {
		e.definition.Spec.Release.AllowNamespaceOverride = true
		e.app.Spec.ReleaseNamespace = new("custom")
		if err := e.c.Update(context.Background(), e.definition); err != nil {
			t.Fatal(err)
		}
		if err := e.c.Update(context.Background(), e.app); err != nil {
			t.Fatal(err)
		}
	})
	e.reconcile(t)
	if err := e.c.Delete(context.Background(), e.definition); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(context.Background(), e.app); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	if len(e.helm.uninstall) != 1 || e.helm.uninstall[0].Namespace != "custom" || e.helm.uninstall[0].ReleaseName != appTestName {
		t.Fatalf("cleanup did not use bound override namespace: %#v", e.helm.uninstall)
	}
}

func (m *appTestClaims) Verify(context.Context, releaseclaim.Identity, releaseclaim.Claim) error {
	m.verifies++
	if m.deleted {
		return releaseclaim.ErrClaimNotFound
	}
	return m.verifyErr
}
func (m *appTestClaims) Delete(context.Context, releaseclaim.Identity, releaseclaim.Claim) error {
	m.deletes++
	if m.deleted {
		return releaseclaim.ErrClaimNotFound
	}
	if m.deleteErr == nil {
		m.deleted = true
	}
	return m.deleteErr
}

type appTestEnvironment struct {
	c          client.Client
	r          *ApplicationReconciler
	app        *platformv1alpha1.Application
	definition *platformv1alpha1.ApplicationDefinition
	cluster    *platformv1alpha1.Cluster
	puller     *appTestPuller
	helm       *appTestHelm
	claims     *appTestClaims
	request    ctrl.Request
	reader     client.Client
	recorder   *record.FakeRecorder
}

type appCompositeReader struct {
	management client.Reader
	secrets    client.Reader
}

type appUpdateConflictClient struct {
	client.Client
	failFinalizerRemoval bool
}

func (c *appUpdateConflictClient) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	if c.failFinalizerRemoval {
		if app, ok := object.(*platformv1alpha1.Application); ok && !containsString(app.Finalizers, applicationFinalizer) {
			c.failFinalizerRemoval = false
			return apierrors.NewConflict(schema.GroupResource{Group: platformv1alpha1.GroupVersion.Group, Resource: "applications"}, app.Name, errors.New("simulated finalizer update conflict"))
		}
	}
	return c.Client.Update(ctx, object, options...)
}

func (r appCompositeReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, isSecret := object.(*corev1.Secret); isSecret {
		return r.secrets.Get(ctx, key, object, options...)
	}
	return r.management.Get(ctx, key, object, options...)
}

func (r appCompositeReader) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if _, isSecretList := list.(*corev1.SecretList); isSecretList {
		return r.secrets.List(ctx, list, options...)
	}
	return r.management.List(ctx, list, options...)
}

func newAppTestEnvironment(t *testing.T, configure func(*appTestEnvironment)) *appTestEnvironment {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(server.Close)
	archive := []byte("verified archive")
	sum := sha256.Sum256(archive)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	app := &platformv1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: appTestNamespace, Name: appTestName, UID: types.UID("application-uid"), Generation: 7, Finalizers: []string{applicationFinalizer}}, Spec: platformv1alpha1.ApplicationSpec{ApplicationDefinitionRef: platformv1alpha1.ApplicationReference{Name: appTestDefName}, ClusterRef: &platformv1alpha1.ApplicationReference{Name: appTestCluster}, Values: &apiextensionsv1.JSON{Raw: []byte(`{"replicaCount":2}`)}}}
	definition := &platformv1alpha1.ApplicationDefinition{ObjectMeta: metav1.ObjectMeta{Name: appTestDefName, UID: types.UID("definition-uid"), Generation: 3}, Spec: platformv1alpha1.ApplicationDefinitionSpec{Chart: platformv1alpha1.ApplicationDefinitionChart{Repository: "https://charts.example.test", Name: "demo-chart", Version: testChartVersion}, Release: platformv1alpha1.ApplicationDefinitionRelease{Namespace: appTestReleaseNamespace}, ConfigurableValues: []platformv1alpha1.ApplicationDefinitionValue{{Path: "replicaCount", Type: "integer"}}}, Status: platformv1alpha1.ApplicationDefinitionStatus{Digest: digest, ObservedGeneration: 3, Conditions: []metav1.Condition{{Type: "Validated", Status: metav1.ConditionTrue, Reason: "Validated", ObservedGeneration: 3}}}}
	cluster := &platformv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: appTestCluster, UID: types.UID("cluster-uid"), Generation: 5}, Spec: platformv1alpha1.ClusterSpec{KubeconfigSecretRef: platformv1alpha1.KubeconfigSecretReference{Name: testSecretName}}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testManagerNamespace, Name: testSecretName}, Data: map[string][]byte{"kubeconfig": testKubeconfig(server)}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&platformv1alpha1.Application{}, &platformv1alpha1.ApplicationDefinition{}, &platformv1alpha1.Cluster{}).WithObjects(app, definition, cluster).Build()
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	puller := &appTestPuller{archive: archive, digest: digest}
	helm := &appTestHelm{t: t, client: c, result: helmrelease.Result{HelmRevision: 1}}
	claims := &appTestClaims{claim: releaseclaim.Claim{Namespace: appTestReleaseNamespace, Name: releaseclaim.ClaimName(appTestName), UID: types.UID("claim-uid")}}
	e := &appTestEnvironment{c: c, reader: reader, app: app, definition: definition, cluster: cluster, puller: puller, helm: helm, claims: claims, request: ctrl.Request{NamespacedName: types.NamespacedName{Namespace: appTestNamespace, Name: appTestName}}}
	e.recorder = record.NewFakeRecorder(20)
	e.r = &ApplicationReconciler{Client: c, APIReader: appCompositeReader{management: c, secrets: reader}, CredentialsNamespace: testManagerNamespace, TeamNamespacePrefix: appTestNamespace, Scheme: scheme, Puller: puller, Recorder: e.recorder, HelmFactory: func(genericclioptions.RESTClientGetter) applicationHelm { return helm }, ClaimManagerFactory: func(kubernetes.Interface) releaseClaimManager { return claims }}
	if configure != nil {
		configure(e)
	}
	return e
}

func TestReleaseNameDerivesFromTeamNamespace(t *testing.T) {
	r := &ApplicationReconciler{TeamNamespacePrefix: "releaseyard-team-"}
	app := &platformv1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Namespace: "releaseyard-team-platform", Name: "payments"}}
	if got := r.releaseName(app); got != "platform-payments" {
		t.Fatalf("releaseName() = %q, want %q", got, "platform-payments")
	}
}

func (e *appTestEnvironment) reconcile(t *testing.T) {
	t.Helper()
	if _, err := e.r.Reconcile(context.Background(), e.request); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func (e *appTestEnvironment) getApp(t *testing.T) *platformv1alpha1.Application {
	t.Helper()
	var got platformv1alpha1.Application
	if err := e.c.Get(context.Background(), e.request.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func TestEnvironmentModeWithoutMatchingTargetsDoesNotMutate(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	app := e.getApp(t)
	app.Spec.ClusterRef = nil
	app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "production"}
	if err := e.c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	if e.puller.calls != 0 || e.claims.acquires != 0 || e.claims.verifies != 0 || len(e.helm.apply) != 0 || len(e.helm.uninstall) != 0 {
		t.Fatalf("environment-mode reconciliation performed external operations: pull=%d acquire=%d verify=%d apply=%d uninstall=%d", e.puller.calls, e.claims.acquires, e.claims.verifies, len(e.helm.apply), len(e.helm.uninstall))
	}
	got := e.getApp(t)
	ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	workload := meta.FindStatusCondition(got.Status.Conditions, "WorkloadHealthy")
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "NoEnvironmentTargets" {
		t.Fatalf("expected no-target Ready condition, got %#v", ready)
	}
	if workload == nil || workload.Status != metav1.ConditionUnknown || workload.Reason != "NoEnvironmentTargets" {
		t.Fatalf("expected unknown workload condition, got %#v", workload)
	}
}

func TestEnvironmentDuplicateTargetUIDRejectedBeforeMutation(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	app := e.getApp(t)
	app.Spec.ClusterRef = nil
	app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "production", Targets: []platformv1alpha1.ApplicationEnvironmentTarget{
		{Name: "first", UID: types.UID("duplicate")}, {Name: "second", UID: types.UID("duplicate")},
	}}
	if err := e.c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	if e.puller.calls != 0 || e.claims.acquires != 0 || len(e.helm.apply) != 0 {
		t.Fatalf("duplicate UID caused mutations: pull=%d acquire=%d apply=%d", e.puller.calls, e.claims.acquires, len(e.helm.apply))
	}
	got := e.getApp(t)
	if got.Status.EnvironmentBinding != nil || len(got.Status.Instances) != 0 {
		t.Fatalf("invalid snapshot wrote target status: %#v", got.Status)
	}
}

func TestEnvironmentModeDeletionGuard(t *testing.T) {
	tests := []struct {
		name          string
		setBoundState func(*platformv1alpha1.Application)
		wantFinalizer bool
	}{
		{name: "unbound application removes finalizer"},
		{name: "legacy binding retains finalizer", setBoundState: func(app *platformv1alpha1.Application) {
			app.Status.Binding = &platformv1alpha1.ApplicationBinding{ClusterUID: types.UID("legacy-cluster-uid")}
		}, wantFinalizer: true},
		{name: "environment binding without instance binding is safe", setBoundState: func(app *platformv1alpha1.Application) {
			app.Status.EnvironmentBinding = &platformv1alpha1.ApplicationEnvironmentBinding{ApplicationDefinitionUID: types.UID("definition-uid"), ReleaseNamespace: appTestReleaseNamespace}
		}},
		{name: "instance retains finalizer", setBoundState: func(app *platformv1alpha1.Application) {
			app.Status.Instances = []platformv1alpha1.ApplicationInstanceStatus{{ClusterName: "cluster-a", ClusterUID: types.UID("cluster-uid-a")}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newAppTestEnvironment(t, nil)
			app := e.getApp(t)
			app.Spec.ClusterRef = nil
			app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "production", Targets: []platformv1alpha1.ApplicationEnvironmentTarget{{Name: "cluster-a", UID: "cluster-uid-a"}}}
			if err := e.c.Update(context.Background(), app); err != nil {
				t.Fatal(err)
			}
			if tt.setBoundState != nil {
				app = e.getApp(t)
				tt.setBoundState(app)
				if err := e.c.Status().Update(context.Background(), app); err != nil {
					t.Fatal(err)
				}
			}
			app = e.getApp(t)
			if err := e.c.Delete(context.Background(), app); err != nil {
				t.Fatal(err)
			}
			if _, err := e.r.Reconcile(context.Background(), e.request); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			if tt.wantFinalizer {
				remaining := e.getApp(t)
				if !slices.Contains(remaining.Finalizers, applicationFinalizer) {
					t.Fatal("bound environment Application lost its finalizer")
				}
			}
			if e.puller.calls != 0 || e.claims.acquires != 0 || e.claims.verifies != 0 || e.claims.deletes != 0 || len(e.helm.apply) != 0 || len(e.helm.uninstall) != 0 {
				t.Fatalf("environment deletion performed remote operations: pull=%d acquire=%d verify=%d delete=%d apply=%d uninstall=%d", e.puller.calls, e.claims.acquires, e.claims.verifies, e.claims.deletes, len(e.helm.apply), len(e.helm.uninstall))
			}
		})
	}
}

func TestApplicationSuccessfulInstallPersistsOwnershipBeforeApply(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	app := e.getApp(t)
	app.Finalizers = nil
	if err := e.c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	result, err := e.r.Reconcile(context.Background(), e.request)
	if err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("initial finalizer reconcile: result=%#v err=%v", result, err)
	}
	if e.puller.calls != 0 || e.claims.acquires != 0 || len(e.helm.apply) != 0 {
		t.Fatal("initial finalizer reconcile performed external operations")
	}
	e.helm.check = func(request helmrelease.Request) {
		got := e.getApp(t)
		if got.Status.Binding == nil || got.Status.Binding.RemoteClaimUID != e.claims.claim.UID || len(got.Status.ChartPins) != 1 || got.Status.LastAttemptedIdentity == "" {
			t.Fatalf("binding, chart pin, and attempt were not persisted before Helm Apply: %#v", got.Status)
		}
		if len(e.helm.apply) == 1 && request.PreviousAttempt {
			t.Fatal("first attempt unexpectedly marked as previous")
		}
		if len(e.helm.apply) > 1 && !request.PreviousAttempt {
			t.Fatal("repeat attempt was not marked as previous")
		}
	}
	e.reconcile(t) // first reconciliation performs the verified install
	got := e.getApp(t)
	if got.Status.Applied == nil || got.Status.Applied.ApplicationGeneration != e.app.Generation || got.Status.Applied.ApplicationDefinitionGeneration != e.definition.Generation {
		t.Fatalf("Applied identity missing or stale: %#v", got.Status.Applied)
	}
	if ready := meta.FindStatusCondition(got.Status.Conditions, "Ready"); ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition: %#v", ready)
	}
	if e.claims.acquires != 1 || len(e.helm.apply) != 1 {
		t.Fatalf("unexpected first reconcile calls: acquire=%d apply=%d", e.claims.acquires, len(e.helm.apply))
	}
	e.reconcile(t)
	if len(e.helm.apply) != 2 || !e.helm.apply[1].PreviousAttempt || e.helm.apply[0].Digest != e.helm.apply[1].Digest || e.claims.acquires != 1 {
		t.Fatalf("repeat reconcile did not preserve ownership/attempt identity: %#v", e.helm.apply)
	}
}

func drainRecorderEvents(recorder *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case event := <-recorder.Events:
			events = append(events, event)
		default:
			return events
		}
	}
}

func TestApplicationReconcileEmitsReadyActivityEvent(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	e.reconcile(t)
	events := drainRecorderEvents(e.recorder)
	found := false
	for _, event := range events {
		if strings.HasPrefix(event, "Normal Ready ") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a Normal Ready activity event, got %v", events)
	}
}

func TestApplicationReconcileEmitsWarningActivityEventOnFailure(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	app := e.getApp(t)
	app.Spec.ClusterRef = nil
	app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "production"}
	if err := e.c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	events := drainRecorderEvents(e.recorder)
	found := false
	for _, event := range events {
		if strings.HasPrefix(event, "Warning ") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a Warning activity event, got %v", events)
	}
}

func TestApplicationResourceInventoryRefreshesAfterEverySuccessfulApply(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	e.helm.result.Manifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: rendered\n"
	calls := 0
	e.r.ResourceCollector = func(_ context.Context, cfg *rest.Config, manifest, namespace string) ([]platformv1alpha1.ApplicationResource, error) {
		calls++
		if cfg == nil || manifest != e.helm.result.Manifest || namespace != appTestReleaseNamespace {
			t.Fatalf("collector inputs cfg=%v manifest=%q namespace=%q", cfg != nil, manifest, namespace)
		}
		return []platformv1alpha1.ApplicationResource{{APIVersion: "v1", Kind: "ConfigMap", Namespace: namespace, Name: "rendered", UID: types.UID(fmt.Sprintf("inventory-uid-%d", calls))}}, nil
	}
	e.reconcile(t)
	first := e.getApp(t)
	condition := meta.FindStatusCondition(first.Status.Conditions, "InventoryReady")
	if calls != 1 || len(first.Status.Resources) != 1 || first.Status.Resources[0].Name != "rendered" || condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != first.Generation {
		t.Fatalf("first inventory refresh failed: calls=%d status=%#v", calls, first.Status)
	}
	e.reconcile(t)
	second := e.getApp(t)
	if calls != 2 || len(second.Status.Resources) != 1 || second.Status.Resources[0].UID != types.UID("inventory-uid-2") {
		t.Fatalf("unchanged successful Apply did not refresh inventory: calls=%d resources=%#v", calls, second.Status.Resources)
	}
}

func TestApplicationInventoryFailurePreservesPreviousResourcesAndSuccessfulReady(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	previous := []platformv1alpha1.ApplicationResource{{APIVersion: "v1", Kind: "Secret", Namespace: appTestReleaseNamespace, Name: "previous", UID: types.UID("previous-uid")}}
	e.app.Status.Resources = previous
	observedAt := metav1.NewTime(time.Unix(123, 0))
	e.app.Status.WorkloadObservedAt = &observedAt
	meta.SetStatusCondition(&e.app.Status.Conditions, metav1.Condition{Type: "WorkloadHealthy", Status: metav1.ConditionTrue, ObservedGeneration: e.app.Generation, Reason: applicationWorkloadHealthy, Message: "All covered workloads are healthy", LastTransitionTime: observedAt})
	if err := e.c.Status().Update(context.Background(), e.app); err != nil {
		t.Fatal(err)
	}
	e.r.ResourceCollector = func(context.Context, *rest.Config, string, string) ([]platformv1alpha1.ApplicationResource, error) {
		return nil, errors.New("sensitive remote response must not reach status")
	}
	e.reconcile(t)
	got := e.getApp(t)
	ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	inventoryReady := meta.FindStatusCondition(got.Status.Conditions, "InventoryReady")
	workloadHealthy := meta.FindStatusCondition(got.Status.Conditions, "WorkloadHealthy")
	if got.Status.Applied == nil || len(got.Status.Resources) != 1 || got.Status.Resources[0] != previous[0] {
		t.Fatalf("inventory failure changed Applied or prior resources: %#v", got.Status)
	}
	if ready == nil || ready.Status != metav1.ConditionTrue || inventoryReady == nil || inventoryReady.Status != metav1.ConditionFalse || inventoryReady.ObservedGeneration != got.Generation || strings.Contains(inventoryReady.Message, "sensitive") {
		t.Fatalf("unexpected conditions after inventory failure: %#v", got.Status.Conditions)
	}
	if got.Status.WorkloadObservedAt == nil || !got.Status.WorkloadObservedAt.Equal(&observedAt) || workloadHealthy == nil || workloadHealthy.Status != metav1.ConditionUnknown || workloadHealthy.Reason != "ObservationUnavailable" || workloadHealthy.ObservedGeneration != got.Generation {
		t.Fatalf("workload observation was not safely invalidated while preserving its timestamp: %#v", got.Status)
	}
}

func TestApplicationInvalidInputsDoNotCallExternalAdapters(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*appTestEnvironment)
	}{
		{name: "unvalidated definition", mutate: func(e *appTestEnvironment) {
			e.definition.Status.ObservedGeneration--
			if err := e.c.Status().Update(context.Background(), e.definition); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "invalid values", mutate: func(e *appTestEnvironment) {
			e.app.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(`{"notConfigurable":true}`)}
			if err := e.c.Update(context.Background(), e.app); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newAppTestEnvironment(t, tt.mutate)
			e.reconcile(t)
			if e.puller.calls != 0 || e.claims.acquires != 0 || len(e.helm.apply) != 0 {
				t.Fatalf("external calls pull=%d acquire=%d apply=%d", e.puller.calls, e.claims.acquires, len(e.helm.apply))
			}
		})
	}
}

func TestApplicationMissingCredentialsDoesNotPull(t *testing.T) {
	e := newAppTestEnvironment(t, func(e *appTestEnvironment) {
		e.r.APIReader = appCompositeReader{management: e.c, secrets: fake.NewClientBuilder().WithScheme(e.r.Scheme).Build()}
	})
	e.reconcile(t)
	if e.puller.calls != 0 || e.claims.acquires != 0 || len(e.helm.apply) != 0 {
		t.Fatalf("external calls pull=%d acquire=%d apply=%d", e.puller.calls, e.claims.acquires, len(e.helm.apply))
	}
}

func TestApplicationValueChangesUsePreviousAttemptAndApplyOnSuccess(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	e.reconcile(t)
	first := e.getApp(t).Status.Applied.DeepCopy()
	app := e.getApp(t)
	app.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(`{"replicaCount":4}`)}
	if err := e.c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	e.helm.err = errors.New("remote unavailable")
	e.reconcile(t)
	failed := e.getApp(t)
	if failed.Status.Applied == nil || failed.Status.Applied.DesiredInputIdentity != first.DesiredInputIdentity || !e.helm.apply[len(e.helm.apply)-1].PreviousAttempt {
		t.Fatalf("failed operation overwrote Applied or missed previous attempt: %#v", failed.Status.Applied)
	}
	e.helm.err = nil
	e.reconcile(t)
	updated := e.getApp(t)
	if updated.Status.Applied == nil || updated.Status.Applied.DesiredInputIdentity == first.DesiredInputIdentity || !e.helm.apply[len(e.helm.apply)-1].PreviousAttempt {
		t.Fatalf("successful retry did not record new identity: %#v", updated.Status.Applied)
	}
}

func TestApplicationDigestMismatchDoesNotMutateRelease(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	e.reconcile(t)
	previous := e.getApp(t).Status.Applied.DeepCopy()
	e.puller.digest = "sha256:" + string(make([]byte, 64))
	e.reconcile(t)
	got := e.getApp(t)
	if got.Status.Applied == nil || got.Status.Applied.DesiredInputIdentity != previous.DesiredInputIdentity || len(e.helm.apply) != 1 {
		t.Fatalf("digest mismatch changed applied state or called Helm: %#v, calls=%d", got.Status.Applied, len(e.helm.apply))
	}
}

func TestApplicationPinnedChartCoordinateRejectsNewDigest(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	e.reconcile(t)
	previous := e.getApp(t).Status.Applied.DeepCopy()
	changedArchive := []byte("different verified archive")
	changedSum := sha256.Sum256(changedArchive)
	changedDigest := "sha256:" + hex.EncodeToString(changedSum[:])
	e.puller.archive, e.puller.digest = changedArchive, changedDigest
	e.definition.Status.Digest = changedDigest
	e.definition.Status.ObservedGeneration = e.definition.Generation
	if err := e.c.Status().Update(context.Background(), e.definition); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	got := e.getApp(t)
	if got.Status.Applied == nil || got.Status.Applied.DesiredInputIdentity != previous.DesiredInputIdentity || len(e.helm.apply) != 1 {
		t.Fatalf("new bytes for a pinned chart coordinate changed Applied or invoked Helm: %#v, calls=%d", got.Status.Applied, len(e.helm.apply))
	}
}

func TestApplicationDifferentRecordedAttemptRequiresRecoveryMode(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	e.app.Status.LastAttemptedIdentity = "older-input-identity"
	if err := e.c.Status().Update(context.Background(), e.app); err != nil {
		t.Fatal(err)
	}
	e.helm.check = func(request helmrelease.Request) {
		if len(e.helm.apply) == 1 && !request.PreviousAttempt {
			t.Fatal("request with a prior different identity was not marked PreviousAttempt")
		}
	}
	e.reconcile(t)
	if len(e.helm.apply) != 1 {
		t.Fatalf("Apply calls = %d, want 1", len(e.helm.apply))
	}
}

func TestApplicationAcquireFailureDoesNotRecordAttempt(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	e.claims.acquireErr = errors.New("ownership collision")
	e.reconcile(t)
	if got := e.getApp(t); got.Status.LastAttemptedIdentity != "" {
		t.Fatalf("claim acquisition failure recorded attempted identity %q", got.Status.LastAttemptedIdentity)
	}
	if len(e.helm.apply) != 0 {
		t.Fatalf("claim acquisition failure called Helm %d times", len(e.helm.apply))
	}
	e.claims.acquireErr = nil
	e.reconcile(t)
	if len(e.helm.apply) != 1 || e.helm.apply[0].PreviousAttempt {
		t.Fatalf("first actual Helm call after claim recovery = %#v, want PreviousAttempt=false", e.helm.apply)
	}
	e.reconcile(t)
	if len(e.helm.apply) != 2 || !e.helm.apply[1].PreviousAttempt {
		t.Fatalf("retry after a real Helm action = %#v, want PreviousAttempt=true", e.helm.apply)
	}
}

func TestApplicationAuthoritativeStatusChangeDuringPullStopsBeforeClaim(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	management := fake.NewClientBuilder().WithScheme(e.r.Scheme).WithStatusSubresource(&platformv1alpha1.Application{}).WithObjects(e.app.DeepCopy(), e.definition.DeepCopy(), e.cluster.DeepCopy()).Build()
	e.r.APIReader = appCompositeReader{management: management, secrets: e.reader}
	e.puller.afterPull = func() {
		var current platformv1alpha1.Application
		if err := management.Get(context.Background(), e.request.NamespacedName, &current); err != nil {
			t.Fatal(err)
		}
		current.Status.LastAttemptedIdentity = "authoritative-new-attempt"
		current.Status.ChartPins = append(current.Status.ChartPins, platformv1alpha1.ApplicationChartIdentity{Repository: "https://authority.example.test", Name: "authoritative", Version: "1.0.0", Digest: e.puller.digest})
		if err := management.Status().Update(context.Background(), &current); err != nil {
			t.Fatal(err)
		}
	}
	e.reconcile(t)
	var authoritative platformv1alpha1.Application
	if err := management.Get(context.Background(), e.request.NamespacedName, &authoritative); err != nil {
		t.Fatal(err)
	}
	if authoritative.Status.LastAttemptedIdentity != "authoritative-new-attempt" || len(authoritative.Status.ChartPins) != 1 {
		t.Fatalf("authoritative same-generation status was overwritten: %#v", authoritative.Status)
	}
	if e.claims.acquires != 0 || len(e.helm.apply) != 0 {
		t.Fatalf("stale status snapshot proceeded to external actions: acquire=%d Apply=%d", e.claims.acquires, len(e.helm.apply))
	}
}

func TestApplicationAuthoritativeGenerationDuringPullStopsBeforeClaim(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	management := fake.NewClientBuilder().WithScheme(e.r.Scheme).WithObjects(e.app.DeepCopy(), e.definition.DeepCopy(), e.cluster.DeepCopy()).Build()
	e.r.APIReader = appCompositeReader{management: management, secrets: e.reader}
	e.puller.afterPull = func() {
		var current platformv1alpha1.Application
		if err := management.Get(context.Background(), e.request.NamespacedName, &current); err != nil {
			t.Fatal(err)
		}
		current.Generation++
		if err := management.Update(context.Background(), &current); err != nil {
			t.Fatal(err)
		}
	}
	e.reconcile(t)
	if e.claims.acquires != 0 || len(e.helm.apply) != 0 {
		t.Fatalf("authoritative generation change proceeded to external actions: acquire=%d Apply=%d", e.claims.acquires, len(e.helm.apply))
	}
}

func TestApplicationDeletionUsesAuthoritativeBindingInsteadOfStaleCache(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	e.reconcile(t)
	management := fake.NewClientBuilder().WithScheme(e.r.Scheme).WithObjects(e.getApp(t), e.definition.DeepCopy(), e.cluster.DeepCopy()).Build()
	e.r.APIReader = appCompositeReader{management: management, secrets: e.reader}
	e.claims.verifyErr = releaseclaim.ErrOwnershipConflict
	app := e.getApp(t)
	app.Status.Binding = nil
	if err := e.c.Status().Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(context.Background(), e.getApp(t)); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	got := e.getApp(t)
	if !containsString(got.Finalizers, applicationFinalizer) {
		t.Fatal("stale deletion snapshot without binding removed finalizer")
	}
	if e.claims.verifies == 0 || len(e.helm.uninstall) != 0 || e.claims.deletes != 0 {
		t.Fatal("authoritative binding was not verified before blocked cleanup")
	}
}

func TestApplicationCleanupCheckpointAllowsFinalizerRetryWithoutClaim(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	e.reconcile(t)
	if err := e.c.Delete(context.Background(), e.getApp(t)); err != nil {
		t.Fatal(err)
	}
	conflictingClient := &appUpdateConflictClient{Client: e.c, failFinalizerRemoval: true}
	e.r.Client = conflictingClient
	if _, err := e.r.Reconcile(context.Background(), e.request); err == nil {
		t.Fatal("expected simulated finalizer update conflict")
	}
	checkpoint := e.getApp(t)
	if !applicationCleanupComplete(t, checkpoint) {
		t.Fatal("successful Uninstall and claim Delete did not persist CleanupComplete checkpoint")
	}
	if !e.claims.deleted || len(e.helm.uninstall) != 1 || e.claims.deletes != 1 {
		t.Fatal("first cleanup did not uninstall and delete the ownership claim")
	}
	verifiesBeforeRetry := e.claims.verifies
	e.reconcile(t)
	if e.claims.acquires != 1 || e.claims.verifies != verifiesBeforeRetry || e.claims.deletes != 2 || len(e.helm.uninstall) != 1 {
		t.Fatalf("checkpoint retry touched remote ownership/release: acquire=%d verify=%d delete=%d uninstall=%d", e.claims.acquires, e.claims.verifies, e.claims.deletes, len(e.helm.uninstall))
	}
	if err := e.c.Get(context.Background(), e.request.NamespacedName, &platformv1alpha1.Application{}); err == nil {
		t.Fatal("checkpoint retry did not remove the finalizer")
	}
}

func TestApplicationDeletionWithoutCheckpointBlocksOnMissingOrForeignClaim(t *testing.T) {
	for _, test := range []struct {
		name      string
		verifyErr error
	}{{name: "missing", verifyErr: releaseclaim.ErrClaimNotFound}, {name: "foreign replacement", verifyErr: releaseclaim.ErrOwnershipConflict}} {
		t.Run(test.name, func(t *testing.T) {
			e := newAppTestEnvironment(t, nil)
			e.reconcile(t)
			e.claims.verifyErr = test.verifyErr
			if err := e.c.Delete(context.Background(), e.getApp(t)); err != nil {
				t.Fatal(err)
			}
			e.reconcile(t)
			if !containsString(e.getApp(t).Finalizers, applicationFinalizer) || len(e.helm.uninstall) != 0 || e.claims.deletes != 0 {
				t.Fatal("missing/foreign claim without cleanup checkpoint did not block deletion safely")
			}
		})
	}
}

func applicationCleanupComplete(t *testing.T, app *platformv1alpha1.Application) bool {
	t.Helper()
	condition := meta.FindStatusCondition(app.Status.Conditions, "CleanupComplete")
	return condition != nil && condition.Status == metav1.ConditionTrue
}

func TestApplicationMissingBoundClaimDoesNotReacquire(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	e.reconcile(t)
	e.claims.verifyErr = errors.New("claim missing")
	e.reconcile(t)
	if e.claims.acquires != 1 || len(e.helm.apply) != 1 {
		t.Fatalf("missing claim reacquired or mutated release: acquire=%d apply=%d", e.claims.acquires, len(e.helm.apply))
	}
}

func TestApplicationDeletionUsesBindingWithoutDefinitionAndDeletesClaimAfterUninstall(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	e.reconcile(t)
	if err := e.c.Delete(context.Background(), e.definition); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(context.Background(), e.app); err != nil {
		t.Fatal(err)
	}
	e.helm.afterUninstall = func() {
		if e.claims.deletes != 0 {
			t.Fatal("claim deleted before Helm uninstall completed")
		}
	}
	e.reconcile(t)
	if len(e.helm.uninstall) != 1 || e.claims.verifies < 1 || e.claims.deletes != 1 {
		t.Fatalf("cleanup calls uninstall=%d verify=%d delete=%d", len(e.helm.uninstall), e.claims.verifies, e.claims.deletes)
	}
	if e.helm.uninstall[0].ClaimUID != string(e.claims.claim.UID) {
		t.Fatalf("uninstall did not use bound claim UID: %#v", e.helm.uninstall[0])
	}
	var got platformv1alpha1.Application
	if err := e.c.Get(context.Background(), e.request.NamespacedName, &got); err == nil {
		t.Fatal("Application remained after successful cleanup")
	}
}

func TestApplicationDeletionFailuresRetainFinalizerAndCanRecover(t *testing.T) {
	t.Run("credentials", func(t *testing.T) {
		e := newAppTestEnvironment(t, nil)
		e.reconcile(t)
		if err := e.c.Delete(context.Background(), e.app); err != nil {
			t.Fatal(err)
		}
		secret := &corev1.Secret{}
		key := types.NamespacedName{Namespace: testManagerNamespace, Name: testSecretName}
		if err := e.reader.Get(context.Background(), key, secret); err != nil {
			t.Fatal(err)
		}
		validKubeconfig := append([]byte(nil), secret.Data["kubeconfig"]...)
		secret.Data["kubeconfig"] = nil
		if err := e.reader.Update(context.Background(), secret); err != nil {
			t.Fatal(err)
		}
		e.reconcile(t)
		if !containsString(e.getApp(t).Finalizers, applicationFinalizer) || e.claims.deletes != 0 || len(e.helm.uninstall) != 0 {
			t.Fatal("missing credentials did not retain finalizer safely")
		}
		secret.Data["kubeconfig"] = validKubeconfig
		if err := e.reader.Update(context.Background(), secret); err != nil {
			t.Fatal(err)
		}
		e.reconcile(t)
		if e.claims.deletes != 1 {
			t.Fatalf("cleanup recovery deletes=%d, want 1", e.claims.deletes)
		}
	})
	t.Run("verification and uninstall", func(t *testing.T) {
		e := newAppTestEnvironment(t, nil)
		e.reconcile(t)
		if err := e.c.Delete(context.Background(), e.app); err != nil {
			t.Fatal(err)
		}
		e.claims.verifyErr = errors.New("claim UID mismatch")
		e.reconcile(t)
		if !containsString(e.getApp(t).Finalizers, applicationFinalizer) || e.claims.deletes != 0 || len(e.helm.uninstall) != 0 {
			t.Fatal("claim mismatch did not block cleanup")
		}
		e.claims.verifyErr = nil
		e.helm.err = errors.New("uninstall failed")
		e.reconcile(t)
		if !containsString(e.getApp(t).Finalizers, applicationFinalizer) || e.claims.deletes != 0 {
			t.Fatal("uninstall failure did not preserve finalizer/claim")
		}
		e.helm.err = nil
		e.reconcile(t)
		if e.claims.deletes != 1 {
			t.Fatalf("recovered cleanup deletes=%d, want 1", e.claims.deletes)
		}
	})
}

func TestApplicationBoundClusterReplacementBlocksApply(t *testing.T) {
	t.Run("UID replacement", func(t *testing.T) {
		e := newAppTestEnvironment(t, nil)
		e.reconcile(t)
		previous := e.getApp(t).Status.Applied.DeepCopy()
		if err := e.c.Delete(context.Background(), e.cluster); err != nil {
			t.Fatal(err)
		}
		replacement := e.cluster.DeepCopy()
		replacement.ResourceVersion = ""
		replacement.UID = types.UID("replacement-cluster-uid")
		if err := e.c.Create(context.Background(), replacement); err != nil {
			t.Fatal(err)
		}
		e.reconcile(t)
		got := e.getApp(t)
		if len(e.helm.apply) != 1 || got.Status.Applied == nil || got.Status.Applied.DesiredInputIdentity != previous.DesiredInputIdentity {
			t.Fatal("replacement Cluster was allowed to mutate the release")
		}
	})
	t.Run("API target change", func(t *testing.T) {
		e := newAppTestEnvironment(t, nil)
		e.reconcile(t)
		previous := e.getApp(t).Status.Applied.DeepCopy()
		secret := &corev1.Secret{}
		key := types.NamespacedName{Namespace: testManagerNamespace, Name: testSecretName}
		if err := e.reader.Get(context.Background(), key, secret); err != nil {
			t.Fatal(err)
		}
		secret.Data["kubeconfig"] = []byte(strings.Replace(string(secret.Data["kubeconfig"]), "server: https://", "server: https://changed.", 1))
		if err := e.reader.Update(context.Background(), secret); err != nil {
			t.Fatal(err)
		}
		e.reconcile(t)
		got := e.getApp(t)
		if len(e.helm.apply) != 1 || got.Status.Applied == nil || got.Status.Applied.DesiredInputIdentity != previous.DesiredInputIdentity {
			t.Fatal("changed API target was allowed to mutate the release")
		}
	})
	t.Run("TLS trust change", func(t *testing.T) {
		e := newAppTestEnvironment(t, nil)
		e.reconcile(t)
		secret := &corev1.Secret{}
		key := types.NamespacedName{Namespace: testManagerNamespace, Name: testSecretName}
		if err := e.reader.Get(context.Background(), key, secret); err != nil {
			t.Fatal(err)
		}
		secret.Data["kubeconfig"] = []byte(strings.Replace(string(secret.Data["kubeconfig"]), caDataFromConfig(secret.Data["kubeconfig"]), base64.StdEncoding.EncodeToString(makeTestCA(t)), 1))
		if err := e.reader.Update(context.Background(), secret); err != nil {
			t.Fatal(err)
		}
		e.reconcile(t)
		if len(e.helm.apply) != 1 {
			t.Fatal("changed TLS trust material was allowed to mutate the release")
		}
	})
}

func TestApplicationGenerationChangesDuringRemoteWorkDoNotApplyStaleSpec(t *testing.T) {
	t.Run("during pull", func(t *testing.T) {
		e := newAppTestEnvironment(t, nil)
		e.puller.afterPull = func() { bumpApplicationGeneration(t, e) }
		e.reconcile(t)
		if e.claims.acquires != 0 || len(e.helm.apply) != 0 {
			t.Fatalf("stale pull acquired claim/applied: acquire=%d apply=%d", e.claims.acquires, len(e.helm.apply))
		}
	})
	t.Run("during Helm operation", func(t *testing.T) {
		e := newAppTestEnvironment(t, nil)
		e.helm.afterApply = func() {
			bumpApplicationGeneration(t, e)
			current := e.getApp(t)
			current.Status.LastAttemptedIdentity = "concurrent-new-input"
			current.Status.ChartPins = append(current.Status.ChartPins, platformv1alpha1.ApplicationChartIdentity{Repository: "https://other.example.test", Name: "other", Version: "1.0.0", Digest: e.puller.digest})
			if err := e.c.Status().Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
		}
		e.reconcile(t)
		got := e.getApp(t)
		if got.Status.Applied == nil || got.Status.Applied.ApplicationGeneration != 7 || got.Status.Applied.HelmRevision != 1 || len(e.helm.apply) != 1 {
			t.Fatalf("stale operation did not retain the successful old input/revision: applied=%#v calls=%d", got.Status.Applied, len(e.helm.apply))
		}
		if got.Status.LastAttemptedIdentity != "concurrent-new-input" || len(got.Status.ChartPins) != 2 {
			t.Fatal("stale operation overwrote concurrent attempt/pin state")
		}
		ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
		if ready == nil || ready.Status != metav1.ConditionFalse || ready.ObservedGeneration != got.Generation {
			t.Fatalf("stale operation should report Ready=False for current generation %d, got %#v", got.Generation, ready)
		}
	})
}

func TestApplicationDefinitionChartChangePinsNewVersion(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	e.reconcile(t)
	definition := e.definition.DeepCopy()
	definition.Generation++
	definition.Spec.Chart.Version = testNextChartVersion
	if err := e.c.Update(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	definition.Status.ObservedGeneration = definition.Generation
	definition.Status.Conditions[0].ObservedGeneration = definition.Generation
	if err := e.c.Status().Update(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	e.definition = definition
	e.reconcile(t)
	got := e.getApp(t)
	if len(e.helm.apply) != 2 || got.Status.Applied == nil || got.Status.Applied.Chart.Version != testNextChartVersion || len(got.Status.ChartPins) != 2 {
		t.Fatalf("changed chart version was not applied and pinned: apply=%d applied=%#v pins=%#v", len(e.helm.apply), got.Status.Applied, got.Status.ChartPins)
	}
	definition = definition.DeepCopy()
	definition.Generation++
	definition.Spec.Chart.Version = testChartVersion
	changedArchive := []byte("different digest for original coordinate")
	sum := sha256.Sum256(changedArchive)
	definition.Status.Digest = "sha256:" + hex.EncodeToString(sum[:])
	definition.Status.ObservedGeneration = definition.Generation
	definition.Status.Conditions[0].ObservedGeneration = definition.Generation
	if err := e.c.Update(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Status().Update(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	e.puller.archive, e.puller.digest = changedArchive, definition.Status.Digest
	e.reconcile(t)
	got = e.getApp(t)
	if len(e.helm.apply) != 2 || got.Status.Applied.Chart.Version != testNextChartVersion {
		t.Fatal("changed digest for a previously pinned coordinate was applied")
	}
}

func TestApplicationDefinitionAndClusterEventsMapToReferencingApplication(t *testing.T) {
	e := newAppTestEnvironment(t, nil)
	definitionRequests := e.r.mapDefinition(context.Background(), e.definition)
	if len(definitionRequests) != 1 || definitionRequests[0].NamespacedName != e.request.NamespacedName {
		t.Fatalf("definition event mapped to %#v, want %s", definitionRequests, e.request.NamespacedName)
	}
	clusterRequests := e.r.mapCluster(context.Background(), e.cluster)
	if len(clusterRequests) != 1 || clusterRequests[0].NamespacedName != e.request.NamespacedName {
		t.Fatalf("cluster event mapped to %#v, want %s", clusterRequests, e.request.NamespacedName)
	}
}

func bumpApplicationGeneration(t *testing.T, e *appTestEnvironment) {
	t.Helper()
	app := e.getApp(t)
	oldGeneration := app.Generation
	app.Generation++
	app.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(`{"replicaCount":9}`)}
	if err := e.c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if current := e.getApp(t); current.Generation == oldGeneration {
		t.Fatal("fake client did not retain the generation change")
	}
}
