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

// ApplicationDefinitionChart identifies a pinned Helm chart.
type ApplicationDefinitionChart struct {
	// repository is the base URL of a ChartMuseum-compatible HTTP(S) Helm repository.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=8
	// +kubebuilder:validation:Pattern="^https?://.+$"
	Repository string `json:"repository"`

	// name is the chart name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$"
	Name string `json:"name"`

	// version is a pinned semantic version. Floating tags such as "latest" are not accepted.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=5
	// +kubebuilder:validation:Pattern="^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(\\+[0-9A-Za-z-]+(\\.[0-9A-Za-z-]+)*)?$"
	Version string `json:"version"`
}

// ApplicationDefinitionRelease configures the target namespace for chart releases.
type ApplicationDefinitionRelease struct {
	// namespace is created/used on the target workload cluster, not in the management cluster.
	// It is immutable after the ApplicationDefinition is created.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="release namespace is immutable"
	Namespace string `json:"namespace"`

	// allowNamespaceOverride permits Applications to select a different existing target namespace.
	// +kubebuilder:default=false
	// +optional
	AllowNamespaceOverride bool `json:"allowNamespaceOverride,omitempty"`
}

// ApplicationDefinitionValue exposes one developer-configurable chart value path.
// +kubebuilder:validation:XValidation:rule="!has(self.minimum) || self.type == 'integer'",message="minimum is only valid for integer values"
// +kubebuilder:validation:XValidation:rule="!has(self.maximum) || self.type == 'integer'",message="maximum is only valid for integer values"
// +kubebuilder:validation:XValidation:rule="!has(self.minimum) || !has(self.maximum) || self.minimum <= self.maximum",message="minimum must not exceed maximum"
type ApplicationDefinitionValue struct {
	// path is a dot-separated path; dots have no escaping syntax.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern="^[A-Za-z][A-Za-z0-9_-]*(\\.[A-Za-z][A-Za-z0-9_-]*)*$"
	Path string `json:"path"`

	// type is the value type exposed to an Application.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=string;integer;boolean
	Type string `json:"type"`

	// required indicates whether an Application must supply this value. It defaults to false.
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

// ApplicationDefinitionSpec defines the desired state of ApplicationDefinition.
type ApplicationDefinitionSpec struct {
	// description is a short human-readable summary of the definition, shown in
	// the catalog and in the access definitions picker.
	// +kubebuilder:validation:MaxLength=280
	// +optional
	Description string `json:"description,omitempty"`

	// chart identifies the Helm chart used by Applications consuming this definition.
	// +kubebuilder:validation:Required
	Chart ApplicationDefinitionChart `json:"chart"`

	// release configures the target workload cluster namespace and release naming convention.
	// The eventual Helm release name is the consuming Application's metadata.name.
	// +kubebuilder:validation:Required
	Release ApplicationDefinitionRelease `json:"release"`

	// defaults is an optional arbitrary nested Helm values object.
	// Application value validation and Helm operations are future work; this API only stores the contract.
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Defaults *apiextensionsv1.JSON `json:"defaults,omitempty"`

	// configurableValues lists the only chart value paths an Application may eventually set.
	// Application validation and Helm operations are future work.
	// +listType=map
	// +listMapKey=path
	// +optional
	ConfigurableValues []ApplicationDefinitionValue `json:"configurableValues,omitempty"`
}

// ApplicationDefinitionStatus defines the observed state of ApplicationDefinition.
type ApplicationDefinitionStatus struct {
	// digest is the SHA-256 digest of the downloaded chart archive bytes.
	// +optional
	Digest string `json:"digest,omitempty"`

	// lastCheckedTime is when the current generation was last checked.
	// +optional
	LastCheckedTime metav1.Time `json:"lastCheckedTime,omitempty"`

	// observedGeneration is the generation used for the current status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current validation and chart-verification state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status

// ApplicationDefinition is the cluster-scoped management-cluster contract for an application chart.
// spec.release.namespace identifies the target namespace on a workload cluster.
type ApplicationDefinition struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ApplicationDefinition
	// +required
	Spec ApplicationDefinitionSpec `json:"spec"`

	// status defines the observed state of ApplicationDefinition
	// +optional
	Status ApplicationDefinitionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ApplicationDefinitionList contains a list of ApplicationDefinition
type ApplicationDefinitionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ApplicationDefinition `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ApplicationDefinition{}, &ApplicationDefinitionList{})
		return nil
	})
}
