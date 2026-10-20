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

// ResourceDefinitionSpec defines the desired state of ResourceDefinition.
type ResourceDefinitionSpec struct {
	// description is a short human-readable summary shown in the catalog.
	// +kubebuilder:validation:MaxLength=280
	// +optional
	Description string `json:"description,omitempty"`

	// configurableValues lists the value paths a Resource may set.
	// +listType=map
	// +listMapKey=path
	// +optional
	ConfigurableValues []ResourceDefinitionValue `json:"configurableValues,omitempty"`

	// resources is the ordered list of cloud objects to create.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	Resources []ResourceTemplate `json:"resources"`

	// outputs maps Resource status keys to paths on the composed resource.
	// +listType=map
	// +listMapKey=name
	// +optional
	Outputs []ResourceOutput `json:"outputs,omitempty"`
}

// ResourceDefinitionValue exposes one configurable value.
type ResourceDefinitionValue struct {
	// path is a dot-separated value path.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern="^[A-Za-z][A-Za-z0-9_-]*(\\.[A-Za-z][A-Za-z0-9_-]*)*$"
	Path string `json:"path"`

	// type is the value type exposed to a Resource.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=string;integer;boolean
	Type string `json:"type"`

	// required indicates whether a Resource must supply the value.
	// +kubebuilder:default=false
	// +optional
	Required bool `json:"required,omitempty"`

	// minimum is an inclusive lower bound for integer values.
	// +optional
	Minimum *int64 `json:"minimum,omitempty"`

	// maximum is an inclusive upper bound for integer values.
	// +optional
	Maximum *int64 `json:"maximum,omitempty"`
}

// ResourceTemplate describes one managed resource to create.
type ResourceTemplate struct {
	// name identifies the template within the definition.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	Name string `json:"name"`

	// apiVersion of the managed resource to create.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`

	// kind of the managed resource to create.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// base is an optional static fragment merged into the managed resource.
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Base *apiextensionsv1.JSON `json:"base,omitempty"`

	// mappings copy Resource values or CloudTarget fields into the managed resource.
	// +listType=map
	// +listMapKey=to
	// +optional
	Mappings []FieldMapping `json:"mappings,omitempty"`
}

// FieldMapping copies a value into a field of a managed resource.
type FieldMapping struct {
	// from is a source path: values.<path> or target.<path>.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	From string `json:"from"`

	// to is a path on the managed resource, e.g. spec.forProvider.region.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	To string `json:"to"`
}

// ResourceOutput maps a managed resource field to Resource.status.outputs.
type ResourceOutput struct {
	// name is the output key exposed on Resource.status.outputs.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// from is a path on the composed resource, e.g. status.atProvider.arn.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	From string `json:"from"`
}

// ResourceDefinitionStatus defines the observed state of ResourceDefinition.
type ResourceDefinitionStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the ResourceDefinition resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// ResourceDefinition is the Schema for the resourcedefinitions API
type ResourceDefinition struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ResourceDefinition
	// +required
	Spec ResourceDefinitionSpec `json:"spec"`

	// status defines the observed state of ResourceDefinition
	// +optional
	Status ResourceDefinitionStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ResourceDefinitionList contains a list of ResourceDefinition
type ResourceDefinitionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ResourceDefinition `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ResourceDefinition{}, &ResourceDefinitionList{})
		return nil
	})
}
