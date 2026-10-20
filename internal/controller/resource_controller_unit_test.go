package controller

import (
	"context"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
)

var bucketGVK = schema.GroupVersionKind{Group: "s3.aws.m.upbound.io", Version: "v1beta1", Kind: "Bucket"}

const (
	resourceTestNamespace = "team-a"
	resourceTestName      = "logs"
)

func newResourceTestReconciler(t *testing.T, objects ...client.Object) (*ResourceReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(bucketGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(bucketGVK.GroupVersion().WithKind("BucketList"), &unstructured.UnstructuredList{})
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&platformv1alpha1.Resource{}).
		WithObjects(objects...).
		Build()
	return &ResourceReconciler{Client: c, Scheme: scheme}, c
}

func testResource() *platformv1alpha1.Resource {
	return &platformv1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{Name: resourceTestName, Namespace: resourceTestNamespace, Generation: 1},
		Spec: platformv1alpha1.ResourceSpec{
			DefinitionRef: platformv1alpha1.LocalReference{Name: "bucket"},
			TargetRef:     platformv1alpha1.LocalReference{Name: "aws"},
		},
	}
}

func testDefinition() *platformv1alpha1.ResourceDefinition {
	return &platformv1alpha1.ResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "bucket"},
		Spec: platformv1alpha1.ResourceDefinitionSpec{
			Resources: []platformv1alpha1.ResourceTemplate{{
				Name:       "bucket",
				APIVersion: bucketGVK.Group + "/" + bucketGVK.Version,
				Kind:       bucketGVK.Kind,
				Base:       &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"managementPolicies":["*"]}}`)},
				Mappings: []platformv1alpha1.FieldMapping{
					{From: "values.bucketName", To: "metadata.annotations[crossplane.io/external-name]"},
					{From: "target.region", To: "spec.forProvider.region"},
				},
			}},
			Outputs: []platformv1alpha1.ResourceOutput{
				{Name: "arn", From: "status.atProvider.arn"},
			},
		},
	}
}

func testTarget() *platformv1alpha1.CloudTarget {
	return &platformv1alpha1.CloudTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "aws"},
		Spec: platformv1alpha1.CloudTargetSpec{
			Provider: "aws",
			Region:   "eu-west-1",
			ProviderConfigRef: platformv1alpha1.ProviderConfigReference{
				Name:      "localstack",
				Namespace: resourceTestNamespace,
			},
		},
	}
}

func reconcileResource(t *testing.T, r *ResourceReconciler) {
	t.Helper()
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: resourceTestNamespace, Name: resourceTestName}}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func resourceCondition(t *testing.T, c client.Client) *metav1.Condition {
	t.Helper()
	var got platformv1alpha1.Resource
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: resourceTestNamespace, Name: resourceTestName}, &got); err != nil {
		t.Fatal(err)
	}
	return meta.FindStatusCondition(got.Status.Conditions, resourceConditionReady)
}

func TestResourceReconcileDefinitionNotFound(t *testing.T) {
	r, c := newResourceTestReconciler(t, testResource())
	reconcileResource(t, r)

	condition := resourceCondition(t, c)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != resourceReasonDefinitionNotFound {
		t.Fatalf("unexpected condition: %#v", condition)
	}
}

func TestResourceReconcileTargetNotFound(t *testing.T) {
	r, c := newResourceTestReconciler(t, testResource(), testDefinition())
	reconcileResource(t, r)

	condition := resourceCondition(t, c)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != resourceReasonTargetNotFound {
		t.Fatalf("unexpected condition: %#v", condition)
	}
}

func TestResourceReconcileResolvesDefinitionAndTarget(t *testing.T) {
	r, c := newResourceTestReconciler(t, testResource(), testDefinition(), testTarget())
	reconcileResource(t, r)

	condition := resourceCondition(t, c)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != resourceReasonCreating {
		t.Fatalf("unexpected condition: %#v", condition)
	}
}

func TestResourceReconcileAppliesManagedResource(t *testing.T) {
	resource := testResource()
	resource.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(`{"bucketName":"logs-bucket"}`)}
	r, c := newResourceTestReconciler(t, resource, testDefinition(), testTarget())
	reconcileResource(t, r)

	bucket := &unstructured.Unstructured{}
	bucket.SetGroupVersionKind(bucketGVK)
	key := types.NamespacedName{Namespace: resourceTestNamespace, Name: "logs-bucket"}
	if err := c.Get(context.Background(), key, bucket); err != nil {
		t.Fatalf("get composed bucket: %v", err)
	}
	if region, _, _ := unstructured.NestedString(bucket.Object, "spec", "forProvider", "region"); region != "eu-west-1" {
		t.Fatalf("unexpected region: %q", region)
	}
	if got := bucket.GetAnnotations()["crossplane.io/external-name"]; got != "logs-bucket" {
		t.Fatalf("unexpected external-name: %q", got)
	}

	var applied platformv1alpha1.Resource
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: resourceTestNamespace, Name: resourceTestName}, &applied); err != nil {
		t.Fatal(err)
	}
	if len(applied.Status.Composed) != 1 || applied.Status.Composed[0].Name != "bucket" || applied.Status.Composed[0].Kind != "Bucket" {
		t.Fatalf("unexpected composed status: %#v", applied.Status.Composed)
	}
}

func TestResourceReconcileReportsReadyAndOutputs(t *testing.T) {
	resource := testResource()
	resource.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(`{"bucketName":"logs-bucket"}`)}
	r, c := newResourceTestReconciler(t, resource, testDefinition(), testTarget())
	reconcileResource(t, r)

	bucket := &unstructured.Unstructured{}
	bucket.SetGroupVersionKind(bucketGVK)
	key := types.NamespacedName{Namespace: resourceTestNamespace, Name: "logs-bucket"}
	if err := c.Get(context.Background(), key, bucket); err != nil {
		t.Fatalf("get composed bucket: %v", err)
	}
	if err := unstructured.SetNestedSlice(bucket.Object, []any{
		map[string]any{"type": "Ready", "status": "True", "reason": "Available", "message": "Available"},
	}, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(bucket.Object, "arn:aws:s3:::logs-bucket", "status", "atProvider", "arn"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(context.Background(), bucket); err != nil {
		t.Fatalf("update composed bucket status: %v", err)
	}

	reconcileResource(t, r)

	var applied platformv1alpha1.Resource
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: resourceTestNamespace, Name: resourceTestName}, &applied); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(applied.Status.Conditions, resourceConditionReady)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != resourceReasonAvailable {
		t.Fatalf("unexpected condition: %#v", condition)
	}
	if applied.Status.Outputs["arn"] != "arn:aws:s3:::logs-bucket" {
		t.Fatalf("unexpected outputs: %#v", applied.Status.Outputs)
	}
}
