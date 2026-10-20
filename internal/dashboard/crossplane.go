package dashboard

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func crossplaneInstalled(ctx context.Context, c client.Client) bool {
	crd := apiextensionsv1.CustomResourceDefinition{}
	err := c.Get(ctx, client.ObjectKey{Name: "compositions.apiextensions.crossplane.io"}, &crd)
	if err != nil {
		return false
	}
	return true
}
