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
	"k8s.io/apimachinery/pkg/types"
)

// ApplicationReference identifies an object by name.
type ApplicationReference struct {
	// name identifies the referenced resource.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$"
	Name string `json:"name"`
}

// ApplicationSpec defines the desired state of Application.
// +kubebuilder:validation:XValidation:rule="(!has(self.releaseNamespace) && !has(oldSelf.releaseNamespace)) || (has(self.releaseNamespace) && has(oldSelf.releaseNamespace) && self.releaseNamespace == oldSelf.releaseNamespace)",message="releaseNamespace is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.clusterRef) != has(self.environment)",message="exactly one of clusterRef or environment must be set"
// +kubebuilder:validation:XValidation:rule="(has(oldSelf.clusterRef) && has(self.clusterRef) && self.clusterRef == oldSelf.clusterRef && !has(self.environment) && !has(oldSelf.environment)) || (has(oldSelf.environment) && has(self.environment) && self.environment == oldSelf.environment && !has(self.clusterRef) && !has(oldSelf.clusterRef))",message="the Application target selector is immutable"
type ApplicationSpec struct {
	// applicationDefinitionRef identifies a cluster-scoped ApplicationDefinition.
	// Its name cannot be changed after creation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self.name == oldSelf.name",message="applicationDefinitionRef is immutable"
	ApplicationDefinitionRef ApplicationReference `json:"applicationDefinitionRef"`

	// clusterRef identifies the target cluster-scoped Cluster resource.
	// Its name cannot be changed after creation.
	// +optional
	ClusterRef *ApplicationReference `json:"clusterRef,omitempty"`
	// environment selects Clusters by their environment label. When targets is
	// omitted the matching set is resolved dynamically on every reconcile. Set
	// targets to pin a fixed snapshot instead.
	// +optional
	Environment *ApplicationEnvironmentSelection `json:"environment,omitempty"`

	// releaseNamespace selects an existing namespace on the target workload cluster.
	// When omitted, the ApplicationDefinition's release.namespace is used.
	// This value cannot be changed after the Application is created.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +optional
	ReleaseNamespace *string `json:"releaseNamespace,omitempty"`

	// values contains user-supplied overrides for paths exposed by the referenced definition.
	// Chart defaults are not read here.
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Values *apiextensionsv1.JSON `json:"values,omitempty"`
}

// ApplicationEnvironmentSelection selects an environment. An empty targets list
// makes the target set dynamic; a non-empty list pins it to those identities.
type ApplicationEnvironmentSelection struct {
	// name is the environment label value used to select target Clusters.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern="^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$"
	Name string `json:"name"`
	// targets optionally pins the Cluster identities for this Application. When
	// omitted or empty, the controller resolves the matching Clusters dynamically.
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	// +optional
	Targets []ApplicationEnvironmentTarget `json:"targets,omitempty"`
}

// ApplicationEnvironmentTarget pins one Cluster identity in an environment.
// +kubebuilder:validation:XValidation:rule="size(self.uid) >= 1 && size(self.uid) <= 128",message="uid must contain between 1 and 128 characters"
type ApplicationEnvironmentTarget struct {
	// name is the DNS subdomain name of the target Cluster.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$"
	Name string `json:"name"`
	// uid identifies the exact target Cluster object.
	// +kubebuilder:validation:Required
	UID types.UID `json:"uid"`
}

// ApplicationChartIdentity records the chart coordinate and verified archive digest.
type ApplicationChartIdentity struct {
	// repository is the configured ChartMuseum-compatible repository URL.
	// +kubebuilder:validation:Required
	Repository string `json:"repository"`
	// name is the chart name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// version is the pinned chart version.
	// +kubebuilder:validation:Required
	Version string `json:"version"`
	// digest is the SHA-256 digest of the verified archive bytes.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern="^sha256:[a-f0-9]{64}$"
	Digest string `json:"digest"`
}

// ApplicationBinding records target and ownership identities fixed before remote side effects.
type ApplicationBinding struct {
	// applicationDefinitionUID identifies the exact bound definition object.
	ApplicationDefinitionUID types.UID `json:"applicationDefinitionUID"`
	// clusterUID identifies the exact bound Cluster object.
	ClusterUID types.UID `json:"clusterUID"`
	// targetAPIServer is the immutable target API server URL.
	TargetAPIServer string `json:"targetAPIServer"`
	// targetTrustHash is the SHA-256 hash of the target cluster's TLS trust material.
	TargetTrustHash string `json:"targetTrustHash"`
	// releaseNamespace is the pre-existing target namespace for the Helm release.
	ReleaseNamespace string `json:"releaseNamespace"`
	// releaseName is the Helm release name derived from Application metadata.name.
	ReleaseName string `json:"releaseName"`
	// remoteClaimUID identifies the remote per-release ownership claim, when created.
	// +optional
	RemoteClaimUID types.UID `json:"remoteClaimUID,omitempty"`
}

// ApplicationEnvironmentBinding records definition and release namespace identity shared by environment instances.
type ApplicationEnvironmentBinding struct {
	ApplicationDefinitionUID types.UID `json:"applicationDefinitionUID"`
	ReleaseNamespace         string    `json:"releaseNamespace"`
}

// ApplicationInstanceStatus records the independent state of one environment target.
type ApplicationInstanceStatus struct {
	ClusterName           string                      `json:"clusterName"`
	ClusterUID            types.UID                   `json:"clusterUID"`
	Binding               *ApplicationBinding         `json:"binding,omitempty"`
	Applied               *ApplicationAppliedIdentity `json:"applied,omitempty"`
	LastAttemptedIdentity string                      `json:"lastAttemptedIdentity,omitempty"`
	ObservedGeneration    int64                       `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions         []metav1.Condition    `json:"conditions,omitempty"`
	Resources          []ApplicationResource `json:"resources,omitempty"`
	WorkloadObservedAt *metav1.Time          `json:"workloadObservedAt,omitempty"`
	CleanupComplete    bool                  `json:"cleanupComplete,omitempty"`
}

// ApplicationAppliedIdentity records the inputs applied by a successful Helm action.
type ApplicationAppliedIdentity struct {
	// applicationDefinitionGeneration is the definition generation used for the action.
	ApplicationDefinitionGeneration int64 `json:"applicationDefinitionGeneration"`
	// applicationGeneration is the Application generation used for the action.
	ApplicationGeneration int64 `json:"applicationGeneration"`
	// chart records the full chart coordinate and verified archive digest successfully applied.
	Chart ApplicationChartIdentity `json:"chart"`
	// desiredInputIdentity identifies the complete desired chart and values input.
	DesiredInputIdentity string `json:"desiredInputIdentity"`
	// helmRevision is the resulting Helm release revision.
	HelmRevision int64 `json:"helmRevision"`
}

// ApplicationResource records metadata for a resource rendered by the chart or one of its descendants.
// It deliberately excludes resource specifications and Secret data.
type ApplicationResource struct {
	// apiVersion is the API version of the observed resource.
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`
	// kind is the kind of the observed resource.
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`
	// namespace is the namespace of the observed resource, when namespaced.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// name is the name of the observed resource.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// uid is the UID of the observed resource, when available.
	// +optional
	UID types.UID `json:"uid,omitempty"`
	// parentUID identifies the observed parent resource, when this resource is a descendant.
	// +optional
	ParentUID types.UID `json:"parentUID,omitempty"`
	// deployment contains safe health details for an observed Deployment.
	// +optional
	Deployment *ApplicationDeploymentHealth `json:"deployment,omitempty"`
	// pod contains safe health details for an observed Pod.
	// +optional
	Pod *ApplicationPodHealth `json:"pod,omitempty"`
	// statefulSet summarizes health for an observed StatefulSet.
	// +optional
	StatefulSet *ApplicationStatefulSetHealth `json:"statefulSet,omitempty"`
	// daemonSet summarizes health for an observed DaemonSet.
	// +optional
	DaemonSet *ApplicationDaemonSetHealth `json:"daemonSet,omitempty"`
	// job summarizes health for an observed Job.
	// +optional
	Job *ApplicationJobHealth `json:"job,omitempty"`
}

// ApplicationDeploymentHealth summarizes rollout health without exposing the Deployment spec.
type ApplicationDeploymentHealth struct {
	DesiredReplicas int32  `json:"desiredReplicas"`
	ReadyReplicas   int32  `json:"readyReplicas"`
	State           string `json:"state"`
	Reason          string `json:"reason,omitempty"`
}

// ApplicationPodHealth summarizes readiness without exposing raw Kubernetes status messages.
type ApplicationPodHealth struct {
	Ready   string `json:"ready"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// ApplicationStatefulSetHealth summarizes StatefulSet readiness.
type ApplicationStatefulSetHealth struct {
	DesiredReplicas *int32 `json:"desiredReplicas,omitempty"`
	ReadyReplicas   *int32 `json:"readyReplicas,omitempty"`
	State           string `json:"state"`
	Reason          string `json:"reason,omitempty"`
}

// ApplicationDaemonSetHealth summarizes DaemonSet scheduling and readiness.
type ApplicationDaemonSetHealth struct {
	DesiredNumberScheduled *int32 `json:"desiredNumberScheduled,omitempty"`
	NumberReady            *int32 `json:"numberReady,omitempty"`
	State                  string `json:"state"`
	Reason                 string `json:"reason,omitempty"`
}

// ApplicationJobHealth summarizes Job completion without exposing raw status messages.
type ApplicationJobHealth struct {
	Completions *int32 `json:"completions,omitempty"`
	Active      *int32 `json:"active,omitempty"`
	Succeeded   *int32 `json:"succeeded,omitempty"`
	Failed      *int32 `json:"failed,omitempty"`
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"`
}

// ApplicationStatus defines the observed state of Application.
type ApplicationStatus struct {
	// observedGeneration is the Application generation represented by this status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// binding records immutable target and remote ownership identities.
	// +optional
	Binding *ApplicationBinding `json:"binding,omitempty"`
	// applied records the identities of the last successful Helm action.
	// +optional
	Applied *ApplicationAppliedIdentity `json:"applied,omitempty"`
	// chartPins permanently records the first verified digest for every encountered chart coordinate.
	// +listType=map
	// +listMapKey=repository
	// +listMapKey=name
	// +listMapKey=version
	// +optional
	ChartPins []ApplicationChartIdentity `json:"chartPins,omitempty"`
	// lastAttemptedIdentity identifies the last desired input attempted by reconciliation.
	// +optional
	LastAttemptedIdentity string `json:"lastAttemptedIdentity,omitempty"`
	// conditions represent the current state of the Application resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// resources contains observed chart resources and descendant metadata only; it excludes specs and Secret data.
	// +optional
	Resources []ApplicationResource `json:"resources,omitempty"`
	// workloadObservedAt is the time of the last successful workload health observation.
	// +optional
	WorkloadObservedAt *metav1.Time `json:"workloadObservedAt,omitempty"`
	// environmentBinding records shared environment-mode identity, without changing legacy singular fields.
	// +optional
	EnvironmentBinding *ApplicationEnvironmentBinding `json:"environmentBinding,omitempty"`
	// instances contains one independent status entry per resolved target Cluster UID.
	// +listType=map
	// +listMapKey=clusterUID
	// +optional
	Instances []ApplicationInstanceStatus `json:"instances,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 53 && self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="Application name must be a DNS label of at most 53 characters for Helm release naming"

// Application is the Schema for the applications API
type Application struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Application
	// +required
	Spec ApplicationSpec `json:"spec"`

	// status defines the observed state of Application
	// +optional
	Status ApplicationStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ApplicationList contains a list of Application
type ApplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Application `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Application{}, &ApplicationList{})
		return nil
	})
}
