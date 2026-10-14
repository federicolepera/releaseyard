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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ClusterEnvironmentLabelKey is the Cluster metadata label used for environment selection.
const ClusterEnvironmentLabelKey = "platform.releaseyard.io/environment"

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ClusterSpec defines the desired state of Cluster
type ClusterSpec struct {
	// kubeconfigSecretRef names the Secret containing this cluster's kubeconfig.
	// The Secret is read from the manager's credentials namespace and must contain
	// the kubeconfig in its data.kubeconfig key.
	// +kubebuilder:validation:Required
	KubeconfigSecretRef KubeconfigSecretReference `json:"kubeconfigSecretRef"`
}

// KubeconfigSecretReference identifies the Secret containing a target cluster kubeconfig.
type KubeconfigSecretReference struct {
	// name is the name of the Secret in the manager's credentials namespace.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$"
	Name string `json:"name"`
}

// ClusterNode summarizes the readiness of a node in the remote cluster.
type ClusterNode struct {
	// name is the node's name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// ready is the node's Ready condition status.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=True;False;Unknown
	Ready string `json:"ready"`
}

// ClusterStatus defines the observed state of Cluster.
type ClusterStatus struct {
	// lastProbeTime is the time of the most recent connection probe, which checks
	// whether the configured credentials can authenticate to the target cluster.
	// It does not imply that workloads or the cluster are healthy.
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`

	// nodes contains the latest name and Ready-condition summary for remote nodes.
	// +optional
	Nodes []ClusterNode `json:"nodes,omitempty"`

	// nodesObservedAt is the time the node inventory was last successfully refreshed.
	// +optional
	NodesObservedAt *metav1.Time `json:"nodesObservedAt,omitempty"`

	// conditions represent the observed state of the Cluster. A Connected condition
	// reports whether the latest connection probe could authenticate to the target.
	// NodesReady reports whether the node inventory was successfully refreshed.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// Cluster is the Schema for the clusters API
type Cluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Cluster
	// +required
	Spec ClusterSpec `json:"spec"`

	// status defines the observed state of Cluster
	// +optional
	Status ClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterList contains a list of Cluster
type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Cluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Cluster{}, &ClusterList{})
		return nil
	})
}
