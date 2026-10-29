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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const (
	applicationDefinitionIntegerType   = "integer"
	applicationDefinitionStringType    = "string"
	applicationDefinitionMinimumField  = "minimum"
	applicationDefinitionPathField     = "path"
	applicationDefinitionRequiredField = "required"
	applicationDefinitionTypeField     = "type"
)

var _ = Describe("ApplicationDefinition API", func() {
	var definitionName string

	BeforeEach(func() {
		definitionName = fmt.Sprintf("%s-%d", testApplicationDefinitionName, time.Now().UnixNano())
	})

	It("creates and reads a definition while preserving nested defaults and map-list values", func() {
		definition := validApplicationDefinition(definitionName)
		definition.Object["spec"].(map[string]any)["defaults"] = map[string]any{
			applicationDefinitionReplicasPath: int64(3),
			apiTestResourcesKey: map[string]any{
				apiTestLimitsKey: map[string]any{apiTestCPUKey: "500m", "memory": "512Mi"},
			},
		}
		definition.Object["spec"].(map[string]any)["configurableValues"] = []any{
			map[string]any{applicationDefinitionPathField: applicationDefinitionReplicasPath, applicationDefinitionTypeField: applicationDefinitionIntegerType, applicationDefinitionRequiredField: true, applicationDefinitionMinimumField: int64(1), "maximum": int64(10)},
			map[string]any{applicationDefinitionPathField: "resources.limits.cpu", applicationDefinitionTypeField: applicationDefinitionStringType, applicationDefinitionRequiredField: false},
		}
		Expect(k8sClient.Create(ctx, definition)).To(Succeed())

		readBack := &unstructured.Unstructured{}
		readBack.SetGroupVersionKind(applicationDefinitionGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: definition.GetName()}, readBack)).To(Succeed())
		Expect(readBack.Object["spec"]).To(Equal(definition.Object["spec"]))
	})

	DescribeTable("rejects invalid spec values", func(mutate func(map[string]any)) {
		definition := validApplicationDefinition(definitionName)
		spec := definition.Object["spec"].(map[string]any)
		mutate(spec)
		err := k8sClient.Create(ctx, definition)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected API schema validation error, got %v", err)
	},
		Entry("invalid chart repository", func(spec map[string]any) {
			spec["chart"].(map[string]any)[apiTestRepositoryKey] = "not-a-repository"
		}),
		Entry("latest chart version", func(spec map[string]any) {
			spec["chart"].(map[string]any)[apiTestVersionKey] = "latest"
		}),
		Entry("invalid release namespace", func(spec map[string]any) {
			spec["release"].(map[string]any)[apiTestNamespaceKey] = "Not_A_Namespace"
		}),
		Entry("malformed configurable value path", func(spec map[string]any) {
			spec["configurableValues"] = []any{map[string]any{applicationDefinitionPathField: "a..b", applicationDefinitionTypeField: applicationDefinitionStringType, applicationDefinitionRequiredField: false}}
		}),
		Entry("duplicate configurable value path", func(spec map[string]any) {
			spec["configurableValues"] = []any{
				map[string]any{applicationDefinitionPathField: applicationDefinitionReplicasPath, applicationDefinitionTypeField: applicationDefinitionIntegerType, applicationDefinitionRequiredField: false},
				map[string]any{applicationDefinitionPathField: applicationDefinitionReplicasPath, applicationDefinitionTypeField: applicationDefinitionStringType, applicationDefinitionRequiredField: false},
			}
		}),
		Entry("unknown configurable value type", func(spec map[string]any) {
			spec["configurableValues"] = []any{map[string]any{applicationDefinitionPathField: applicationDefinitionReplicasPath, applicationDefinitionTypeField: "floatish", applicationDefinitionRequiredField: false}}
		}),
		Entry("integer minimum greater than maximum", func(spec map[string]any) {
			spec["configurableValues"] = []any{map[string]any{applicationDefinitionPathField: applicationDefinitionReplicasPath, applicationDefinitionTypeField: applicationDefinitionIntegerType, applicationDefinitionRequiredField: false, applicationDefinitionMinimumField: int64(5), "maximum": int64(2)}}
		}),
		Entry("minimum on a non-integer value", func(spec map[string]any) {
			spec["configurableValues"] = []any{map[string]any{applicationDefinitionPathField: applicationDefinitionReplicasPath, applicationDefinitionTypeField: applicationDefinitionStringType, applicationDefinitionRequiredField: false, applicationDefinitionMinimumField: int64(1)}}
		}),
		Entry("non-object defaults", func(spec map[string]any) {
			spec["defaults"] = "not-an-object"
		}),
	)

	It("accepts an OCI chart repository", func() {
		definition := validApplicationDefinition(definitionName)
		definition.Object["spec"].(map[string]any)["chart"].(map[string]any)[apiTestRepositoryKey] = "oci://ghcr.io/federicolepera/helm-charts"
		Expect(k8sClient.Create(ctx, definition)).To(Succeed())
	})

	It("rejects changing release.namespace after creation", func() {
		definition := validApplicationDefinition(definitionName)
		Expect(k8sClient.Create(ctx, definition)).To(Succeed())

		definition.Object["spec"].(map[string]any)["release"].(map[string]any)[apiTestNamespaceKey] = "other-apps"
		err := k8sClient.Update(ctx, definition)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected immutability validation error, got %v", err)
	})
})

var applicationDefinitionGVK = schema.GroupVersionKind{Group: "platform.releaseyard.io", Version: "v1alpha1", Kind: "ApplicationDefinition"}

func validApplicationDefinition(name string) *unstructured.Unstructured {
	definition := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": applicationDefinitionGVK.Group + "/" + applicationDefinitionGVK.Version,
		"kind":       applicationDefinitionGVK.Kind,
		"metadata": map[string]any{
			apiTestNameKey: name,
		},
		"spec": map[string]any{
			"chart": map[string]any{
				apiTestRepositoryKey: apiTestChartRepository,
				apiTestNameKey:       testApplicationDefinitionName,
				apiTestVersionKey:    "1.4.0",
			},
			"release": map[string]any{apiTestNamespaceKey: apiTestApplicationsName},
		},
	}}
	return definition
}
