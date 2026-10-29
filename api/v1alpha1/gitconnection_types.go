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

// GitProvider selects a Git provider implementation.
type GitProvider string

const (
	// GitProviderGitHub is the GitHub implementation.
	GitProviderGitHub GitProvider = "github"
)

// GitConnectionSpec defines the desired state of GitConnection.
// +kubebuilder:validation:XValidation:rule="self.provider != 'github' || has(self.github)",message="github configuration is required when provider is github"
type GitConnectionSpec struct {
	// provider selects the Git provider implementation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=github
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="provider is immutable"
	Provider GitProvider `json:"provider"`

	// tokenSecretRef names the Secret containing the provider token in key `token`.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="token secret reference is immutable"
	TokenSecretRef GitTokenSecretReference `json:"tokenSecretRef"`

	// github configures the GitHub provider. It is required when provider is github.
	// +optional
	GitHub *GitHubProviderConfig `json:"github,omitempty"`
}

// GitTokenSecretReference names the Secret containing a provider token.
type GitTokenSecretReference struct {
	// name is the name of the Secret containing key `token`.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// GitHubProviderConfig configures the GitHub provider.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="github configuration is immutable"
type GitHubProviderConfig struct {
	// organization is the GitHub user or organization that owns created repositories.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=39
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`
	Organization string `json:"organization"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=gitconn

// GitConnection is the Schema for the gitconnections API.
type GitConnection struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of GitConnection
	// +required
	Spec GitConnectionSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// GitConnectionList contains a list of GitConnection.
type GitConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []GitConnection `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &GitConnection{}, &GitConnectionList{})
		return nil
	})
}
