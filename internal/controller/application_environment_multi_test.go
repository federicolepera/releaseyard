package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	"github.com/federicolepera/idp/internal/helmrelease"
	"github.com/federicolepera/idp/internal/releaseclaim"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type environmentMultiHelm struct {
	t           *testing.T
	state       map[string]environmentMultiHelmState
	requests    []environmentMultiHelmRequest
	beforeApply func(string, helmrelease.Request)
}

type environmentMultiHelmState struct {
	desired  string
	revision int
}

type environmentMultiHelmRequest struct {
	host    string
	request helmrelease.Request
}

type environmentHostHelm struct {
	parent *environmentMultiHelm
	host   string
}

func (h *environmentHostHelm) Apply(_ context.Context, request helmrelease.Request) (helmrelease.Result, error) {
	if h.parent.beforeApply != nil {
		h.parent.beforeApply(h.host, request)
	}
	identity, err := helmrelease.DesiredIdentity(request.Repository, request.ChartName, request.ChartVersion, request.Digest, request.Values)
	if err != nil {
		return helmrelease.Result{}, err
	}
	state := h.parent.state[h.host]
	if state.desired != identity {
		state.desired = identity
		state.revision++
	}
	h.parent.state[h.host] = state
	h.parent.requests = append(h.parent.requests, environmentMultiHelmRequest{host: h.host, request: request})
	return helmrelease.Result{DesiredIdentity: identity, HelmRevision: state.revision, Manifest: fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: result-%d\n", state.revision)}, nil
}

func (h *environmentHostHelm) Uninstall(context.Context, helmrelease.Request) (helmrelease.Result, error) {
	return helmrelease.Result{}, nil
}

type environmentMultiClaims struct {
	claims   map[types.UID]releaseclaim.Claim
	acquired []releaseclaim.Identity
	verified []releaseclaim.Identity
}

func newEnvironmentMultiClaims() *environmentMultiClaims {
	return &environmentMultiClaims{claims: make(map[types.UID]releaseclaim.Claim)}
}

func (m *environmentMultiClaims) Acquire(_ context.Context, identity releaseclaim.Identity) (releaseclaim.Claim, error) {
	claim := releaseclaim.Claim{Namespace: identity.ReleaseNamespace, Name: releaseclaim.ClaimName(identity.ReleaseName), UID: types.UID("claim-" + string(identity.ClusterUID))}
	m.claims[identity.ClusterUID] = claim
	m.acquired = append(m.acquired, identity)
	return claim, nil
}

func (m *environmentMultiClaims) Verify(_ context.Context, identity releaseclaim.Identity, claim releaseclaim.Claim) error {
	want, ok := m.claims[identity.ClusterUID]
	if !ok || want != claim {
		return releaseclaim.ErrOwnershipConflict
	}
	m.verified = append(m.verified, identity)
	return nil
}

func (m *environmentMultiClaims) Delete(_ context.Context, identity releaseclaim.Identity, claim releaseclaim.Claim) error {
	want, ok := m.claims[identity.ClusterUID]
	if !ok || want != claim {
		return releaseclaim.ErrOwnershipConflict
	}
	delete(m.claims, identity.ClusterUID)
	return nil
}

func TestEnvironmentMultiTargetApplyUsesIndependentRemoteState(t *testing.T) {
	e, secondServer, secondCluster, _ := newEnvironmentMultiTargetTest(t, true)
	helms := &environmentMultiHelm{t: t, state: make(map[string]environmentMultiHelmState)}
	claims := newEnvironmentMultiClaims()
	configureEnvironmentMultiReconciler(t, e, helms, claims, map[string]string{eHostForSecret(t, e.reader, testSecretName): "resource-a", secondServer.URL: "resource-b"})
	setEnvironmentSnapshot(t, e, []platformv1alpha1.ApplicationEnvironmentTarget{{Name: secondCluster.Name, UID: secondCluster.UID}, {Name: e.cluster.Name, UID: e.cluster.UID}})
	assertEnvironmentCheckpointAtHelmEntry(t, e, helms, map[string]types.UID{eHostForSecret(t, e.reader, testSecretName): e.cluster.UID, secondServer.URL: secondCluster.UID})

	e.reconcile(t)
	assertEnvironmentMultiStatus(t, e, helms, claims, secondCluster, "resource-a", "resource-b")
	assertAggregatedEnvironmentHealthy(t, e.getApp(t))

	// Environment membership labels can change after snapshot; the explicit targets remain fixed.
	clusterA := e.cluster.DeepCopy()
	clusterA.Labels = map[string]string{"environment": "staging"}
	if err := e.c.Update(context.Background(), clusterA); err != nil {
		t.Fatal(err)
	}
	clusterB := secondCluster.DeepCopy()
	clusterB.Labels = map[string]string{"environment": "production"}
	if err := e.c.Update(context.Background(), clusterB); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	got := e.getApp(t)
	if len(got.Spec.Environment.Targets) != 2 || len(got.Status.Instances) != 2 || len(helms.requests) != 4 {
		t.Fatalf("label changes altered fixed snapshot or target progression: targets=%#v instances=%#v requests=%d", got.Spec.Environment.Targets, got.Status.Instances, len(helms.requests))
	}
	for _, host := range []string{eHostForSecret(t, e.reader, testSecretName), secondServer.URL} {
		if helms.state[host].revision != 1 {
			t.Fatalf("unchanged follow-up reconcile advanced Helm revision for %s: %#v", host, helms.state[host])
		}
	}
	assertAggregatedEnvironmentHealthy(t, got)
}

func TestEnvironmentMultiTargetUnavailableTargetRecoversWithoutRollback(t *testing.T) {
	e, secondServer, secondCluster, secondSecret := newEnvironmentMultiTargetTest(t, false)
	helms := &environmentMultiHelm{t: t, state: make(map[string]environmentMultiHelmState)}
	claims := newEnvironmentMultiClaims()
	configureEnvironmentMultiReconciler(t, e, helms, claims, map[string]string{eHostForSecret(t, e.reader, testSecretName): "resource-a", secondServer.URL: "resource-b"})
	setEnvironmentSnapshot(t, e, []platformv1alpha1.ApplicationEnvironmentTarget{{Name: e.cluster.Name, UID: e.cluster.UID}, {Name: secondCluster.Name, UID: secondCluster.UID}})
	assertEnvironmentCheckpointAtHelmEntry(t, e, helms, map[string]types.UID{eHostForSecret(t, e.reader, testSecretName): e.cluster.UID, secondServer.URL: secondCluster.UID})

	e.reconcile(t)
	first := e.getApp(t)
	if len(helms.requests) != 1 || helms.requests[0].host != eHostForSecret(t, e.reader, testSecretName) {
		t.Fatalf("unavailable target prevented independent target apply: %#v", helms.requests)
	}
	if first.Status.Instances[0].Applied == nil || first.Status.Instances[0].Applied.HelmRevision != 1 || first.Status.Instances[1].Binding != nil {
		t.Fatalf("first-pass target-local status not retained: %#v", first.Status.Instances)
	}
	ready := conditionForEnvironmentMulti(first.Status.Instances[1].Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "TargetUnavailable" {
		t.Fatalf("missing Secret was not reported target-locally: %#v", ready)
	}

	if err := e.reader.Create(context.Background(), secondSecret); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	second := e.getApp(t)
	if len(helms.requests) != 3 {
		t.Fatalf("recovery pass should reapply the first target and apply the restored target: %#v", helms.requests)
	}
	byUID := map[types.UID]platformv1alpha1.ApplicationInstanceStatus{}
	for _, instance := range second.Status.Instances {
		byUID[instance.ClusterUID] = instance
	}
	firstInstance, firstExists := byUID[e.cluster.UID]
	secondInstance, secondExists := byUID[secondCluster.UID]
	if !firstExists || !secondExists || firstInstance.Applied == nil || firstInstance.Applied.HelmRevision != 1 || secondInstance.Applied == nil || secondInstance.Applied.HelmRevision != 1 {
		t.Fatalf("target-local recovery changed or lost revisions: %#v", second.Status.Instances)
	}
	if firstInstance.Binding == nil || firstInstance.Binding.RemoteClaimUID != types.UID("claim-"+string(e.cluster.UID)) || secondInstance.Binding == nil || secondInstance.Binding.RemoteClaimUID != types.UID("claim-"+string(secondCluster.UID)) {
		t.Fatalf("target-specific claims were not preserved: %#v", second.Status.Instances)
	}
	if got := metaFindApplicationCondition(second.Status.Conditions, "Ready"); got == nil || got.Status != metav1.ConditionTrue {
		t.Fatalf("environment did not converge after restoring credentials: %#v", got)
	}
}

func TestEnvironmentAggregatesStayInvalidWhenLaterTargetBecomesStale(t *testing.T) {
	e, secondServer, secondCluster, secondSecret := newEnvironmentMultiTargetTest(t, true)
	helms := &environmentMultiHelm{t: t, state: make(map[string]environmentMultiHelmState)}
	claims := newEnvironmentMultiClaims()
	configureEnvironmentMultiReconciler(t, e, helms, claims, map[string]string{eHostForSecret(t, e.reader, testSecretName): "resource-a", secondServer.URL: "resource-b"})
	setEnvironmentSnapshot(t, e, []platformv1alpha1.ApplicationEnvironmentTarget{{Name: e.cluster.Name, UID: e.cluster.UID}, {Name: secondCluster.Name, UID: secondCluster.UID}})
	e.reconcile(t)
	assertAggregatedEnvironmentHealthy(t, e.getApp(t))

	firstSecret := &corev1.Secret{}
	firstSecretKey := types.NamespacedName{Namespace: testManagerNamespace, Name: testSecretName}
	if err := e.reader.Get(context.Background(), firstSecretKey, firstSecret); err != nil {
		t.Fatal(err)
	}
	if err := e.reader.Delete(context.Background(), firstSecret); err != nil {
		t.Fatal(err)
	}
	statusClient := &environmentStatusHookClient{Client: e.c}
	mutatedDefinition := false
	statusClient.afterStatusUpdate = func(object client.Object) {
		app, ok := object.(*platformv1alpha1.Application)
		if !ok || mutatedDefinition {
			return
		}
		instance := findEnvironmentInstance(app.Status.Instances, e.cluster.UID)
		ready := (*metav1.Condition)(nil)
		if instance != nil {
			ready = conditionForEnvironmentMulti(instance.Conditions, "Ready")
		}
		if ready == nil || ready.Reason != "TargetUnavailable" {
			return
		}
		mutatedDefinition = true
		definition := &platformv1alpha1.ApplicationDefinition{}
		if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: e.definition.Namespace, Name: e.definition.Name}, definition); err != nil {
			t.Fatal(err)
		}
		validated := meta.FindStatusCondition(definition.Status.Conditions, "Validated")
		validated.Status = metav1.ConditionFalse
		validated.Reason = "InvalidatedDuringTargetPass"
		if err := e.c.Status().Update(context.Background(), definition); err != nil {
			t.Fatal(err)
		}
	}
	e.r.Client = statusClient
	e.reconcile(t)
	if !mutatedDefinition || len(helms.requests) != 2 {
		t.Fatalf("later target was mutated after Definition status invalidation: changed=%t requests=%d", mutatedDefinition, len(helms.requests))
	}
	got := e.getApp(t)
	if !applicationAggregateIsInvalid(got) {
		t.Fatalf("previously healthy root conditions survived target failure/stale stop: %#v", got.Status.Conditions)
	}
	if condition := metaFindApplicationCondition(got.Status.Conditions, "Ready"); condition.Status != metav1.ConditionFalse {
		t.Fatalf("Ready should be false after a target failure, got %#v", condition)
	}
	if condition := metaFindApplicationCondition(got.Status.Conditions, "InventoryReady"); condition.Status != metav1.ConditionUnknown {
		t.Fatalf("InventoryReady should be unknown after a target failure, got %#v", condition)
	}
	if condition := metaFindApplicationCondition(got.Status.Conditions, "WorkloadHealthy"); condition.Status != metav1.ConditionUnknown {
		t.Fatalf("WorkloadHealthy should be unknown after a target failure, got %#v", condition)
	}
	firstSecret.ResourceVersion = ""
	firstSecret.UID = ""
	if err := e.reader.Create(context.Background(), firstSecret); err != nil {
		t.Fatal(err)
	}
	_ = secondSecret
}

func TestEnvironmentApplyPendingCheckpointInvalidatesPreviouslyHealthyRoot(t *testing.T) {
	e, secondServer, secondCluster, _ := newEnvironmentMultiTargetTest(t, true)
	helms := &environmentMultiHelm{t: t, state: make(map[string]environmentMultiHelmState)}
	claims := newEnvironmentMultiClaims()
	configureEnvironmentMultiReconciler(t, e, helms, claims, map[string]string{eHostForSecret(t, e.reader, testSecretName): "resource-a", secondServer.URL: "resource-b"})
	setEnvironmentSnapshot(t, e, []platformv1alpha1.ApplicationEnvironmentTarget{{Name: e.cluster.Name, UID: e.cluster.UID}, {Name: secondCluster.Name, UID: secondCluster.UID}})
	e.reconcile(t)
	assertAggregatedEnvironmentHealthy(t, e.getApp(t))
	if len(helms.requests) != 2 {
		t.Fatalf("initial target pass made %d Helm calls, want two", len(helms.requests))
	}
	bumpApplicationGeneration(t, e)
	statusClient := &environmentStatusHookClient{Client: e.c}
	invalidated := false
	statusClient.afterStatusUpdate = func(object client.Object) {
		app, ok := object.(*platformv1alpha1.Application)
		if !ok || invalidated {
			return
		}
		instance := findEnvironmentInstance(app.Status.Instances, e.cluster.UID)
		inventory := (*metav1.Condition)(nil)
		if instance != nil {
			inventory = conditionForEnvironmentMulti(instance.Conditions, "InventoryReady")
		}
		if inventory == nil || inventory.Reason != "ApplyPending" {
			return
		}
		invalidated = true
		definition := &platformv1alpha1.ApplicationDefinition{}
		if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: e.definition.Namespace, Name: e.definition.Name}, definition); err != nil {
			t.Fatal(err)
		}
		validated := meta.FindStatusCondition(definition.Status.Conditions, "Validated")
		validated.Status = metav1.ConditionFalse
		validated.Reason = "InvalidatedAtApplyPending"
		if err := e.c.Status().Update(context.Background(), definition); err != nil {
			t.Fatal(err)
		}
	}
	e.r.Client = statusClient
	e.reconcile(t)
	if !invalidated || len(helms.requests) != 2 {
		t.Fatalf("ApplyPending invalidation did not stop before Helm: invalidated=%t calls=%d", invalidated, len(helms.requests))
	}
	got := e.getApp(t)
	if !applicationAggregateIsInvalid(got) {
		t.Fatalf("pending checkpoint preserved previously healthy aggregate conditions: %#v", got.Status.Conditions)
	}
}

func findEnvironmentInstance(instances []platformv1alpha1.ApplicationInstanceStatus, clusterUID types.UID) *platformv1alpha1.ApplicationInstanceStatus {
	for i := range instances {
		if instances[i].ClusterUID == clusterUID {
			return &instances[i]
		}
	}
	return nil
}

func newEnvironmentMultiTargetTest(t *testing.T, createSecondSecret bool) (*appTestEnvironment, *httptest.Server, *platformv1alpha1.Cluster, *corev1.Secret) {
	t.Helper()
	e := newAppTestEnvironment(t, nil)
	secondServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(secondServer.Close)
	secondCluster := &platformv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "z-target", UID: types.UID("z-cluster-uid"), Generation: 1}, Spec: platformv1alpha1.ClusterSpec{KubeconfigSecretRef: platformv1alpha1.KubeconfigSecretReference{Name: "z-kubeconfig"}}}
	if err := e.c.Create(context.Background(), secondCluster); err != nil {
		t.Fatal(err)
	}
	secondSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testManagerNamespace, Name: "z-kubeconfig"}, Data: map[string][]byte{"kubeconfig": testKubeconfig(secondServer)}}
	if createSecondSecret {
		if err := e.reader.Create(context.Background(), secondSecret); err != nil {
			t.Fatal(err)
		}
	}
	return e, secondServer, secondCluster, secondSecret
}

func configureEnvironmentMultiReconciler(t *testing.T, e *appTestEnvironment, helms *environmentMultiHelm, claims *environmentMultiClaims, resourceNames map[string]string) {
	t.Helper()
	e.r.HelmFactory = func(getter genericclioptions.RESTClientGetter) applicationHelm {
		config, err := getter.ToRESTConfig()
		if err != nil {
			t.Fatalf("get target REST config: %v", err)
		}
		return &environmentHostHelm{parent: helms, host: config.Host}
	}
	e.r.ClaimManagerFactory = func(interfaceKube kubernetes.Interface) releaseClaimManager { return claims }
	e.r.ResourceCollector = func(_ context.Context, config *rest.Config, _, _ string) ([]platformv1alpha1.ApplicationResource, error) {
		name, ok := resourceNames[config.Host]
		if !ok {
			return nil, fmt.Errorf("no resource fixture for target %s", config.Host)
		}
		return []platformv1alpha1.ApplicationResource{{APIVersion: "apps/v1", Kind: "Deployment", Namespace: appTestReleaseNamespace, Name: name, UID: types.UID("uid-" + name), Deployment: &platformv1alpha1.ApplicationDeploymentHealth{DesiredReplicas: 1, ReadyReplicas: 1, State: applicationWorkloadHealthy}}}, nil
	}
}

func setEnvironmentSnapshot(t *testing.T, e *appTestEnvironment, targets []platformv1alpha1.ApplicationEnvironmentTarget) {
	t.Helper()
	app := e.getApp(t)
	app.Spec.ClusterRef = nil
	app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "production", Targets: targets}
	if err := e.c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
}

func assertEnvironmentMultiStatus(t *testing.T, e *appTestEnvironment, helms *environmentMultiHelm, claims *environmentMultiClaims, secondCluster *platformv1alpha1.Cluster, resourceA, resourceB string) {
	t.Helper()
	got := e.getApp(t)
	if got.Status.Binding != nil || got.Status.Applied != nil {
		t.Fatalf("environment mode wrote legacy singular status: binding=%#v applied=%#v", got.Status.Binding, got.Status.Applied)
	}
	if len(got.Status.ChartPins) != 1 || len(got.Status.Instances) != 2 || len(helms.requests) != 2 || len(claims.acquired) != 2 {
		t.Fatalf("incomplete independent target application: pins=%#v instances=%#v Helm=%#v claims=%#v", got.Status.ChartPins, got.Status.Instances, helms.requests, claims.acquired)
	}
	instances := map[types.UID]platformv1alpha1.ApplicationInstanceStatus{}
	for _, instance := range got.Status.Instances {
		instances[instance.ClusterUID] = instance
	}
	for _, expectation := range []struct {
		cluster *platformv1alpha1.Cluster
		name    string
	}{{e.cluster, resourceA}, {secondCluster, resourceB}} {
		instance, ok := instances[expectation.cluster.UID]
		if !ok || instance.Binding == nil || instance.Binding.ClusterUID != expectation.cluster.UID || instance.Binding.RemoteClaimUID != types.UID("claim-"+string(expectation.cluster.UID)) || instance.Applied == nil || instance.Applied.HelmRevision != 1 || len(instance.Resources) != 1 || instance.Resources[0].Name != expectation.name {
			t.Fatalf("target %s did not retain independent binding, revision, and inventory: instance=%#v helm=%#v claims=%#v appConditions=%#v", expectation.cluster.Name, instance, helms.requests, claims.acquired, got.Status.Conditions)
		}
		if instance.Binding.TargetAPIServer == "" {
			t.Fatalf("target %s binding missing API server", expectation.cluster.Name)
		}
	}
	first, second := instances[e.cluster.UID], instances[secondCluster.UID]
	if first.Binding.TargetAPIServer == second.Binding.TargetAPIServer || first.Binding.RemoteClaimUID == second.Binding.RemoteClaimUID {
		t.Fatalf("targets shared remote identity: first=%#v second=%#v", first.Binding, second.Binding)
	}
	for _, identity := range claims.acquired {
		if identity.ClusterUID != e.cluster.UID && identity.ClusterUID != secondCluster.UID {
			t.Fatalf("claim acquired for unexpected cluster: %#v", identity)
		}
	}
	for _, expectedHost := range []string{eHostForSecret(t, e.reader, testSecretName), eHostForSecret(t, e.reader, "z-kubeconfig")} {
		if helms.state[expectedHost].revision != 1 {
			t.Fatalf("target %s has unexpected Helm revision: %#v", expectedHost, helms.state[expectedHost])
		}
	}
}

func assertAggregatedEnvironmentHealthy(t *testing.T, app *platformv1alpha1.Application) {
	t.Helper()
	for _, conditionType := range []string{"Ready", "InventoryReady", "WorkloadHealthy"} {
		condition := metaFindApplicationCondition(app.Status.Conditions, conditionType)
		if condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != app.Generation {
			t.Fatalf("aggregate %s condition is not current and healthy: %#v", conditionType, condition)
		}
	}
	for _, instance := range app.Status.Instances {
		if instance.WorkloadObservedAt == nil || time.Since(instance.WorkloadObservedAt.Time) < 0 || time.Since(instance.WorkloadObservedAt.Time) > 5*time.Minute {
			t.Fatalf("target %s has no fresh workload observation: %#v", instance.ClusterName, instance.WorkloadObservedAt)
		}
	}
}

func assertEnvironmentCheckpointAtHelmEntry(t *testing.T, e *appTestEnvironment, helms *environmentMultiHelm, targetUIDs map[string]types.UID) {
	t.Helper()
	helms.beforeApply = func(host string, request helmrelease.Request) {
		clusterUID, ok := targetUIDs[host]
		if !ok {
			t.Fatalf("Helm called for unrecognized target host %q", host)
		}
		app := e.getApp(t)
		instanceIndex := -1
		for i := range app.Status.Instances {
			if app.Status.Instances[i].ClusterUID == clusterUID {
				instanceIndex = i
				break
			}
		}
		if instanceIndex < 0 {
			t.Fatalf("target %s status was not persisted before Helm", clusterUID)
		}
		instance := app.Status.Instances[instanceIndex]
		wantIdentity, err := helmrelease.DesiredIdentity(request.Repository, request.ChartName, request.ChartVersion, request.Digest, request.Values)
		if err != nil {
			t.Fatalf("compute request identity: %v", err)
		}
		if instance.Binding == nil || instance.Binding.ClusterUID != clusterUID || instance.Binding.RemoteClaimUID != types.UID("claim-"+string(clusterUID)) || instance.LastAttemptedIdentity != wantIdentity {
			t.Fatalf("target %s checkpoint missing at Helm entry: binding=%#v attempted=%q want=%q", clusterUID, instance.Binding, instance.LastAttemptedIdentity, wantIdentity)
		}
	}
}

func conditionForEnvironmentMulti(conditions []metav1.Condition, typ string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == typ {
			return &conditions[i]
		}
	}
	return nil
}

func metaFindApplicationCondition(conditions []metav1.Condition, typ string) *metav1.Condition {
	return conditionForEnvironmentMulti(conditions, typ)
}

func eHostForSecret(t *testing.T, reader client.Reader, name string) string {
	t.Helper()
	secret := &corev1.Secret{}
	if err := reader.Get(context.Background(), types.NamespacedName{Namespace: testManagerNamespace, Name: name}, secret); err != nil {
		t.Fatal(err)
	}
	config, _, err := parseRemoteClusterConfig(secret.Data["kubeconfig"], appTestReleaseNamespace)
	if err != nil {
		t.Fatal(err)
	}
	return config.Host
}

var _ releaseclaim.Manager = (*environmentMultiClaims)(nil)
var _ applicationHelm = (*environmentHostHelm)(nil)
var _ client.Reader = appCompositeReader{}
