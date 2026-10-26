package controller

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	"github.com/federicolepera/idp/internal/contract"
	"github.com/federicolepera/idp/internal/helmrelease"
	"github.com/federicolepera/idp/internal/releaseclaim"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	errEnvironmentTooManyTargets = errors.New("environment has too many matching Clusters")
)

func usesMultiTargetSelection(app *platformv1alpha1.Application) bool {
	return app.Spec.Environment != nil || len(app.Spec.Clusters) > 0
}

func explicitClusterTargets(app *platformv1alpha1.Application) []platformv1alpha1.ApplicationEnvironmentTarget {
	targets := make([]platformv1alpha1.ApplicationEnvironmentTarget, 0, len(app.Spec.Clusters))
	for _, cluster := range app.Spec.Clusters {
		targets = append(targets, platformv1alpha1.ApplicationEnvironmentTarget{Name: cluster.Name, UID: types.UID(cluster.UID)})
	}
	return targets
}

// reconcileEnvironmentApply resolves the environment target set serially. A
// non-empty spec.environment.targets list pins the set; an empty list follows
// the Cluster environment label dynamically. It deliberately does not share the
// legacy singular binding or release state.
//
//nolint:gocyclo // orchestration handler; splitting would obscure the sequential flow.
func (r *ApplicationReconciler) reconcileEnvironmentApply(ctx context.Context, app *platformv1alpha1.Application) (ctrl.Result, error) {
	if err := validateMultiTargetSelection(app); err != nil {
		return r.fail(ctx, app, "InvalidTargetSelector", "Application target selector is invalid")
	}
	targets, err := r.resolveMultiTargets(ctx, app)
	if err != nil {
		switch {
		case errors.Is(err, errEnvironmentTooManyTargets):
			return r.environmentFail(ctx, app, "TooManyEnvironmentTargets", "Target selector has more than 32 Clusters")
		default:
			return r.environmentFail(ctx, app, "TargetsUnavailable", "Application targets could not be resolved")
		}
	}
	result, stopped, err := r.reconcileEnvironmentRemovals(ctx, app, targets)
	if err != nil || stopped {
		return result, err
	}
	if len(targets) == 0 {
		if app.Spec.Environment != nil {
			return r.environmentFail(ctx, app, "NoEnvironmentTargets", "No Clusters match the environment")
		}
		return r.environmentFail(ctx, app, "NoClusterTargets", "No Clusters are selected")
	}
	var definition platformv1alpha1.ApplicationDefinition
	if err := r.getAuthoritative(ctx, types.NamespacedName{Name: app.Spec.ApplicationDefinitionRef.Name}, &definition); err != nil {
		return r.environmentFail(ctx, app, "DependencyNotReady", "Application dependencies are unavailable or invalid")
	}
	validated := meta.FindStatusCondition(definition.Status.Conditions, "Validated")
	if validated == nil || validated.Status != metav1.ConditionTrue || validated.ObservedGeneration != definition.Generation || definition.Status.ObservedGeneration != definition.Generation || !validDefinitionDigest(definition.Status.Digest) {
		return r.environmentFail(ctx, app, "DependencyNotReady", "Application dependencies are unavailable or invalid")
	}
	if err := contract.ValidateApplication(&definition, app); err != nil {
		return r.environmentFail(ctx, app, "InvalidApplication", "Application inputs are invalid")
	}
	if err := r.preflightEnvironmentTargets(ctx, targets); err != nil {
		return r.environmentFail(ctx, app, "TargetIdentityConflict", "An environment Cluster name now identifies a different object")
	}
	namespace, err := contract.EffectiveReleaseNamespace(&definition, app)
	if err != nil {
		return r.environmentFail(ctx, app, "InvalidApplication", "Application inputs are invalid")
	}
	values, err := contract.MergeValues(&definition, app)
	if err != nil {
		return r.environmentFail(ctx, app, "InvalidApplication", "Application inputs are invalid")
	}
	chart := platformv1alpha1.ApplicationChartIdentity{Repository: definition.Spec.Chart.Repository, Name: definition.Spec.Chart.Name, Version: definition.Spec.Chart.Version, Digest: definition.Status.Digest}
	desired, err := helmrelease.DesiredIdentity(chart.Repository, chart.Name, chart.Version, chart.Digest, values)
	if err != nil {
		return r.environmentFail(ctx, app, "InvalidApplication", "Application inputs are invalid")
	}
	if app.Status.Binding != nil {
		return r.environmentFail(ctx, app, "BindingConflict", "Legacy target binding conflicts with environment mode")
	}
	if app.Status.EnvironmentBinding != nil && (app.Status.EnvironmentBinding.ApplicationDefinitionUID != definition.UID || app.Status.EnvironmentBinding.ReleaseNamespace != namespace) {
		return r.environmentFail(ctx, app, "BindingConflict", "Environment binding conflicts with current inputs")
	}
	if r.Puller == nil {
		return r.environmentFail(ctx, app, "ChartUnavailable", "Verified chart retrieval is not configured")
	}
	archive, actualDigest, pullErr := r.Puller.PullVerified(ctx, definition.Spec.Chart)
	if pullErr != nil || actualDigest != chart.Digest {
		return r.environmentFail(ctx, app, "ChartVerificationFailed", "The verified chart does not match the ApplicationDefinition")
	}
	if pin := chartPinIndex(app.Status.ChartPins, chart); pin >= 0 && app.Status.ChartPins[pin].Digest != chart.Digest {
		return r.environmentFail(ctx, app, "ChartDigestChanged", "A previously observed chart coordinate has different archive bytes")
	}
	if app.Status.EnvironmentBinding == nil {
		app.Status.EnvironmentBinding = &platformv1alpha1.ApplicationEnvironmentBinding{ApplicationDefinitionUID: definition.UID, ReleaseNamespace: namespace}
		invalidateEnvironmentAggregates(app, "BindingPending", "Environment target bindings are being established")
		if err := r.persistStatus(ctx, app, app.Generation); err != nil {
			return environmentPersistFailure(err)
		}
	}
	if chartPinIndex(app.Status.ChartPins, chart) < 0 {
		app.Status.ChartPins = append(app.Status.ChartPins, chart)
		if err := r.persistStatus(ctx, app, app.Generation); err != nil {
			return environmentPersistFailure(err)
		}
	}
	targets, result, stopped, err = runEnvironmentTargets(targets, func(target platformv1alpha1.ApplicationEnvironmentTarget) (ctrl.Result, bool, error) {
		return r.applyEnvironmentTarget(ctx, app, &definition, target, namespace, values, chart, archive, desired)
	})
	if err != nil {
		return result, err
	}
	if stopped {
		return result, nil
	}
	if aggregateEnvironmentStatus(app, targets, desired, chart, definition.Generation) {
		r.recordEvent(app, corev1.EventTypeNormal, "Ready", "All environment releases are ready")
	}
	if err := r.persistStatus(ctx, app, app.Generation); err != nil {
		return environmentPersistFailure(err)
	}
	return ctrl.Result{RequeueAfter: applicationRequeue}, nil
}

func (r *ApplicationReconciler) resolveMultiTargets(ctx context.Context, app *platformv1alpha1.Application) ([]platformv1alpha1.ApplicationEnvironmentTarget, error) {
	if len(app.Spec.Clusters) > 0 {
		return explicitClusterTargets(app), nil
	}
	return r.resolveEnvironmentTargets(ctx, app)
}

func (r *ApplicationReconciler) resolveEnvironmentTargets(ctx context.Context, app *platformv1alpha1.Application) ([]platformv1alpha1.ApplicationEnvironmentTarget, error) {
	if app.Spec.Environment == nil {
		return nil, errors.New("environment selection is required")
	}
	if len(app.Spec.Environment.Targets) > 0 {
		return append([]platformv1alpha1.ApplicationEnvironmentTarget(nil), app.Spec.Environment.Targets...), nil
	}
	var clusters platformv1alpha1.ClusterList
	if err := r.Client.List(ctx, &clusters, client.MatchingLabels{platformv1alpha1.ClusterEnvironmentLabelKey: app.Spec.Environment.Name}); err != nil {
		return nil, err
	}
	targets := make([]platformv1alpha1.ApplicationEnvironmentTarget, 0, len(clusters.Items))
	for i := range clusters.Items {
		cluster := &clusters.Items[i]
		if !cluster.DeletionTimestamp.IsZero() || cluster.UID == "" {
			continue
		}
		targets = append(targets, platformv1alpha1.ApplicationEnvironmentTarget{Name: cluster.Name, UID: cluster.UID})
	}
	if len(targets) > 32 {
		return nil, errEnvironmentTooManyTargets
	}
	slices.SortFunc(targets, func(a, b platformv1alpha1.ApplicationEnvironmentTarget) int { return cmp.Compare(a.Name, b.Name) })
	return targets, nil
}

func (r *ApplicationReconciler) reconcileEnvironmentRemovals(ctx context.Context, app *platformv1alpha1.Application, desired []platformv1alpha1.ApplicationEnvironmentTarget) (ctrl.Result, bool, error) {
	for i := 0; i < len(app.Status.Instances); {
		inst := &app.Status.Instances[i]
		if environmentTargetIn(desired, inst.ClusterName, inst.ClusterUID) {
			i++
			continue
		}
		target := platformv1alpha1.ApplicationEnvironmentTarget{Name: inst.ClusterName, UID: inst.ClusterUID}
		if inst.Binding == nil {
			if inst.Applied != nil || inst.LastAttemptedIdentity != "" || inst.CleanupComplete || releaseAbsent(inst) {
				markEnvironmentFailure(inst, app.Generation, "OrphanedTarget", "Environment target has remote state without a binding")
				invalidateEnvironmentAggregates(app, "OrphanedTarget", "Environment target has remote state without a binding")
				if err := r.persistStatus(ctx, app, app.Generation); err != nil {
					result, persistErr := environmentPersistFailure(err)
					return result, true, persistErr
				}
				return ctrl.Result{RequeueAfter: applicationRequeue}, true, nil
			}
			app.Status.Instances = slices.Delete(app.Status.Instances, i, i+1)
			if err := r.persistStatus(ctx, app, app.Generation); err != nil {
				result, persistErr := environmentPersistFailure(err)
				return result, true, persistErr
			}
			continue
		}
		result, done, continuePass, err := r.cleanupEnvironmentInstance(ctx, app, target, inst)
		if err != nil {
			return result, true, err
		}
		if !continuePass {
			return result, true, nil
		}
		if !done {
			return ctrl.Result{RequeueAfter: applicationRequeue}, true, nil
		}
		index := environmentInstanceIndex(app.Status.Instances, target.Name)
		if index < 0 || app.Status.Instances[index].ClusterUID != target.UID {
			return ctrl.Result{RequeueAfter: staleInputRequeue}, true, nil
		}
		app.Status.Instances = slices.Delete(app.Status.Instances, index, index+1)
		if err := r.persistStatus(ctx, app, app.Generation); err != nil {
			result, persistErr := environmentPersistFailure(err)
			return result, true, persistErr
		}
		i = index
	}
	return ctrl.Result{}, false, nil
}

func environmentTargetIn(targets []platformv1alpha1.ApplicationEnvironmentTarget, name string, uid types.UID) bool {
	for _, target := range targets {
		if target.Name == name && target.UID == uid {
			return true
		}
	}
	return false
}

// runEnvironmentTargets uses continueNext=true only after a successful target
// checkpoint or a persisted target-local failure. A false value stops the pass.
func runEnvironmentTargets(targets []platformv1alpha1.ApplicationEnvironmentTarget, apply func(platformv1alpha1.ApplicationEnvironmentTarget) (ctrl.Result, bool, error)) ([]platformv1alpha1.ApplicationEnvironmentTarget, ctrl.Result, bool, error) {
	sorted := append([]platformv1alpha1.ApplicationEnvironmentTarget(nil), targets...)
	slices.SortFunc(sorted, func(a, b platformv1alpha1.ApplicationEnvironmentTarget) int { return cmp.Compare(a.Name, b.Name) })
	for _, target := range sorted {
		result, continueNext, err := apply(target)
		if err != nil {
			return sorted, result, true, err
		}
		if !continueNext {
			return sorted, result, true, nil
		}
	}
	return sorted, ctrl.Result{}, false, nil
}

func (r *ApplicationReconciler) preflightEnvironmentTargets(ctx context.Context, targets []platformv1alpha1.ApplicationEnvironmentTarget) error {
	for _, target := range targets {
		var cluster platformv1alpha1.Cluster
		err := r.getAuthoritative(ctx, types.NamespacedName{Name: target.Name}, &cluster)
		if apierrors.IsNotFound(err) {
			continue // Missing targets are target-local failures handled by apply.
		}
		if err != nil {
			return err
		}
		if cluster.UID != target.UID {
			return errors.New("environment target Cluster was replaced")
		}
	}
	return nil
}

func validateMultiTargetSelection(app *platformv1alpha1.Application) error {
	if app.Spec.ClusterRef != nil {
		return errors.New("invalid selector")
	}
	var targets []platformv1alpha1.ApplicationEnvironmentTarget
	switch {
	case app.Spec.Environment != nil && len(app.Spec.Clusters) == 0:
		targets = app.Spec.Environment.Targets
	case app.Spec.Environment == nil && len(app.Spec.Clusters) > 0:
		targets = explicitClusterTargets(app)
	default:
		return errors.New("invalid selector")
	}
	seenNames, seenUIDs := map[string]bool{}, map[types.UID]bool{}
	for _, target := range targets {
		if target.Name == "" || target.UID == "" || seenNames[target.Name] || seenUIDs[target.UID] {
			return errors.New("invalid target identity")
		}
		seenNames[target.Name], seenUIDs[target.UID] = true, true
	}
	return nil
}

//nolint:gocyclo // orchestration handler; splitting would obscure the sequential flow.
func (r *ApplicationReconciler) applyEnvironmentTarget(ctx context.Context, app *platformv1alpha1.Application, definition *platformv1alpha1.ApplicationDefinition, snapshot platformv1alpha1.ApplicationEnvironmentTarget, namespace string, values map[string]any, chart platformv1alpha1.ApplicationChartIdentity, archive []byte, desired string) (ctrl.Result, bool, error) {
	i := environmentInstanceIndex(app.Status.Instances, snapshot.Name)
	if i < 0 {
		app.Status.Instances = append(app.Status.Instances, platformv1alpha1.ApplicationInstanceStatus{ClusterName: snapshot.Name, ClusterUID: snapshot.UID})
		i = len(app.Status.Instances) - 1
	}
	inst := &app.Status.Instances[i]
	if inst.ClusterUID != snapshot.UID {
		markEnvironmentFailure(inst, app.Generation, "BindingConflict", "Snapshotted Cluster identity conflicts with recorded instance")
		return r.persistEnvironmentFailure(ctx, app, inst)
	}
	var cluster platformv1alpha1.Cluster
	if err := r.getAuthoritative(ctx, types.NamespacedName{Name: snapshot.Name}, &cluster); err != nil || cluster.UID != snapshot.UID || !cluster.DeletionTimestamp.IsZero() {
		markEnvironmentFailure(inst, app.Generation, "TargetUnavailable", "Snapshotted Cluster is unavailable or has been replaced")
		return r.persistEnvironmentFailure(ctx, app, inst)
	}
	if r.APIReader == nil || r.CredentialsNamespace == "" {
		markEnvironmentFailure(inst, app.Generation, "TargetUnavailable", "Target credentials are unavailable")
		return r.persistEnvironmentFailure(ctx, app, inst)
	}
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: r.CredentialsNamespace, Name: cluster.Spec.KubeconfigSecretRef.Name}
	if key.Name == "" || r.APIReader.Get(ctx, key, &secret) != nil || len(secret.Data["kubeconfig"]) == 0 {
		markEnvironmentFailure(inst, app.Generation, "TargetUnavailable", "Target credentials are unavailable")
		return r.persistEnvironmentFailure(ctx, app, inst)
	}
	cfg, remoteTarget, err := parseRemoteClusterConfig(secret.Data["kubeconfig"], namespace)
	if err != nil {
		markEnvironmentFailure(inst, app.Generation, "TargetUnavailable", "Target connection could not be established")
		return r.persistEnvironmentFailure(ctx, app, inst)
	}
	if inst.Binding != nil && (inst.Binding.ApplicationDefinitionUID != definition.UID || inst.Binding.ClusterUID != snapshot.UID || inst.Binding.TargetAPIServer != remoteTarget.Server || inst.Binding.TargetTrustHash != trustHash(remoteTarget.TrustSHA256) || inst.Binding.ReleaseNamespace != namespace || inst.Binding.ReleaseName != r.releaseName(app)) {
		markEnvironmentFailure(inst, app.Generation, "BindingConflict", "Recorded target binding conflicts with current target identity")
		return r.persistEnvironmentFailure(ctx, app, inst)
	}
	if inst.Binding == nil {
		inst.Binding = &platformv1alpha1.ApplicationBinding{ApplicationDefinitionUID: definition.UID, ClusterUID: snapshot.UID, TargetAPIServer: remoteTarget.Server, TargetTrustHash: trustHash(remoteTarget.TrustSHA256), ReleaseNamespace: namespace, ReleaseName: r.releaseName(app)}
		invalidateEnvironmentAggregates(app, "BindingPending", "Environment target binding is being established")
		if err := r.persistStatus(ctx, app, app.Generation); err != nil {
			result, persistErr := environmentPersistFailure(err)
			return result, false, persistErr
		}
		inst = environmentInstanceForTarget(app, snapshot)
		if inst == nil {
			return ctrl.Result{RequeueAfter: staleInputRequeue}, false, nil
		}
	}
	if !r.currentEnvironmentInputs(ctx, app, definition, &cluster) {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, false, nil
	}
	remote, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		markEnvironmentFailure(inst, app.Generation, "TargetUnavailable", "Target connection could not be established")
		return r.persistEnvironmentFailure(ctx, app, inst)
	}
	manager := r.claimManager(remote)
	claimID := claimIdentityFor(app, inst.Binding)
	claim := releaseclaim.Claim{Namespace: namespace, Name: releaseclaim.ClaimName(inst.Binding.ReleaseName), UID: inst.Binding.RemoteClaimUID}
	if claim.UID == "" {
		claim, err = manager.Acquire(ctx, claimID)
		if err != nil {
			markEnvironmentFailure(inst, app.Generation, "OwnershipConflict", "Remote release ownership could not be established")
			return r.persistEnvironmentFailure(ctx, app, inst)
		}
		inst.Binding.RemoteClaimUID = claim.UID
		invalidateEnvironmentAggregates(app, "OwnershipPending", "Environment target ownership is being verified")
		if err := r.persistStatus(ctx, app, app.Generation); err != nil {
			result, persistErr := environmentPersistFailure(err)
			return result, false, persistErr
		}
		inst = environmentInstanceForTarget(app, snapshot)
		if inst == nil || inst.Binding == nil || inst.Binding.RemoteClaimUID != claim.UID {
			return ctrl.Result{RequeueAfter: staleInputRequeue}, false, nil
		}
	} else if err := manager.Verify(ctx, claimID, claim); err != nil {
		markEnvironmentFailure(inst, app.Generation, "OwnershipConflict", "Recorded remote release ownership could not be verified")
		return r.persistEnvironmentFailure(ctx, app, inst)
	}
	if err := manager.Verify(ctx, claimID, claim); err != nil || !r.currentEnvironmentInputs(ctx, app, definition, &cluster) {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, false, nil
	}
	previousAttempt := inst.LastAttemptedIdentity != ""
	inst.ObservedGeneration = app.Generation
	if inst.LastAttemptedIdentity != desired {
		inst.LastAttemptedIdentity = desired
	}
	setInstanceCondition(inst, "InventoryReady", metav1.ConditionUnknown, "ApplyPending", "Inventory is not current until the release is observed")
	setInstanceCondition(inst, "WorkloadHealthy", metav1.ConditionUnknown, "ApplyPending", "Workload health is not current until observed")
	invalidateEnvironmentAggregates(app, "ApplyPending", "Environment target releases are not all current")
	if err := r.persistStatus(ctx, app, app.Generation); err != nil {
		result, persistErr := environmentPersistFailure(err)
		return result, false, persistErr
	}
	inst = environmentInstanceForTarget(app, snapshot)
	if inst == nil || inst.Binding == nil || inst.Binding.RemoteClaimUID != claim.UID || inst.LastAttemptedIdentity != desired {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, false, nil
	}
	if !r.currentEnvironmentInputs(ctx, app, definition, &cluster) {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, false, nil
	}
	bindingSnapshot := *inst.Binding
	request := helmrelease.Request{Namespace: namespace, ReleaseName: bindingSnapshot.ReleaseName, Repository: chart.Repository, ChartName: chart.Name, ChartVersion: chart.Version, Digest: actualChartDigest(chart, archive), Archive: archive, Values: values, ApplicationUID: string(app.UID), ClaimUID: string(claim.UID), PreviousAttempt: previousAttempt}
	result, err := r.helm(cfg, namespace).Apply(ctx, request)
	if err != nil {
		markEnvironmentFailure(inst, app.Generation, "ReleaseOperationFailed", "Helm release operation failed and may require recovery")
		return r.persistEnvironmentFailure(ctx, app, inst)
	}
	claimErr := manager.Verify(ctx, claimID, claim)
	inputsCurrent := r.currentEnvironmentInputs(ctx, app, definition, &cluster)
	if claimErr != nil || !inputsCurrent {
		reason := "StaleOperation"
		if claimErr != nil {
			reason = "OwnershipChanged"
		}
		return r.recoverEnvironmentApply(ctx, app, snapshot, bindingSnapshot, definition.Generation, chart, result, reason)
	}
	inst.Applied = &platformv1alpha1.ApplicationAppliedIdentity{ApplicationDefinitionGeneration: definition.Generation, ApplicationGeneration: app.Generation, Chart: chart, DesiredInputIdentity: result.DesiredIdentity, HelmRevision: int64(result.HelmRevision)}
	inst.ObservedGeneration = app.Generation
	setInstanceCondition(inst, "InventoryReady", metav1.ConditionUnknown, "ObservationPending", "Inventory is awaiting observation")
	setInstanceCondition(inst, "WorkloadHealthy", metav1.ConditionUnknown, "ObservationPending", "Workload health is awaiting observation")
	invalidateEnvironmentAggregates(app, "ObservationPending", "Environment target observations are pending")
	if err := r.persistStatus(ctx, app, app.Generation); err != nil {
		return r.recoverEnvironmentApply(ctx, app, snapshot, bindingSnapshot, definition.Generation, chart, result, "StaleOperation")
	}
	inst = environmentInstanceForTarget(app, snapshot)
	if inst == nil || inst.Binding == nil || *inst.Binding != bindingSnapshot {
		return r.recoverEnvironmentApply(ctx, app, snapshot, bindingSnapshot, definition.Generation, chart, result, "StaleOperation")
	}
	if err := manager.Verify(ctx, claimID, claim); err != nil || !r.currentEnvironmentInputs(ctx, app, definition, &cluster) {
		reason := "StaleOperation"
		if err != nil {
			reason = "OwnershipChanged"
		}
		return r.recoverEnvironmentApply(ctx, app, snapshot, bindingSnapshot, definition.Generation, chart, result, reason)
	}
	resources, err := r.collectResources(ctx, cfg, result.Manifest, namespace)
	if err != nil {
		setInstanceCondition(inst, "InventoryReady", metav1.ConditionFalse, "CollectionFailed", "Application resource inventory could not be refreshed")
		setInstanceCondition(inst, "WorkloadHealthy", metav1.ConditionUnknown, applicationObservationUnavailable, "Workload health could not be observed")
	} else {
		inst.Resources = resources
		now := metav1.Now()
		inst.WorkloadObservedAt = &now
		status, reason, message := aggregateWorkloadHealth(resources)
		setInstanceCondition(inst, "InventoryReady", metav1.ConditionTrue, "Collected", "Application resource inventory was refreshed")
		setInstanceCondition(inst, "WorkloadHealthy", status, reason, message)
	}
	inst.ObservedGeneration = app.Generation
	setInstanceCondition(inst, "Ready", metav1.ConditionTrue, "Ready", "Application release is ready")
	invalidateEnvironmentAggregates(app, "TargetCheckpointPending", "Environment aggregate status is pending the remaining targets")
	if err := r.persistStatus(ctx, app, app.Generation); err != nil {
		result, persistErr := environmentPersistFailure(err)
		return result, false, persistErr
	}
	return ctrl.Result{}, true, nil
}

// recoverEnvironmentApply records the exact successful Helm result against the
// same target binding when the original status snapshot has gone stale. It
// intentionally leaves sibling instances and current desired inputs untouched.
func (r *ApplicationReconciler) recoverEnvironmentApply(ctx context.Context, attempted *platformv1alpha1.Application, target platformv1alpha1.ApplicationEnvironmentTarget, binding platformv1alpha1.ApplicationBinding, definitionGeneration int64, chart platformv1alpha1.ApplicationChartIdentity, result helmrelease.Result, reason string) (ctrl.Result, bool, error) {
	var current platformv1alpha1.Application
	if err := r.getAuthoritative(ctx, types.NamespacedName{Namespace: attempted.Namespace, Name: attempted.Name}, &current); err != nil {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, false, nil
	}
	if current.UID != attempted.UID || !current.DeletionTimestamp.IsZero() || !usesMultiTargetSelection(&current) || !environmentTargetAllowed(&current, target) {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, false, nil
	}
	if current.Status.EnvironmentBinding == nil || current.Status.EnvironmentBinding.ApplicationDefinitionUID != binding.ApplicationDefinitionUID || current.Status.EnvironmentBinding.ReleaseNamespace != binding.ReleaseNamespace {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, false, nil
	}
	i := environmentInstanceIndex(current.Status.Instances, target.Name)
	if i < 0 {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, false, nil
	}
	inst := &current.Status.Instances[i]
	if inst.ClusterUID != target.UID || inst.Binding == nil || *inst.Binding != binding || inst.Binding.RemoteClaimUID != binding.RemoteClaimUID {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, false, nil
	}
	applied := &platformv1alpha1.ApplicationAppliedIdentity{
		ApplicationDefinitionGeneration: definitionGeneration,
		ApplicationGeneration:           attempted.Generation,
		Chart:                           chart,
		DesiredInputIdentity:            result.DesiredIdentity,
		HelmRevision:                    int64(result.HelmRevision),
	}
	if inst.Applied == nil || (inst.Applied.HelmRevision < applied.HelmRevision && inst.Applied.ApplicationGeneration <= applied.ApplicationGeneration && inst.Applied.ApplicationDefinitionGeneration <= applied.ApplicationDefinitionGeneration) || (inst.Applied.HelmRevision == applied.HelmRevision && inst.Applied.DesiredInputIdentity == applied.DesiredInputIdentity && inst.Applied.ApplicationGeneration <= applied.ApplicationGeneration && inst.Applied.ApplicationDefinitionGeneration <= applied.ApplicationDefinitionGeneration) {
		inst.Applied = applied
	}
	inst.ObservedGeneration = current.Generation
	setInstanceCondition(inst, "Ready", metav1.ConditionFalse, reason, "Helm succeeded but the operation is not current")
	setInstanceCondition(inst, "InventoryReady", metav1.ConditionUnknown, reason, "Inventory is not current after a stale operation")
	setInstanceCondition(inst, "WorkloadHealthy", metav1.ConditionUnknown, reason, "Workload health is not current after a stale operation")
	setApplicationCondition(&current, metav1.ConditionFalse, reason, "Helm succeeded but the operation is not current")
	setInventoryCondition(&current, metav1.ConditionUnknown, reason, "Inventory is not current after a stale operation")
	setWorkloadCondition(&current, metav1.ConditionUnknown, reason, "Workload health is not current after a stale operation")
	if err := r.persistStatus(ctx, &current, current.Generation); err != nil {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, false, nil
	}
	return ctrl.Result{RequeueAfter: staleInputRequeue}, false, nil
}

func environmentTargetAllowed(app *platformv1alpha1.Application, target platformv1alpha1.ApplicationEnvironmentTarget) bool {
	if len(app.Spec.Clusters) > 0 {
		return environmentHasTarget(explicitClusterTargets(app), target)
	}
	if app.Spec.Environment == nil {
		return false
	}
	if len(app.Spec.Environment.Targets) == 0 {
		return true
	}
	return environmentHasTarget(app.Spec.Environment.Targets, target)
}

func environmentHasTarget(targets []platformv1alpha1.ApplicationEnvironmentTarget, want platformv1alpha1.ApplicationEnvironmentTarget) bool {
	for _, target := range targets {
		if target.Name == want.Name && target.UID == want.UID {
			return true
		}
	}
	return false
}

// PullVerified returns the bytes' digest already checked against the definition;
// passing that expected digest keeps Helm requests pinned to the verified archive.
func actualChartDigest(chart platformv1alpha1.ApplicationChartIdentity, _ []byte) string {
	return chart.Digest
}

func (r *ApplicationReconciler) currentEnvironmentInputs(ctx context.Context, app *platformv1alpha1.Application, definition *platformv1alpha1.ApplicationDefinition, cluster *platformv1alpha1.Cluster) bool {
	var current platformv1alpha1.Application
	if r.getAuthoritative(ctx, types.NamespacedName{Namespace: app.Namespace, Name: app.Name}, &current) != nil || current.UID != app.UID || current.Generation != app.Generation || current.ResourceVersion != app.ResourceVersion || !current.DeletionTimestamp.IsZero() {
		return false
	}
	var def platformv1alpha1.ApplicationDefinition
	if r.getAuthoritative(ctx, types.NamespacedName{Namespace: definition.Namespace, Name: definition.Name}, &def) != nil || def.UID != definition.UID || def.Generation != definition.Generation || def.Status.Digest != definition.Status.Digest {
		return false
	}
	validated := meta.FindStatusCondition(def.Status.Conditions, "Validated")
	if validated == nil || validated.Status != metav1.ConditionTrue || def.Status.ObservedGeneration != def.Generation || validated.ObservedGeneration != def.Generation {
		return false
	}
	var target platformv1alpha1.Cluster
	return r.getAuthoritative(ctx, types.NamespacedName{Name: cluster.Name}, &target) == nil && target.UID == cluster.UID && target.Generation == cluster.Generation && target.DeletionTimestamp.IsZero()
}

func (r *ApplicationReconciler) persistEnvironmentFailure(ctx context.Context, app *platformv1alpha1.Application, inst *platformv1alpha1.ApplicationInstanceStatus) (ctrl.Result, bool, error) {
	ready := meta.FindStatusCondition(inst.Conditions, "Ready")
	reason, message := "InstanceNotReady", "One or more environment releases are not current and ready"
	if ready != nil && ready.Reason != "" {
		reason, message = ready.Reason, ready.Message
	}
	invalidateEnvironmentAggregates(app, reason, message)
	if err := r.persistStatus(ctx, app, app.Generation); err != nil {
		result, err := environmentPersistFailure(err)
		return result, false, err
	}
	return ctrl.Result{}, true, nil
}

func invalidateEnvironmentAggregates(app *platformv1alpha1.Application, reason, message string) {
	setApplicationCondition(app, metav1.ConditionFalse, reason, message)
	setInventoryCondition(app, metav1.ConditionUnknown, reason, "Environment inventory is not current")
	setWorkloadCondition(app, metav1.ConditionUnknown, reason, "Environment workload health is not current")
}

func (r *ApplicationReconciler) environmentFail(ctx context.Context, app *platformv1alpha1.Application, reason, message string) (ctrl.Result, error) {
	if changed := setApplicationCondition(app, metav1.ConditionFalse, reason, message); changed {
		r.recordEvent(app, corev1.EventTypeWarning, reason, message)
	}
	setInventoryCondition(app, metav1.ConditionUnknown, reason, message)
	setWorkloadCondition(app, metav1.ConditionUnknown, reason, message)
	if err := r.persistStatus(ctx, app, app.Generation); err != nil {
		return environmentPersistFailure(err)
	}
	return ctrl.Result{RequeueAfter: applicationRequeue}, nil
}

func environmentPersistFailure(err error) (ctrl.Result, error) {
	if errors.Is(err, errStaleApplicationSnapshot) || apierrors.IsNotFound(err) {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, nil
	}
	return ctrl.Result{}, err
}

func environmentInstanceIndex(instances []platformv1alpha1.ApplicationInstanceStatus, name string) int {
	for i := range instances {
		if instances[i].ClusterName == name {
			return i
		}
	}
	return -1
}

func environmentInstanceForTarget(app *platformv1alpha1.Application, target platformv1alpha1.ApplicationEnvironmentTarget) *platformv1alpha1.ApplicationInstanceStatus {
	i := environmentInstanceIndex(app.Status.Instances, target.Name)
	if i < 0 || app.Status.Instances[i].ClusterUID != target.UID {
		return nil
	}
	return &app.Status.Instances[i]
}

func markEnvironmentFailure(inst *platformv1alpha1.ApplicationInstanceStatus, generation int64, reason, message string) {
	inst.ObservedGeneration = generation
	setInstanceCondition(inst, "Ready", metav1.ConditionFalse, reason, message)
	setInstanceCondition(inst, "InventoryReady", metav1.ConditionUnknown, reason, "Inventory is not current")
	setInstanceCondition(inst, "WorkloadHealthy", metav1.ConditionUnknown, reason, "Workload health is not current")
}

func setInstanceCondition(inst *platformv1alpha1.ApplicationInstanceStatus, typ string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&inst.Conditions, metav1.Condition{Type: typ, Status: status, ObservedGeneration: inst.ObservedGeneration, Reason: reason, Message: message, LastTransitionTime: metav1.Now()})
}

func aggregateEnvironmentStatus(app *platformv1alpha1.Application, targets []platformv1alpha1.ApplicationEnvironmentTarget, desired string, chart platformv1alpha1.ApplicationChartIdentity, definitionGeneration int64) bool {
	allReady, allInventory := true, true
	readyTransitioned := false
	healthStatuses := make([]metav1.Condition, 0, len(targets))
	for _, target := range targets {
		i := environmentInstanceIndex(app.Status.Instances, target.Name)
		if i < 0 {
			allReady, allInventory = false, false
			healthStatuses = append(healthStatuses, metav1.Condition{Status: metav1.ConditionUnknown})
			continue
		}
		instance := &app.Status.Instances[i]
		ready, inventory, workload := meta.FindStatusCondition(instance.Conditions, "Ready"), meta.FindStatusCondition(instance.Conditions, "InventoryReady"), meta.FindStatusCondition(instance.Conditions, "WorkloadHealthy")
		current := instance.ClusterUID == target.UID && instance.ObservedGeneration == app.Generation && instance.Applied != nil && instance.Applied.ApplicationGeneration == app.Generation && instance.Applied.ApplicationDefinitionGeneration == definitionGeneration && instance.Applied.DesiredInputIdentity == desired && instance.Applied.Chart == chart
		age := time.Duration(-1)
		if instance.WorkloadObservedAt != nil {
			age = time.Since(instance.WorkloadObservedAt.Time)
		}
		fresh := age >= 0 && age <= 5*time.Minute
		if !current || ready == nil || ready.ObservedGeneration != app.Generation || ready.Status != metav1.ConditionTrue {
			allReady = false
		}
		if !current || !fresh || inventory == nil || inventory.ObservedGeneration != app.Generation || inventory.Status != metav1.ConditionTrue {
			allInventory = false
		}
		if !current || !fresh || workload == nil || workload.ObservedGeneration != app.Generation {
			healthStatuses = append(healthStatuses, metav1.Condition{Status: metav1.ConditionUnknown})
		} else {
			healthStatuses = append(healthStatuses, *workload)
		}
	}
	if allReady {
		readyTransitioned = setApplicationCondition(app, metav1.ConditionTrue, "Ready", "All environment releases are ready")
	} else {
		setApplicationCondition(app, metav1.ConditionFalse, "InstanceNotReady", "One or more environment releases are not current and ready")
	}
	if allInventory {
		setInventoryCondition(app, metav1.ConditionTrue, "Collected", "All environment inventories are current")
	} else {
		setInventoryCondition(app, metav1.ConditionFalse, "InstanceInventoryNotReady", "One or more environment inventories are not current")
	}
	status, reason, message := aggregateEnvironmentHealth(healthStatuses)
	setWorkloadCondition(app, status, reason, message)
	return readyTransitioned
}

func aggregateEnvironmentHealth(conditions []metav1.Condition) (metav1.ConditionStatus, string, string) {
	if len(conditions) == 0 {
		return metav1.ConditionUnknown, applicationObservationUnavailable, "Environment workload health is unknown"
	}
	if slices.ContainsFunc(conditions, func(c metav1.Condition) bool {
		return c.Status == metav1.ConditionFalse && c.Reason == applicationWorkloadDegraded
	}) {
		return metav1.ConditionFalse, applicationWorkloadDegraded, "One or more environment workloads are degraded"
	}
	if slices.ContainsFunc(conditions, func(c metav1.Condition) bool { return c.Status == metav1.ConditionUnknown }) {
		return metav1.ConditionUnknown, applicationObservationUnavailable, "Environment workload health is unknown"
	}
	if slices.ContainsFunc(conditions, func(c metav1.Condition) bool { return c.Status == metav1.ConditionFalse }) {
		return metav1.ConditionFalse, applicationWorkloadProgressing, "One or more environment workloads are progressing"
	}
	return metav1.ConditionTrue, applicationWorkloadHealthy, "All environment workloads are healthy"
}
