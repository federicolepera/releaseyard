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

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ResourceSpec defines the desired state of Resource.
type ResourceSpec struct {
	// definitionRef references a cluster-scoped ResourceDefinition.
	// +kubebuilder:validation:Required
	DefinitionRef LocalReference `json:"definitionRef"`

	// targetRef references a cluster-scoped CloudTarget.
	// +kubebuilder:validation:Required
	TargetRef LocalReference `json:"targetRef"`

	// values are validated against the definition's configurableValues.
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Values *apiextensionsv1.JSON `json:"values,omitempty"`
}

// LocalReference identifies an object by name.
type LocalReference struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ResourceStatus defines the observed state of Resource.
type ResourceStatus struct {
	// conditions represent the observed state of the Resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// outputs are values read from the composed resources.
	// +optional
	Outputs map[string]string `json:"outputs,omitempty"`

	// composed lists the managed resources created for this Resource.
	// +listType=map
	// +listMapKey=name
	// +optional
	Composed []ComposedResource `json:"composed,omitempty"`
}

// ComposedResource identifies a managed resource created for a Resource.
type ComposedResource struct {
	// name is the template name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// apiVersion of the composed resource.
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`

	// kind of the composed resource.
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`

	// namespace of the composed resource.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Resource is the Schema for the resources API
type Resource struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Resource
	// +required
	Spec ResourceSpec `json:"spec"`

	// status defines the observed state of Resource
	// +optional
	Status ResourceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ResourceList contains a list of Resource
type ResourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Resource `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Resource{}, &ResourceList{})
		return nil
	})
}
