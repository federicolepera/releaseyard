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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	"github.com/federicolepera/idp/internal/chartfetch"
	"github.com/federicolepera/idp/internal/contract"
	"github.com/federicolepera/idp/internal/helmrelease"
	"github.com/federicolepera/idp/internal/releaseclaim"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	applicationFinalizer = "platform.releaseyard.io/application-release"
	applicationRequeue   = 2 * time.Minute
	staleInputRequeue    = time.Second

	applicationWorkloadDeploymentKind  = "Deployment"
	applicationWorkloadReplicaSetKind  = "ReplicaSet"
	applicationWorkloadPodKind         = "Pod"
	applicationWorkloadStatefulSetKind = "StatefulSet"
	applicationWorkloadDaemonSetKind   = "DaemonSet"
	applicationWorkloadJobKind         = "Job"
	applicationWorkloadCronJobKind     = "CronJob"
	appsAPIVersion                     = "apps/v1"
	batchAPIVersion                    = "batch/v1"
	coreAPIVersion                     = "v1"
	applicationPodTerminatingReason    = "PodTerminating"
	applicationObservationUnavailable  = "ObservationUnavailable"
	applicationNoWorkloadsReason       = "NoWorkloads"
	applicationReleaseAbsentCondition  = "ReleaseAbsent"
	applicationWorkloadDegraded        = "Degraded"
	applicationWorkloadProgressing     = "Progressing"
	applicationWorkloadHealthy         = "Healthy"
	applicationWorkloadUnknown         = "Unknown"
	applicationWorkloadTrue            = "True"
	applicationWorkloadFalse           = "False"
	applicationReadyConditionType      = "Ready"
	applicationSpecField               = "spec"
	applicationObservedGenerationField = "observedGeneration"
	applicationReasonField             = "reason"
	applicationStatusField             = "status"
	applicationConditionsField         = "conditions"
	applicationPhaseField              = "phase"
	applicationReadyReplicasField      = "readyReplicas"
	applicationUpdatedReplicasField    = "updatedReplicas"
	applicationResourceMissingReason   = "ResourceMissing"
	applicationMalformedStatusReason   = "MalformedStatus"
	applicationJobRunningReason        = "Running"
	applicationJobCompleteReason       = "Complete"
	applicationJobFinishingReason      = "Finishing"
	applicationJobSuspendedReason      = "Suspended"
	applicationJobPausedReason         = "Paused"
	applicationJobFailedConditionType  = "Failed"
	applicationJobFailureTargetType    = "FailureTarget"
	applicationJobSuccessCriteriaType  = "SuccessCriteriaMet"
)

var errStaleApplicationSnapshot = errors.New("application snapshot is stale")

type verifiedChartPuller interface {
	PullVerified(context.Context, platformv1alpha1.ApplicationDefinitionChart) ([]byte, string, error)
}

type applicationHelm interface {
	Apply(context.Context, helmrelease.Request) (helmrelease.Result, error)
	Uninstall(context.Context, helmrelease.Request) (helmrelease.Result, error)
}

type releaseClaimManager interface {
	Acquire(context.Context, releaseclaim.Identity) (releaseclaim.Claim, error)
	Verify(context.Context, releaseclaim.Identity, releaseclaim.Claim) error
	Delete(context.Context, releaseclaim.Identity, releaseclaim.Claim) error
}

// ApplicationReconciler reconciles management-side Applications and remote releases.
type ApplicationReconciler struct {
	Client               client.Client
	APIReader            client.Reader
	CredentialsNamespace string
	// TeamNamespacePrefix is stripped from an Application's management namespace
	// to derive the team token used in the workload release name.
	TeamNamespacePrefix string
	Scheme              *runtime.Scheme
	Puller              verifiedChartPuller
	Recorder            record.EventRecorder

	HelmFactory         func(genericclioptions.RESTClientGetter) applicationHelm
	ClaimManagerFactory func(kubernetes.Interface) releaseClaimManager
	ResourceCollector   func(context.Context, *rest.Config, string, string) ([]platformv1alpha1.ApplicationResource, error)
}

// releaseName derives the workload Helm release name from the Application's
// team namespace and name, so app names are unique per team on a shared
// workload namespace.
func (r *ApplicationReconciler) releaseName(app *platformv1alpha1.Application) string {
	return contract.EffectiveReleaseName(app.Namespace, app.Name, r.TeamNamespacePrefix)
}

// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=applications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=applications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=applications/finalizers,verbs=update
// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=applicationdefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *ApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var app platformv1alpha1.Application
	if err := r.getAuthoritative(ctx, req.NamespacedName, &app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !app.DeletionTimestamp.IsZero() {
		if app.Spec.Environment != nil {
			return r.reconcileEnvironmentDelete(ctx, &app)
		}
		return r.reconcileDelete(ctx, &app)
	}
	if !slices.Contains(app.Finalizers, applicationFinalizer) {
		app.Finalizers = append(app.Finalizers, applicationFinalizer)
		if err := r.Client.Update(ctx, &app); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: staleInputRequeue}, nil
	}
	if app.Spec.Environment != nil {
		return r.reconcileEnvironmentApply(ctx, &app)
	}
	return r.reconcileApply(ctx, &app)
}

func (r *ApplicationReconciler) reconcileApply(ctx context.Context, app *platformv1alpha1.Application) (ctrl.Result, error) {
	attemptedBefore := app.Status.LastAttemptedIdentity != ""
	definition, cluster, cfg, target, namespace, values, digest, err := r.resolve(ctx, app)
	if err != nil {
		return r.fail(ctx, app, "DependencyNotReady", "Application dependencies are unavailable or invalid")
	}
	chartIdentity := platformv1alpha1.ApplicationChartIdentity{
		Repository: definition.Spec.Chart.Repository, Name: definition.Spec.Chart.Name,
		Version: definition.Spec.Chart.Version, Digest: digest,
	}
	identity, err := helmrelease.DesiredIdentity(chartIdentity.Repository, chartIdentity.Name, chartIdentity.Version, digest, values)
	if err != nil {
		return r.fail(ctx, app, "InvalidValues", "Application values could not be prepared")
	}
	archive, actualDigest, failureReason, err := r.prepareApplicationAttempt(ctx, app, definition, cluster, target, namespace, chartIdentity)
	if errors.Is(err, errStaleApplicationSnapshot) {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	switch failureReason {
	case "BindingConflict":
		return r.fail(ctx, app, failureReason, "Application target or chart identity conflicts with its recorded binding")
	case "ChartUnavailable":
		return r.fail(ctx, app, failureReason, "Verified chart retrieval is not configured")
	case "ChartDigestChanged":
		return r.fail(ctx, app, failureReason, "A previously observed chart coordinate has different archive bytes")
	case "ChartVerificationFailed":
		return r.fail(ctx, app, failureReason, "The verified chart does not match the ApplicationDefinition")
	}
	if !r.currentApplication(ctx, app, definition, cluster) {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, nil
	}

	remote, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return r.fail(ctx, app, "RemoteUnavailable", "Target cluster is unavailable")
	}
	manager := r.claimManager(remote)
	claimIdentity := claimIdentityFor(app, app.Status.Binding)
	claim := releaseclaim.Claim{
		Namespace: app.Status.Binding.ReleaseNamespace,
		Name:      releaseclaim.ClaimName(app.Status.Binding.ReleaseName),
		UID:       app.Status.Binding.RemoteClaimUID,
	}
	if claim.UID == "" {
		claim, err = manager.Acquire(ctx, claimIdentity)
		if err != nil {
			return r.fail(ctx, app, "OwnershipConflict", "Remote release ownership could not be established")
		}
		app.Status.Binding.RemoteClaimUID = claim.UID
		if err := r.persistStatus(ctx, app, app.Generation); err != nil {
			if errors.Is(err, errStaleApplicationSnapshot) {
				return ctrl.Result{RequeueAfter: staleInputRequeue}, nil
			}
			return ctrl.Result{}, err
		}
	} else if err := manager.Verify(ctx, claimIdentity, claim); err != nil {
		return r.fail(ctx, app, "OwnershipConflict", "Recorded remote release ownership could not be verified")
	}
	if err := manager.Verify(ctx, claimIdentity, claim); err != nil || !r.currentApplication(ctx, app, definition, cluster) {
		return r.fail(ctx, app, "StaleInputs", "Application inputs changed before the remote operation")
	}

	if !r.currentApplication(ctx, app, definition, cluster) {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, nil
	}
	if app.Status.LastAttemptedIdentity != identity {
		app.Status.LastAttemptedIdentity = identity
		if err := r.persistStatus(ctx, app, app.Generation); err != nil {
			if errors.Is(err, errStaleApplicationSnapshot) {
				return ctrl.Result{RequeueAfter: staleInputRequeue}, nil
			}
			return ctrl.Result{}, err
		}
	}
	if !r.currentApplication(ctx, app, definition, cluster) {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, nil
	}
	installer := r.helm(cfg, app.Status.Binding.ReleaseNamespace)
	request := helmrelease.Request{
		Namespace: app.Status.Binding.ReleaseNamespace, ReleaseName: app.Status.Binding.ReleaseName,
		Repository: chartIdentity.Repository, ChartName: chartIdentity.Name, ChartVersion: chartIdentity.Version,
		Digest: actualDigest, Archive: archive, Values: values, ApplicationUID: string(app.UID),
		ClaimUID: string(claim.UID), PreviousAttempt: attemptedBefore,
	}
	result, err := installer.Apply(ctx, request)
	if err != nil {
		return r.fail(ctx, app, "ReleaseOperationFailed", "Helm release operation failed and may require recovery")
	}
	applied := &platformv1alpha1.ApplicationAppliedIdentity{
		ApplicationDefinitionGeneration: definition.Generation,
		ApplicationGeneration:           app.Generation, Chart: chartIdentity,
		DesiredInputIdentity: result.DesiredIdentity, HelmRevision: int64(result.HelmRevision),
	}
	if err := manager.Verify(ctx, claimIdentity, claim); err != nil {
		return r.recordSupersededSuccess(ctx, app, applied, "OwnershipChanged", "Release succeeded but its ownership claim changed")
	}
	if !r.currentApplication(ctx, app, definition, cluster) {
		return r.recordSupersededSuccess(ctx, app, applied, "StaleOperation", "Release succeeded for inputs that are no longer current")
	}
	app.Status.Applied = applied
	app.Status.ObservedGeneration = app.Generation
	resources, inventoryErr := r.collectResources(ctx, cfg, result.Manifest, app.Status.Binding.ReleaseNamespace)
	if inventoryErr != nil {
		setInventoryCondition(app, metav1.ConditionFalse, "CollectionFailed", "Application resource inventory could not be refreshed")
		setWorkloadCondition(app, metav1.ConditionUnknown, applicationObservationUnavailable, "Workload health could not be observed")
		ctrl.LoggerFrom(ctx).Info("Application resource inventory collection failed")
	} else {
		app.Status.Resources = resources
		now := metav1.Now()
		app.Status.WorkloadObservedAt = &now
		status, reason, message := aggregateWorkloadHealth(resources)
		setWorkloadCondition(app, status, reason, message)
		setInventoryCondition(app, metav1.ConditionTrue, "Collected", "Application resource inventory was refreshed")
	}
	if changed := setApplicationCondition(app, metav1.ConditionTrue, "Ready", "Application release is ready"); changed {
		r.recordEvent(app, corev1.EventTypeNormal, "Ready", fmt.Sprintf("Applied chart %s %s (release %s revision %d)", chartIdentity.Name, chartIdentity.Version, app.Status.Binding.ReleaseName, applied.HelmRevision))
	}
	if err := r.persistStatus(ctx, app, app.Generation); err != nil {
		if errors.Is(err, errStaleApplicationSnapshot) {
			return r.recordSupersededSuccess(ctx, app, applied, "StaleOperation", "Release succeeded for inputs that are no longer current")
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: applicationRequeue}, nil
}

func (r *ApplicationReconciler) collectResources(ctx context.Context, cfg *rest.Config, manifest, namespace string) ([]platformv1alpha1.ApplicationResource, error) {
	if r.ResourceCollector != nil {
		return r.ResourceCollector(ctx, cfg, manifest, namespace)
	}
	return collectApplicationResources(ctx, cfg, manifest, namespace)
}

// prepareApplicationAttempt persists the binding and immutable chart pin before
// creating a remote claim. The caller records an attempt only after ownership
// is verified. Failure reasons are generic and safe for status.
func (r *ApplicationReconciler) prepareApplicationAttempt(ctx context.Context, app *platformv1alpha1.Application, definition *platformv1alpha1.ApplicationDefinition, cluster *platformv1alpha1.Cluster, target remoteTargetIdentity, namespace string, chartIdentity platformv1alpha1.ApplicationChartIdentity) ([]byte, string, string, error) {
	if app.Status.Binding != nil {
		if err := r.validateBoundInputs(app, definition, cluster, target, namespace, chartIdentity); err != nil {
			return nil, "", "BindingConflict", nil
		}
	} else {
		app.Status.Binding = &platformv1alpha1.ApplicationBinding{
			ApplicationDefinitionUID: definition.UID, ClusterUID: cluster.UID,
			TargetAPIServer: target.Server, TargetTrustHash: trustHash(target.TrustSHA256),
			ReleaseNamespace: namespace, ReleaseName: r.releaseName(app),
		}
		if err := r.persistStatus(ctx, app, app.Generation); err != nil {
			return nil, "", "", err
		}
	}
	if r.Puller == nil {
		return nil, "", "ChartUnavailable", nil
	}
	archive, actualDigest, err := r.Puller.PullVerified(ctx, definition.Spec.Chart)
	if err != nil || actualDigest != definition.Status.Digest || actualDigest != chartIdentity.Digest {
		return nil, "", "ChartVerificationFailed", nil
	}
	pinIndex := chartPinIndex(app.Status.ChartPins, chartIdentity)
	if pinIndex >= 0 && app.Status.ChartPins[pinIndex].Digest != chartIdentity.Digest {
		return nil, "", "ChartDigestChanged", nil
	}
	if pinIndex < 0 {
		app.Status.ChartPins = append(app.Status.ChartPins, chartIdentity)
		if err := r.persistStatus(ctx, app, app.Generation); err != nil {
			return nil, "", "", err
		}
	}
	return archive, actualDigest, "", nil
}

func (r *ApplicationReconciler) resolve(ctx context.Context, app *platformv1alpha1.Application) (*platformv1alpha1.ApplicationDefinition, *platformv1alpha1.Cluster, *rest.Config, remoteTargetIdentity, string, map[string]any, string, error) {
	if app.Spec.ClusterRef == nil || app.Spec.ClusterRef.Name == "" {
		return nil, nil, nil, remoteTargetIdentity{}, "", nil, "", errors.New("cluster reference is required for legacy mode")
	}
	var definition platformv1alpha1.ApplicationDefinition
	if err := r.getAuthoritative(ctx, types.NamespacedName{Name: app.Spec.ApplicationDefinitionRef.Name}, &definition); err != nil {
		return nil, nil, nil, remoteTargetIdentity{}, "", nil, "", err
	}
	condition := meta.FindStatusCondition(definition.Status.Conditions, "Validated")
	if condition == nil || condition.Status != metav1.ConditionTrue || definition.Status.ObservedGeneration != definition.Generation || condition.ObservedGeneration != definition.Generation || !validDefinitionDigest(definition.Status.Digest) {
		return nil, nil, nil, remoteTargetIdentity{}, "", nil, "", errors.New("definition is not validated")
	}
	var cluster platformv1alpha1.Cluster
	if err := r.getAuthoritative(ctx, types.NamespacedName{Name: app.Spec.ClusterRef.Name}, &cluster); err != nil {
		return nil, nil, nil, remoteTargetIdentity{}, "", nil, "", err
	}
	if r.APIReader == nil || r.CredentialsNamespace == "" {
		return nil, nil, nil, remoteTargetIdentity{}, "", nil, "", errors.New("credentials reader unavailable")
	}
	var secret corev1.Secret
	secretKey := types.NamespacedName{Namespace: r.CredentialsNamespace, Name: cluster.Spec.KubeconfigSecretRef.Name}
	if secretKey.Name == "" || r.APIReader.Get(ctx, secretKey, &secret) != nil || len(secret.Data["kubeconfig"]) == 0 {
		return nil, nil, nil, remoteTargetIdentity{}, "", nil, "", errors.New("credentials unavailable")
	}
	namespace, err := contract.EffectiveReleaseNamespace(&definition, app)
	if err != nil {
		return nil, nil, nil, remoteTargetIdentity{}, "", nil, "", err
	}
	values, err := contract.MergeValues(&definition, app)
	if err != nil {
		return nil, nil, nil, remoteTargetIdentity{}, "", nil, "", err
	}
	if app.Status.Binding != nil && (app.Status.Binding.ApplicationDefinitionUID != definition.UID || app.Status.Binding.ClusterUID != cluster.UID) {
		return nil, nil, nil, remoteTargetIdentity{}, "", nil, "", errors.New("bound object identity changed")
	}
	cfg, target, err := parseRemoteClusterConfig(secret.Data["kubeconfig"], namespace)
	if err != nil {
		return nil, nil, nil, remoteTargetIdentity{}, "", nil, "", err
	}
	if app.Status.Binding != nil && (app.Status.Binding.TargetAPIServer != target.Server || app.Status.Binding.TargetTrustHash != trustHash(target.TrustSHA256) || app.Status.Binding.ReleaseNamespace != namespace || app.Status.Binding.ReleaseName != r.releaseName(app)) {
		return nil, nil, nil, remoteTargetIdentity{}, "", nil, "", errors.New("bound target changed")
	}
	return &definition, &cluster, cfg, target, namespace, values, definition.Status.Digest, nil
}

func (r *ApplicationReconciler) reconcileDelete(ctx context.Context, app *platformv1alpha1.Application) (ctrl.Result, error) {
	if !slices.Contains(app.Finalizers, applicationFinalizer) {
		return ctrl.Result{}, nil
	}
	binding := app.Status.Binding
	if binding == nil {
		return r.removeFinalizer(ctx, app)
	}
	if app.Spec.ClusterRef == nil || app.Spec.ClusterRef.Name == "" {
		return r.fail(ctx, app, "CleanupUnavailable", "Bound target cluster reference is unavailable")
	}
	if r.APIReader == nil || r.CredentialsNamespace == "" {
		return r.fail(ctx, app, "CleanupUnavailable", "Remote release cleanup is waiting for credentials")
	}
	var cluster platformv1alpha1.Cluster
	if err := r.getAuthoritative(ctx, types.NamespacedName{Name: app.Spec.ClusterRef.Name}, &cluster); err != nil || cluster.UID != binding.ClusterUID {
		return r.fail(ctx, app, "CleanupUnavailable", "Bound target cluster is unavailable")
	}
	var secret corev1.Secret
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: r.CredentialsNamespace, Name: cluster.Spec.KubeconfigSecretRef.Name}, &secret); err != nil || len(secret.Data["kubeconfig"]) == 0 {
		return r.fail(ctx, app, "CleanupUnavailable", "Bound target credentials are unavailable")
	}
	cfg, target, err := parseRemoteClusterConfig(secret.Data["kubeconfig"], binding.ReleaseNamespace)
	if err != nil || target.Server != binding.TargetAPIServer || trustHash(target.TrustSHA256) != binding.TargetTrustHash {
		return r.fail(ctx, app, "CleanupUnavailable", "Bound target identity could not be verified")
	}
	remote, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return r.fail(ctx, app, "CleanupUnavailable", "Bound target is unavailable")
	}
	manager := r.claimManager(remote)
	identity := claimIdentityFor(app, binding)
	claim := releaseclaim.Claim{Namespace: binding.ReleaseNamespace, Name: releaseclaim.ClaimName(binding.ReleaseName), UID: binding.RemoteClaimUID}
	if cleanupComplete(app) {
		if claim.UID == "" {
			return r.fail(ctx, app, "CleanupConflict", "Recorded remote cleanup ownership is incomplete")
		}
		if err := manager.Delete(ctx, identity, claim); err != nil && !errors.Is(err, releaseclaim.ErrClaimNotFound) {
			return r.fail(ctx, app, "CleanupFailed", "Remote ownership claim cleanup failed")
		}
		return r.removeFinalizer(ctx, app)
	}
	if claim.UID == "" {
		claim, err = manager.Acquire(ctx, identity)
		if err != nil {
			return r.fail(ctx, app, "CleanupUnavailable", "Remote release ownership could not be checked")
		}
		bindingCopy := *binding
		bindingCopy.RemoteClaimUID = claim.UID
		app.Status.Binding = &bindingCopy
		if err := r.persistStatus(ctx, app, app.Generation); err != nil {
			if errors.Is(err, errStaleApplicationSnapshot) {
				return ctrl.Result{RequeueAfter: staleInputRequeue}, nil
			}
			return ctrl.Result{}, err
		}
	}
	if err := manager.Verify(ctx, identity, claim); err != nil {
		return r.fail(ctx, app, "CleanupConflict", "Remote release ownership could not be verified")
	}
	installer := r.helm(cfg, binding.ReleaseNamespace)
	_, err = installer.Uninstall(ctx, helmrelease.Request{
		Namespace: binding.ReleaseNamespace, ReleaseName: binding.ReleaseName,
		ApplicationUID: string(app.UID), ClaimUID: string(claim.UID),
	})
	if err != nil {
		return r.fail(ctx, app, "CleanupFailed", "Remote Helm release cleanup failed")
	}
	r.recordEvent(app, corev1.EventTypeNormal, "ReleaseDeleted", fmt.Sprintf("Uninstalled release %s from %s", binding.ReleaseName, binding.ReleaseNamespace))
	setCleanupComplete(app)
	if err := r.persistStatus(ctx, app, app.Generation); err != nil {
		if errors.Is(err, errStaleApplicationSnapshot) {
			return ctrl.Result{RequeueAfter: staleInputRequeue}, nil
		}
		return ctrl.Result{}, err
	}
	if err := manager.Delete(ctx, identity, claim); err != nil && !errors.Is(err, releaseclaim.ErrClaimNotFound) {
		return r.fail(ctx, app, "CleanupFailed", "Remote ownership claim cleanup failed")
	}
	return r.removeFinalizer(ctx, app)
}

func (r *ApplicationReconciler) fail(ctx context.Context, app *platformv1alpha1.Application, reason, message string) (ctrl.Result, error) {
	setWorkloadCondition(app, metav1.ConditionUnknown, applicationObservationUnavailable, "Workload health could not be observed")
	changed := setApplicationCondition(app, metav1.ConditionFalse, reason, message)
	if changed {
		r.recordEvent(app, corev1.EventTypeWarning, reason, message)
	}
	if err := r.persistStatus(ctx, app, app.Generation); err != nil {
		if errors.Is(err, errStaleApplicationSnapshot) || apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: staleInputRequeue}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: applicationRequeue}, nil
}

// recordSupersededSuccess records the exact release revision that did succeed,
// while retaining all newer desired-input status fields from the live object.
func (r *ApplicationReconciler) recordSupersededSuccess(ctx context.Context, attempted *platformv1alpha1.Application, applied *platformv1alpha1.ApplicationAppliedIdentity, reason, message string) (ctrl.Result, error) {
	var current platformv1alpha1.Application
	if err := r.getAuthoritative(ctx, types.NamespacedName{Namespace: attempted.Namespace, Name: attempted.Name}, &current); err != nil {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, client.IgnoreNotFound(err)
	}
	if current.UID != attempted.UID || !current.DeletionTimestamp.IsZero() {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, nil
	}
	current.Status.Applied = applied.DeepCopy()
	setWorkloadCondition(&current, metav1.ConditionUnknown, applicationObservationUnavailable, "Workload health could not be observed")
	if changed := setApplicationCondition(&current, metav1.ConditionFalse, reason, message); changed {
		r.recordEvent(&current, corev1.EventTypeWarning, reason, message)
	}
	if err := r.persistStatus(ctx, &current, current.Generation); err != nil {
		if errors.Is(err, errStaleApplicationSnapshot) || apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: staleInputRequeue}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: staleInputRequeue}, nil
}

func setWorkloadCondition(app *platformv1alpha1.Application, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{Type: "WorkloadHealthy", Status: status, ObservedGeneration: app.Generation, Reason: reason, Message: message, LastTransitionTime: metav1.Now()})
}

func aggregateWorkloadHealth(resources []platformv1alpha1.ApplicationResource) (metav1.ConditionStatus, string, string) {
	states := []string{}
	deploymentUIDs, statefulSetUIDs, daemonSetUIDs := map[types.UID]bool{}, map[types.UID]bool{}, map[types.UID]bool{}
	replicaSetDeploymentUIDs := map[types.UID]types.UID{}
	for _, r := range resources {
		switch {
		case r.APIVersion == appsAPIVersion && r.Kind == applicationWorkloadDeploymentKind:
			deploymentUIDs[r.UID] = true
			if r.Deployment != nil {
				states = append(states, r.Deployment.State)
			}
		case r.APIVersion == appsAPIVersion && r.Kind == applicationWorkloadStatefulSetKind && r.StatefulSet != nil:
			statefulSetUIDs[r.UID] = true
			states = append(states, r.StatefulSet.State)
		case r.APIVersion == appsAPIVersion && r.Kind == applicationWorkloadDaemonSetKind && r.DaemonSet != nil:
			daemonSetUIDs[r.UID] = true
			states = append(states, r.DaemonSet.State)
		case r.APIVersion == appsAPIVersion && r.Kind == applicationWorkloadReplicaSetKind:
			replicaSetDeploymentUIDs[r.UID] = r.ParentUID
		case r.APIVersion == batchAPIVersion && r.Kind == applicationWorkloadJobKind && r.Job != nil && r.ParentUID == "":
			states = append(states, r.Job.State)
		}
	}
	for _, r := range resources {
		if !podCoversWorkload(r, deploymentUIDs, statefulSetUIDs, daemonSetUIDs, replicaSetDeploymentUIDs) || r.Pod.Reason == applicationPodTerminatingReason {
			continue
		}
		states = append(states, r.Pod.State)
	}
	if len(states) == 0 {
		return metav1.ConditionUnknown, applicationNoWorkloadsReason, "No covered workloads were observed"
	}
	if slices.Contains(states, applicationWorkloadDegraded) {
		return metav1.ConditionFalse, applicationWorkloadDegraded, "One or more workloads are degraded"
	}
	if slices.Contains(states, applicationWorkloadUnknown) {
		return metav1.ConditionUnknown, applicationObservationUnavailable, "Workload health is unknown"
	}
	if slices.Contains(states, applicationWorkloadProgressing) {
		return metav1.ConditionFalse, applicationWorkloadProgressing, "One or more workloads are progressing"
	}
	return metav1.ConditionTrue, applicationWorkloadHealthy, "All covered workloads are healthy"
}

func podCoversWorkload(r platformv1alpha1.ApplicationResource, deploymentUIDs, statefulSetUIDs, daemonSetUIDs map[types.UID]bool, replicaSetDeploymentUIDs map[types.UID]types.UID) bool {
	if r.APIVersion != coreAPIVersion || r.Kind != applicationWorkloadPodKind || r.Pod == nil {
		return false
	}
	if r.ParentUID == "" {
		return true
	}
	if parent, ok := replicaSetDeploymentUIDs[r.ParentUID]; ok && deploymentUIDs[parent] {
		return true
	}
	return statefulSetUIDs[r.ParentUID] || daemonSetUIDs[r.ParentUID]
}

func (r *ApplicationReconciler) persistStatus(ctx context.Context, app *platformv1alpha1.Application, expectedGeneration int64) error {
	var current platformv1alpha1.Application
	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}
	if err := r.getAuthoritative(ctx, key, &current); err != nil {
		if apierrors.IsNotFound(err) {
			return errStaleApplicationSnapshot
		}
		return err
	}
	if current.UID != app.UID || current.Generation != expectedGeneration || current.ResourceVersion != app.ResourceVersion || !sameDeletionTimestamp(current.DeletionTimestamp, app.DeletionTimestamp) {
		return errStaleApplicationSnapshot
	}
	err := r.Client.Status().Update(ctx, app)
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return errStaleApplicationSnapshot
	}
	return err
}

func (r *ApplicationReconciler) removeFinalizer(ctx context.Context, app *platformv1alpha1.Application) (ctrl.Result, error) {
	var current platformv1alpha1.Application
	if err := r.getAuthoritative(ctx, types.NamespacedName{Namespace: app.Namespace, Name: app.Name}, &current); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if current.UID != app.UID || current.Generation != app.Generation || current.ResourceVersion != app.ResourceVersion || !sameDeletionTimestamp(current.DeletionTimestamp, app.DeletionTimestamp) || app.Status.Binding == nil && (current.Status.Binding != nil || cleanupComplete(&current) && !cleanupComplete(app)) {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, nil
	}
	current.Finalizers = removeString(current.Finalizers, applicationFinalizer)
	return ctrl.Result{}, r.Client.Update(ctx, &current)
}

func (r *ApplicationReconciler) helm(cfg *rest.Config, namespace string) applicationHelm {
	getter, _ := newRemoteRESTClientGetter(cfg, namespace)
	if r.HelmFactory != nil {
		return r.HelmFactory(getter)
	}
	return &helmrelease.Installer{Getter: getter}
}

func (r *ApplicationReconciler) claimManager(remote kubernetes.Interface) releaseClaimManager {
	if r.ClaimManagerFactory != nil {
		return r.ClaimManagerFactory(remote)
	}
	return releaseclaim.NewKubernetesManager(remote)
}

func (r *ApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.Application{}, builder.WithPredicates(applicationLifecyclePredicate{})).
		Watches(&platformv1alpha1.ApplicationDefinition{}, handler.EnqueueRequestsFromMapFunc(r.mapDefinition)).
		Watches(&platformv1alpha1.Cluster{}, handler.EnqueueRequestsFromMapFunc(r.mapCluster), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("application").
		Complete(r)
}

type applicationLifecyclePredicate struct{ predicate.Funcs }

func (applicationLifecyclePredicate) Create(event.CreateEvent) bool   { return true }
func (applicationLifecyclePredicate) Delete(event.DeleteEvent) bool   { return true }
func (applicationLifecyclePredicate) Generic(event.GenericEvent) bool { return true }
func (applicationLifecyclePredicate) Update(e event.UpdateEvent) bool {
	oldApp, oldOK := e.ObjectOld.(*platformv1alpha1.Application)
	newApp, newOK := e.ObjectNew.(*platformv1alpha1.Application)
	if !oldOK || !newOK {
		return true
	}
	return oldApp.Generation != newApp.Generation ||
		!oldApp.DeletionTimestamp.Equal(newApp.DeletionTimestamp) ||
		!equalStrings(oldApp.Finalizers, newApp.Finalizers)
}

func (r *ApplicationReconciler) mapDefinition(ctx context.Context, object client.Object) []reconcile.Request {
	var apps platformv1alpha1.ApplicationList
	if r.Client.List(ctx, &apps) != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range apps.Items {
		if apps.Items[i].Spec.ApplicationDefinitionRef.Name == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: apps.Items[i].Namespace, Name: apps.Items[i].Name}})
		}
	}
	return requests
}

func (r *ApplicationReconciler) mapCluster(ctx context.Context, object client.Object) []reconcile.Request {
	var apps platformv1alpha1.ApplicationList
	if r.Client.List(ctx, &apps) != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range apps.Items {
		app := &apps.Items[i]
		snapshotMatch := app.Spec.Environment != nil && slices.ContainsFunc(app.Spec.Environment.Targets, func(target platformv1alpha1.ApplicationEnvironmentTarget) bool {
			return target.Name == object.GetName()
		})
		legacyMatch := app.Spec.ClusterRef != nil && app.Spec.ClusterRef.Name == object.GetName()
		if snapshotMatch || legacyMatch {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: app.Namespace, Name: app.Name}})
		}
	}
	return requests
}

func (r *ApplicationReconciler) currentApplication(ctx context.Context, app *platformv1alpha1.Application, definition *platformv1alpha1.ApplicationDefinition, cluster *platformv1alpha1.Cluster) bool {
	var current platformv1alpha1.Application
	if r.getAuthoritative(ctx, types.NamespacedName{Namespace: app.Namespace, Name: app.Name}, &current) != nil || current.UID != app.UID || current.Generation != app.Generation || current.ResourceVersion != app.ResourceVersion || !sameDeletionTimestamp(current.DeletionTimestamp, app.DeletionTimestamp) || !current.DeletionTimestamp.IsZero() {
		return false
	}
	var currentDefinition platformv1alpha1.ApplicationDefinition
	if r.getAuthoritative(ctx, types.NamespacedName{Name: definition.Name}, &currentDefinition) != nil || currentDefinition.UID != definition.UID || currentDefinition.Generation != definition.Generation || currentDefinition.Status.Digest != definition.Status.Digest {
		return false
	}
	validated := meta.FindStatusCondition(currentDefinition.Status.Conditions, "Validated")
	if validated == nil || validated.Status != metav1.ConditionTrue || validated.ObservedGeneration != currentDefinition.Generation || currentDefinition.Status.ObservedGeneration != currentDefinition.Generation {
		return false
	}
	var currentCluster platformv1alpha1.Cluster
	return r.getAuthoritative(ctx, types.NamespacedName{Name: cluster.Name}, &currentCluster) == nil && currentCluster.UID == cluster.UID && currentCluster.Generation == cluster.Generation
}

func (r *ApplicationReconciler) getAuthoritative(ctx context.Context, key types.NamespacedName, object client.Object) error {
	if r.APIReader == nil {
		return errors.New("authoritative API reader is not configured")
	}
	return r.APIReader.Get(ctx, key, object)
}

func sameDeletionTimestamp(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(b)
}

func (r *ApplicationReconciler) validateBoundInputs(app *platformv1alpha1.Application, definition *platformv1alpha1.ApplicationDefinition, cluster *platformv1alpha1.Cluster, target remoteTargetIdentity, namespace string, chart platformv1alpha1.ApplicationChartIdentity) error {
	binding := app.Status.Binding
	if binding == nil || binding.ApplicationDefinitionUID != definition.UID || binding.ClusterUID != cluster.UID || binding.TargetAPIServer != target.Server || binding.TargetTrustHash != trustHash(target.TrustSHA256) || binding.ReleaseNamespace != namespace || binding.ReleaseName != r.releaseName(app) {
		return errors.New("bound identity mismatch")
	}
	for _, pin := range app.Status.ChartPins {
		if pin.Repository == chart.Repository && pin.Name == chart.Name && pin.Version == chart.Version && pin.Digest != chart.Digest {
			return errors.New("chart pin mismatch")
		}
	}
	return nil
}

func claimIdentityFor(app *platformv1alpha1.Application, binding *platformv1alpha1.ApplicationBinding) releaseclaim.Identity {
	return releaseclaim.Identity{
		ApplicationUID: app.UID, ManagementNamespace: app.Namespace, ManagementName: app.Name,
		ClusterUID: binding.ClusterUID, TargetAPIServer: binding.TargetAPIServer,
		TargetTrustFingerprint: binding.TargetTrustHash, ReleaseNamespace: binding.ReleaseNamespace,
		ReleaseName: binding.ReleaseName,
	}
}

func setApplicationCondition(app *platformv1alpha1.Application, status metav1.ConditionStatus, reason, message string) bool {
	changed := meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: status, ObservedGeneration: app.Generation,
		Reason: reason, Message: message, LastTransitionTime: metav1.Now(),
	})
	app.Status.ObservedGeneration = app.Generation
	return changed
}

// recordEvent emits a Kubernetes Event for the Application when a recorder is
// configured. The dashboard exposes these events as the per-object activity log.
func (r *ApplicationReconciler) recordEvent(app *platformv1alpha1.Application, eventType, reason, message string) {
	if r.Recorder == nil || app == nil {
		return
	}
	r.Recorder.Event(app, eventType, reason, message)
}

func setInventoryCondition(app *platformv1alpha1.Application, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type: "InventoryReady", Status: status, ObservedGeneration: app.Generation,
		Reason: reason, Message: message, LastTransitionTime: metav1.Now(),
	})
}

func setCleanupComplete(app *platformv1alpha1.Application) {
	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type: "CleanupComplete", Status: metav1.ConditionTrue, ObservedGeneration: app.Generation,
		Reason: applicationReleaseAbsentCondition, Message: "Remote Helm release is absent",
		LastTransitionTime: metav1.Now(),
	})
}

func cleanupComplete(app *platformv1alpha1.Application) bool {
	condition := meta.FindStatusCondition(app.Status.Conditions, "CleanupComplete")
	return condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == app.Generation
}

func validDefinitionDigest(digest string) bool {
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	return err == nil && len(decoded) == sha256.Size
}

func trustHash(sum [sha256.Size]byte) string { return "sha256:" + hex.EncodeToString(sum[:]) }

func chartPinIndex(pins []platformv1alpha1.ApplicationChartIdentity, wanted platformv1alpha1.ApplicationChartIdentity) int {
	for i := range pins {
		if pins[i].Repository == wanted.Repository && pins[i].Name == wanted.Name && pins[i].Version == wanted.Version {
			return i
		}
	}
	return -1
}

// Kept as a function value for compatibility with focused package tests.
var containsString = func(values []string, value string) bool { return slices.Contains(values, value) }

func removeString(values []string, value string) []string {
	result := values[:0]
	for _, item := range values {
		if item != value {
			result = append(result, item)
		}
	}
	return result
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var _ verifiedChartPuller = (*chartfetch.Puller)(nil)
