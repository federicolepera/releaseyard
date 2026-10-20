package controller

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
)

func TestBuildManagedResource(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	resource := testResource()
	resource.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(`{"bucketName":"logs-bucket"}`)}
	target := testTarget()
	template := platformv1alpha1.ResourceTemplate{
		Name:       "bucket",
		APIVersion: "s3.aws.m.upbound.io/v1beta1",
		Kind:       "Bucket",
		Base:       &apiextensionsv1.JSON{Raw: []byte(`{"spec":{"managementPolicies":["*"]}}`)},
		Mappings: []platformv1alpha1.FieldMapping{
			{From: "values.bucketName", To: "metadata.annotations[crossplane.io/external-name]"},
			{From: "target.region", To: "spec.forProvider.region"},
		},
	}

	got, err := buildManagedResource(resource, target, template, scheme)
	if err != nil {
		t.Fatalf("buildManagedResource: %v", err)
	}

	if got.GetAPIVersion() != template.APIVersion || got.GetKind() != template.Kind {
		t.Fatalf("unexpected GVK: %s %s", got.GetAPIVersion(), got.GetKind())
	}
	if got.GetName() != "logs-bucket" || got.GetNamespace() != resourceTestNamespace {
		t.Fatalf("unexpected name/namespace: %s/%s", got.GetNamespace(), got.GetName())
	}
	if got.GetLabels()[resourceLabel] != resourceTestName || got.GetLabels()[managedByLabel] != "releaseyard" {
		t.Fatalf("unexpected labels: %#v", got.GetLabels())
	}
	if got.GetAnnotations()["crossplane.io/external-name"] != "logs-bucket" {
		t.Fatalf("unexpected annotations: %#v", got.GetAnnotations())
	}
	if region, _, _ := unstructured.NestedString(got.Object, "spec", "forProvider", "region"); region != "eu-west-1" {
		t.Fatalf("unexpected region: %q", region)
	}
	if policies, _, _ := unstructured.NestedStringSlice(got.Object, "spec", "managementPolicies"); len(policies) != 1 || policies[0] != "*" {
		t.Fatalf("unexpected management policies: %#v", policies)
	}
	if name, _, _ := unstructured.NestedString(got.Object, "spec", "providerConfigRef", "name"); name != "localstack" {
		t.Fatalf("unexpected providerConfigRef name: %q", name)
	}
	if refs := got.GetOwnerReferences(); len(refs) != 1 || refs[0].Kind != "Resource" || refs[0].Name != resourceTestName {
		t.Fatalf("unexpected owner references: %#v", refs)
	}
}

func TestBuildManagedResourceSkipsMissingValue(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	resource := testResource()
	template := platformv1alpha1.ResourceTemplate{
		Name:       "bucket",
		APIVersion: "s3.aws.m.upbound.io/v1beta1",
		Kind:       "Bucket",
		Mappings: []platformv1alpha1.FieldMapping{
			{From: "values.bucketName", To: "metadata.annotations[crossplane.io/external-name]"},
		},
	}

	got, err := buildManagedResource(resource, testTarget(), template, scheme)
	if err != nil {
		t.Fatalf("buildManagedResource: %v", err)
	}
	if _, ok := got.GetAnnotations()["crossplane.io/external-name"]; ok {
		t.Fatalf("unexpected external-name annotation: %#v", got.GetAnnotations())
	}
}

func TestParseFieldPath(t *testing.T) {
	segments, err := parseFieldPath("metadata.annotations[crossplane.io/external-name]")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"metadata", "annotations", "crossplane.io/external-name"}
	if len(segments) != len(want) {
		t.Fatalf("unexpected segments: %#v", segments)
	}
	for i := range want {
		if segments[i] != want[i] {
			t.Fatalf("segment %d = %q, want %q", i, segments[i], want[i])
		}
	}
}
