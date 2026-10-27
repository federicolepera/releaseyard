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

// ProjectConditionRepositoryReady indicates that the Git repository exists and is recorded in status.
const ProjectConditionRepositoryReady = "RepositoryReady"

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ProjectSpec defines the desired state of Project
type ProjectSpec struct {
	// repository defines the repository to create and its connection.
	// +kubebuilder:validation:Required
	Repository ProjectRepositorySpec `json:"repository"`

	// description is attached to the provisioned repository.
	// +optional
	// +kubebuilder:validation:MaxLength=280
	Description string `json:"description,omitempty"`
}

// ProjectRepositorySpec describes the repository to provision for a Project.
type ProjectRepositorySpec struct {
	// connectionRef identifies the GitConnection used to provision this project.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="connection reference is immutable"
	ConnectionRef GitConnectionReference `json:"connectionRef"`

	// name is the repository name. It defaults to the Project name when empty.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=100
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="repository name is immutable"
	Name string `json:"name,omitempty"`
}

// GitConnectionReference identifies a namespaced GitConnection.
type GitConnectionReference struct {
	// name is the name of the referenced GitConnection.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// namespace is the GitConnection namespace. It defaults to the manager
	// credentials namespace when empty.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace,omitempty"`
}

// ProjectStatus defines the observed state of Project.
type ProjectStatus struct {
	// repository identifies the Git repository created for this Project.
	// +optional
	Repository *ProjectRepositoryStatus `json:"repository,omitempty"`

	// provisioning records the durable checkpoint used to make repository creation fail closed.
	// +optional
	Provisioning *ProjectProvisioningStatus `json:"provisioning,omitempty"`

	// conditions represent the current state of the Project resource. The
	// RepositoryReady condition indicates whether the GitHub repository has been
	// provisioned and verified.
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

// ProjectRepositoryStatus describes the repository provisioned for a Project.
type ProjectRepositoryStatus struct {
	// id is the stable provider repository ID.
	ID int64 `json:"id"`
	// owner is the provider account that owns the repository.
	Owner string `json:"owner"`
	// name is the repository name.
	Name string `json:"name"`
	// url is the canonical web URL.
	URL string `json:"url"`
	// cloneURL is the HTTPS clone URL.
	CloneURL string `json:"cloneURL"`
	// sshURL is the SSH clone URL.
	SSHURL string `json:"sshURL"`
	// defaultBranch is the repository default branch.
	DefaultBranch string `json:"defaultBranch"`
	// fullName is the owner/name repository identifier.
	// +optional
	FullName string `json:"fullName,omitempty"`
}

// ProjectProvisioningStatus stores identity and POST-attempt state before repository creation.
type ProjectProvisioningStatus struct {
	// connectionUID is the UID of the GitConnection used for the attempt.
	// +optional
	ConnectionUID string `json:"connectionUID,omitempty"`
	// owner is the target provider owner.
	// +optional
	Owner string `json:"owner,omitempty"`
	// repo is the target repository name.
	// +optional
	Repo string `json:"repo,omitempty"`
	// attempted is true once a repository-creation POST may have been sent.
	// +optional
	Attempted bool `json:"attempted,omitempty"`
	// repositoryID is the stable provider repository ID, once known.
	// +optional
	RepositoryID int64 `json:"repositoryID,omitempty"`
	// retryAfter prevents a status change from triggering an immediate retry
	// after a definite provider rejection.
	// +optional
	RetryAfter *metav1.Time `json:"retryAfter,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// Project is the Schema for the projects API
type Project struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Project
	// +required
	Spec ProjectSpec `json:"spec"`

	// status defines the observed state of Project
	// +optional
	Status ProjectStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ProjectList contains a list of Project
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Project `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Project{}, &ProjectList{})
		return nil
	})
}
