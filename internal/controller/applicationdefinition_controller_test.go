/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testApplicationDefinitionName = "web-service"
	staleDefinitionDigest         = "old-digest"
)

func TestApplicationDefinitionReconcilerInvalidContract(t *testing.T) {
	definition := testApplicationDefinition()
	definition.Spec.Defaults = rawDefaults(`[]`)
	puller := &fakeChartPuller{digest: testChartDigest}
	reconciler, testClient := testReconciler(t, definition, puller)

	result, err := reconciler.Reconcile(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("invalid contract result = %#v, want no periodic requeue", result)
	}
	if puller.calls != 0 {
		t.Fatalf("Pull() calls = %d, want 0", puller.calls)
	}
	stored := getApplicationDefinition(t, testClient)
	if stored.Status.Digest != "" {
		t.Errorf("status digest = %q, want empty", stored.Status.Digest)
	}
	assertCondition(t, stored, metav1.ConditionFalse, applicationDefinitionReasonInvalid, definition.Generation)
	if got := conditionMessage(stored); strings.Contains(got, "[]") {
		t.Errorf("condition message exposes defaults: %q", got)
	}
}

func TestApplicationDefinitionReconcilerSuccess(t *testing.T) {
	definition := testApplicationDefinition()
	puller := &fakeChartPuller{digest: testChartDigest}
	reconciler, testClient := testReconciler(t, definition, puller)

	result, err := reconciler.Reconcile(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != applicationDefinitionRecheck {
		t.Errorf("RequeueAfter = %s, want %s", result.RequeueAfter, applicationDefinitionRecheck)
	}
	if puller.calls != 1 || puller.chart != definition.Spec.Chart {
		t.Errorf("Pull() called %d times with chart %#v", puller.calls, puller.chart)
	}
	stored := getApplicationDefinition(t, testClient)
	if stored.Status.Digest != testChartDigest {
		t.Errorf("status digest = %q, want %q", stored.Status.Digest, testChartDigest)
	}
	assertCondition(t, stored, metav1.ConditionTrue, applicationDefinitionReasonValid, definition.Generation)
	if stored.Status.LastCheckedTime.IsZero() {
		t.Error("LastCheckedTime is zero")
	}
	if stored.Status.ObservedGeneration != definition.Generation {
		t.Errorf("ObservedGeneration = %d, want %d", stored.Status.ObservedGeneration, definition.Generation)
	}
}

func TestApplicationDefinitionReconcilerFetchFailureClearsDigestAndRetries(t *testing.T) {
	definition := testApplicationDefinition()
	definition.Status.Digest = "sha256:" + strings.Repeat("a", 64)
	puller := &fakeChartPuller{err: errors.New("private registry credential leaked")}
	reconciler, testClient := testReconciler(t, definition, puller)

	result, err := reconciler.Reconcile(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != applicationDefinitionFetchRetry {
		t.Errorf("RequeueAfter = %s, want %s", result.RequeueAfter, applicationDefinitionFetchRetry)
	}
	stored := getApplicationDefinition(t, testClient)
	if stored.Status.Digest != "" {
		t.Errorf("status digest = %q, want empty", stored.Status.Digest)
	}
	assertCondition(t, stored, metav1.ConditionFalse, applicationDefinitionReasonFetch, definition.Generation)
	if got := conditionMessage(stored); strings.Contains(got, "private registry") {
		t.Errorf("condition message exposes pull error: %q", got)
	}
}

func TestApplicationDefinitionReconcilerRejectsInvalidDigest(t *testing.T) {
	definition := testApplicationDefinition()
	puller := &fakeChartPuller{digest: "not-a-digest"}
	reconciler, _ := testReconciler(t, definition, puller)

	result, err := reconciler.Reconcile(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != applicationDefinitionFetchRetry {
		t.Errorf("RequeueAfter = %s, want %s", result.RequeueAfter, applicationDefinitionFetchRetry)
	}
}

func TestApplicationDefinitionReconcilerSkipsStaleGenerationStatus(t *testing.T) {
	definition := testApplicationDefinition()
	definition.Status.Digest = staleDefinitionDigest
	puller := &fakeChartPuller{digest: testChartDigest}
	reconciler, testClient := testReconciler(t, definition, puller)
	reconciler.Client = &generationChangingClient{Client: testClient}

	result, err := reconciler.Reconcile(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("stale generation result = %#v, want no requeue", result)
	}
	stored := getApplicationDefinition(t, testClient)
	if stored.Status.Digest != staleDefinitionDigest {
		t.Errorf("status digest = %q, want unchanged old digest", stored.Status.Digest)
	}
	if stored.Status.ObservedGeneration != 0 {
		t.Errorf("ObservedGeneration = %d, want unchanged zero", stored.Status.ObservedGeneration)
	}
}

func TestApplicationDefinitionReconcilerSkipsReplacedObjectStatus(t *testing.T) {
	definition := testApplicationDefinition()
	definition.UID = types.UID("original-uid")
	definition.Status.Digest = staleDefinitionDigest
	puller := &fakeChartPuller{digest: testChartDigest}
	reconciler, testClient := testReconciler(t, definition, puller)
	reconciler.Client = &uidChangingClient{Client: testClient}

	result, err := reconciler.Reconcile(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("replaced object result = %#v, want no requeue", result)
	}
	stored := getApplicationDefinition(t, testClient)
	if stored.Status.Digest != staleDefinitionDigest {
		t.Errorf("status digest = %q, want unchanged old digest", stored.Status.Digest)
	}
	if stored.Status.ObservedGeneration != 0 {
		t.Errorf("ObservedGeneration = %d, want unchanged zero", stored.Status.ObservedGeneration)
	}
}

func TestApplicationDefinitionReconcilerCancellationDoesNotRecordFetchFailure(t *testing.T) {
	definition := testApplicationDefinition()
	definition.Status.Digest = staleDefinitionDigest
	ctx, cancel := context.WithCancel(context.Background())
	puller := &fakeChartPuller{onPull: cancel, err: errors.New("fetch interrupted")}
	reconciler, testClient := testReconciler(t, definition, puller)

	result, err := reconciler.Reconcile(ctx, testRequest())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %s, want 0 after cancellation", result.RequeueAfter)
	}
	stored := getApplicationDefinition(t, testClient)
	if stored.Status.Digest != "old-digest" || len(stored.Status.Conditions) != 0 {
		t.Errorf("status changed after cancellation: %#v", stored.Status)
	}
}

const testChartDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeChartPuller struct {
	digest string
	err    error
	calls  int
	chart  platformv1alpha1.ApplicationDefinitionChart
	onPull func()
}

func (p *fakeChartPuller) Pull(_ context.Context, chart platformv1alpha1.ApplicationDefinitionChart) (string, error) {
	p.calls++
	p.chart = chart
	if p.onPull != nil {
		p.onPull()
	}
	return p.digest, p.err
}

type generationChangingClient struct {
	client.Client
	gets int
}

type uidChangingClient struct {
	client.Client
	gets int
}

func (c *uidChangingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.gets++
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if c.gets == 2 {
		if definition, ok := obj.(*platformv1alpha1.ApplicationDefinition); ok {
			definition.UID = types.UID("replacement-uid")
		}
	}
	return nil
}

func (c *generationChangingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.gets++
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if c.gets == 2 {
		if definition, ok := obj.(*platformv1alpha1.ApplicationDefinition); ok {
			definition.Generation++
		}
	}
	return nil
}

func testReconciler(t *testing.T, definition *platformv1alpha1.ApplicationDefinition, puller ChartPuller) (*ApplicationDefinitionReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	testClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&platformv1alpha1.ApplicationDefinition{}).
		WithObjects(definition).
		Build()
	return &ApplicationDefinitionReconciler{Client: testClient, Scheme: scheme, Puller: puller}, testClient
}

func testApplicationDefinition() *platformv1alpha1.ApplicationDefinition {
	return &platformv1alpha1.ApplicationDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testApplicationDefinitionName,
			Namespace:  "management",
			Generation: 3,
		},
		Spec: platformv1alpha1.ApplicationDefinitionSpec{
			Chart: platformv1alpha1.ApplicationDefinitionChart{
				Repository: apiTestChartRepository,
				Name:       testApplicationDefinitionName,
				Version:    testChartVersion,
			},
			Release: platformv1alpha1.ApplicationDefinitionRelease{Namespace: apiTestApplicationsName},
		},
	}
}

func testRequest() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "management", Name: testApplicationDefinitionName}}
}

func rawDefaults(raw string) *apiextensionsv1.JSON {
	return &apiextensionsv1.JSON{Raw: []byte(raw)}
}

func getApplicationDefinition(t *testing.T, c client.Client) *platformv1alpha1.ApplicationDefinition {
	t.Helper()
	definition := &platformv1alpha1.ApplicationDefinition{}
	if err := c.Get(context.Background(), testRequest().NamespacedName, definition); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	return definition
}

func assertCondition(t *testing.T, definition *platformv1alpha1.ApplicationDefinition, status metav1.ConditionStatus, reason string, generation int64) {
	t.Helper()
	if len(definition.Status.Conditions) != 1 {
		t.Fatalf("conditions = %#v, want exactly one condition", definition.Status.Conditions)
	}
	condition := definition.Status.Conditions[0]
	if condition.Type != applicationDefinitionConditionType || condition.Status != status || condition.Reason != reason {
		t.Errorf("condition = %#v, want type=%q status=%q reason=%q", condition, applicationDefinitionConditionType, status, reason)
	}
	if condition.ObservedGeneration != generation {
		t.Errorf("condition observedGeneration = %d, want %d", condition.ObservedGeneration, generation)
	}
	if condition.LastTransitionTime.IsZero() {
		t.Error("condition LastTransitionTime is zero")
	}
}

func conditionMessage(definition *platformv1alpha1.ApplicationDefinition) string {
	if len(definition.Status.Conditions) == 0 {
		return ""
	}
	return definition.Status.Conditions[0].Message
}
