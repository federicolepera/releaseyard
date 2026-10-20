package dashboard

import (
	"context"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCrossplaneInstalled(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("register apiextensions types: %v", err)
	}

	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "compositions.apiextensions.crossplane.io"},
	}
	present := fake.NewClientBuilder().WithScheme(scheme).WithObjects(crd).Build()
	if !crossplaneInstalled(context.Background(), present) {
		t.Fatal("crossplaneInstalled() = false with a Crossplane CRD present, want true")
	}

	absent := fake.NewClientBuilder().WithScheme(scheme).Build()
	if crossplaneInstalled(context.Background(), absent) {
		t.Fatal("crossplaneInstalled() = true without a Crossplane CRD, want false")
	}
}
