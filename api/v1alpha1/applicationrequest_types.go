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

// ApplicationRequestPhase is the lifecycle phase of a GitOps request.
type ApplicationRequestPhase string

const (
	ApplicationRequestAccepted          ApplicationRequestPhase = "Accepted"
	ApplicationRequestProcessing        ApplicationRequestPhase = "Processing"
	ApplicationRequestBranchPushed      ApplicationRequestPhase = "BranchPushed"
	ApplicationRequestChangeRequestOpen ApplicationRequestPhase = "ChangeRequestOpen"
	ApplicationRequestConflict          ApplicationRequestPhase = "Conflict"
	ApplicationRequestRejected          ApplicationRequestPhase = "Rejected"
)

// ApplicationRequestSpec contains the Application desired state waiting for the GitOps bridge.
type ApplicationRequestSpec struct {
	// projectRef identifies the Project that owns the target repository.
	// +kubebuilder:validation:Required
	ProjectRef ProjectRequestReference `json:"projectRef"`

	// applicationName is the requested Application name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=53
	ApplicationName string `json:"applicationName"`

	// applicationNamespace is the target Application namespace.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ApplicationNamespace string `json:"applicationNamespace"`

	// requestedBy is the authenticated dashboard user.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	RequestedBy string `json:"requestedBy"`

	// idempotencyKey identifies repeated submissions of the same request.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	IdempotencyKey string `json:"idempotencyKey"`

	// application is the Application spec to write to the Project repository.
	// +kubebuilder:validation:Required
	Application ApplicationSpec `json:"application"`
}

// ProjectRequestReference identifies a Project by name.
type ProjectRequestReference struct {
	// name is the Project name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ApplicationRequestStatus reports the request lifecycle.
type ApplicationRequestStatus struct {
	// phase is the current request phase.
	// +optional
	Phase ApplicationRequestPhase `json:"phase,omitempty"`
	// message is a human-readable status message.
	// +optional
	Message string `json:"message,omitempty"`
	// branch is the Git branch created for this request.
	// +optional
	Branch string `json:"branch,omitempty"`
	// commitSHA is the commit written to the request branch.
	// +optional
	CommitSHA string `json:"commitSHA,omitempty"`
	// changeRequestURL is the pull request URL opened by the bridge.
	// +optional
	ChangeRequestURL string `json:"changeRequestURL,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=.status.phase
// +kubebuilder:printcolumn:name="Branch",type=string,JSONPath=.status.branch
// +kubebuilder:printcolumn:name="PR",type=string,JSONPath=.status.changeRequestURL

// ApplicationRequest is a durable GitOps request waiting for the bridge.
type ApplicationRequest struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the requested Application.
	// +required
	Spec ApplicationRequestSpec `json:"spec"`

	// status reports the request lifecycle.
	// +optional
	Status ApplicationRequestStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ApplicationRequestList contains a list of ApplicationRequest.
type ApplicationRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ApplicationRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ApplicationRequest{}, &ApplicationRequestList{})
		return nil
	})
}
