package controller

import (
	"cmp"
	"context"
	"errors"
	"slices"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	"github.com/federicolepera/idp/internal/helmrelease"
	"github.com/federicolepera/idp/internal/releaseclaim"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *ApplicationReconciler) reconcileEnvironmentDelete(ctx context.Context, app *platformv1alpha1.Application) (ctrl.Result, error) {
	if !slices.Contains(app.Finalizers, applicationFinalizer) {
		return ctrl.Result{}, nil
	}
	if app.Status.Binding != nil {
		return r.environmentDeletePending(ctx, app, "LegacyBindingConflict")
	}
	if app.Spec.Environment == nil {
		return r.environmentDeletePending(ctx, app, "SnapshotUnavailable")
	}
	targets := make([]platformv1alpha1.ApplicationEnvironmentTarget, 0, len(app.Status.Instances))
	for i := range app.Status.Instances {
		targets = append(targets, platformv1alpha1.ApplicationEnvironmentTarget{Name: app.Status.Instances[i].ClusterName, UID: app.Status.Instances[i].ClusterUID})
	}
	slices.SortFunc(targets, func(a, b platformv1alpha1.ApplicationEnvironmentTarget) int { return cmp.Compare(a.Name, b.Name) })
	blocked := false
	for _, target := range targets {
		i := environmentInstanceIndex(app.Status.Instances, target.Name)
		if i < 0 {
			continue
		}
		inst := &app.Status.Instances[i]
		if inst.ClusterUID != target.UID {
			blocked = true
			continue
		}
		if inst.Binding == nil {
			// No binding means no remote mutation was allowed. However, evidence of a
			// prior operation without its binding is inconsistent and must fail closed.
			if inst.Applied != nil || inst.LastAttemptedIdentity != "" || inst.CleanupComplete || releaseAbsent(inst) {
				blocked = true
			}
			continue
		}
		if inst.CleanupComplete {
			if !releaseAbsent(inst) || inst.Binding == nil || inst.Binding.RemoteClaimUID == "" {
				blocked = true
			}
			continue
		}
		result, done, continuePass, err := r.cleanupEnvironmentInstance(ctx, app, target, inst)
		if err != nil {
			return result, err
		}
		if !continuePass {
			return result, nil
		}
		if !done {
			blocked = true
			continue
		}
	}
	if blocked || !allEnvironmentBindingsCleaned(app) {
		return ctrl.Result{RequeueAfter: applicationRequeue}, nil
	}
	return r.removeEnvironmentFinalizer(ctx, app)
}

//nolint:gocyclo // orchestration handler; splitting would obscure the sequential flow.
func (r *ApplicationReconciler) cleanupEnvironmentInstance(ctx context.Context, app *platformv1alpha1.Application, target platformv1alpha1.ApplicationEnvironmentTarget, inst *platformv1alpha1.ApplicationInstanceStatus) (ctrl.Result, bool, bool, error) {
	b := inst.Binding
	var ok bool
	if b == nil || b.ClusterUID != target.UID || b.ReleaseName != r.releaseName(app) || b.ReleaseNamespace == "" || b.ApplicationDefinitionUID == "" || b.TargetAPIServer == "" || b.TargetTrustHash == "" {
		return ctrl.Result{}, false, true, nil
	}
	if app.Status.EnvironmentBinding == nil || app.Status.EnvironmentBinding.ApplicationDefinitionUID != b.ApplicationDefinitionUID || app.Status.EnvironmentBinding.ReleaseNamespace != b.ReleaseNamespace {
		return ctrl.Result{}, false, true, nil
	}
	if r.APIReader == nil || r.CredentialsNamespace == "" {
		return ctrl.Result{}, false, true, nil
	}
	var cluster platformv1alpha1.Cluster
	if err := r.getAuthoritative(ctx, types.NamespacedName{Name: target.Name}, &cluster); err != nil || cluster.UID != target.UID || !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, false, true, nil
	}
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: r.CredentialsNamespace, Name: cluster.Spec.KubeconfigSecretRef.Name}
	if key.Name == "" || r.APIReader.Get(ctx, key, &secret) != nil || len(secret.Data["kubeconfig"]) == 0 {
		return ctrl.Result{}, false, true, nil
	}
	cfg, identity, err := parseRemoteClusterConfig(secret.Data["kubeconfig"], b.ReleaseNamespace)
	if err != nil || identity.Server != b.TargetAPIServer || trustHash(identity.TrustSHA256) != b.TargetTrustHash {
		return ctrl.Result{}, false, true, nil
	}
	remote, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return ctrl.Result{}, false, true, nil
	}
	manager := r.claimManager(remote)
	claimIdentity := claimIdentityFor(app, b)
	claim := releaseclaim.Claim{Namespace: b.ReleaseNamespace, Name: releaseclaim.ClaimName(b.ReleaseName), UID: b.RemoteClaimUID}
	if releaseAbsent(inst) {
		if claim.UID == "" {
			return ctrl.Result{}, false, true, nil
		}
		if err := manager.Delete(ctx, claimIdentity, claim); err != nil && !errors.Is(err, releaseclaim.ErrClaimNotFound) {
			return ctrl.Result{}, false, true, nil
		}
		inst.CleanupComplete = true
		if err := r.persistStatus(ctx, app, app.Generation); err != nil {
			result, persistErr := environmentPersistFailure(err)
			return result, false, false, persistErr
		}
		inst, ok = reselectEnvironmentInstance(app, target)
		if !ok || inst.Binding == nil || inst.Binding.RemoteClaimUID != claim.UID {
			return ctrl.Result{RequeueAfter: staleInputRequeue}, false, false, nil
		}
		b = inst.Binding
		claim = releaseclaim.Claim{Namespace: b.ReleaseNamespace, Name: releaseclaim.ClaimName(b.ReleaseName), UID: b.RemoteClaimUID}
		return ctrl.Result{}, true, true, nil
	}
	if claim.UID == "" {
		claim, err = manager.Acquire(ctx, claimIdentity)
		if err != nil || claim.UID == "" {
			return ctrl.Result{}, false, true, nil
		}
		updated := *b
		updated.RemoteClaimUID = claim.UID
		inst.Binding = &updated
		if err := r.persistStatus(ctx, app, app.Generation); err != nil {
			result, persistErr := environmentPersistFailure(err)
			return result, false, false, persistErr
		}
		inst, ok = reselectEnvironmentInstance(app, target)
		if !ok || inst.Binding == nil || inst.Binding.RemoteClaimUID != claim.UID {
			return ctrl.Result{RequeueAfter: staleInputRequeue}, false, false, nil
		}
		b = inst.Binding
		claimIdentity = claimIdentityFor(app, b)
		claim = releaseclaim.Claim{Namespace: b.ReleaseNamespace, Name: releaseclaim.ClaimName(b.ReleaseName), UID: b.RemoteClaimUID}
	}
	if err := manager.Verify(ctx, claimIdentity, claim); err != nil {
		return ctrl.Result{}, false, true, nil
	}
	_, err = r.helm(cfg, b.ReleaseNamespace).Uninstall(ctx, helmrelease.Request{Namespace: b.ReleaseNamespace, ReleaseName: b.ReleaseName, ApplicationUID: string(app.UID), ClaimUID: string(claim.UID)})
	if err != nil {
		return ctrl.Result{}, false, true, nil
	}
	meta.SetStatusCondition(&inst.Conditions, metav1.Condition{Type: applicationReleaseAbsentCondition, Status: metav1.ConditionTrue, ObservedGeneration: app.Generation, Reason: "Uninstalled", Message: "Remote Helm release is absent", LastTransitionTime: metav1.Now()})
	if err := r.persistStatus(ctx, app, app.Generation); err != nil {
		result, persistErr := environmentPersistFailure(err)
		return result, false, false, persistErr
	}
	inst, ok = reselectEnvironmentInstance(app, target)
	if !ok || !releaseAbsent(inst) {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, false, false, nil
	}
	return ctrl.Result{RequeueAfter: staleInputRequeue}, false, true, nil
}

func reselectEnvironmentInstance(app *platformv1alpha1.Application, target platformv1alpha1.ApplicationEnvironmentTarget) (*platformv1alpha1.ApplicationInstanceStatus, bool) {
	var selected *platformv1alpha1.ApplicationInstanceStatus
	for i := range app.Status.Instances {
		instance := &app.Status.Instances[i]
		if instance.ClusterName != target.Name {
			continue
		}
		if instance.ClusterUID != target.UID || selected != nil {
			return nil, false
		}
		selected = instance
	}
	return selected, selected != nil
}

func releaseAbsent(inst *platformv1alpha1.ApplicationInstanceStatus) bool {
	c := metav1.Condition{}
	for i := range inst.Conditions {
		if inst.Conditions[i].Type == applicationReleaseAbsentCondition {
			c = inst.Conditions[i]
			break
		}
	}
	return c.Status == metav1.ConditionTrue
}

func allEnvironmentBindingsCleaned(app *platformv1alpha1.Application) bool {
	if app.Status.Binding != nil || app.Spec.Environment == nil {
		return false
	}
	for i := range app.Status.Instances {
		inst := &app.Status.Instances[i]
		if inst.Binding != nil && !inst.CleanupComplete {
			return false
		}
		if inst.Binding != nil && inst.CleanupComplete && (!releaseAbsent(inst) || inst.Binding.RemoteClaimUID == "") {
			return false
		}
		if inst.Binding != nil && (app.Status.EnvironmentBinding == nil || inst.Binding.ApplicationDefinitionUID != app.Status.EnvironmentBinding.ApplicationDefinitionUID || inst.Binding.ReleaseNamespace != app.Status.EnvironmentBinding.ReleaseNamespace) {
			return false
		}
		if inst.Binding == nil && (inst.Applied != nil || inst.LastAttemptedIdentity != "" || inst.CleanupComplete || releaseAbsent(inst)) {
			return false
		}
	}
	return true
}

func (r *ApplicationReconciler) environmentDeletePending(ctx context.Context, app *platformv1alpha1.Application, reason string) (ctrl.Result, error) {
	setApplicationCondition(app, metav1.ConditionUnknown, reason, "Environment cleanup is blocked by inconsistent ownership status")
	if err := r.persistStatus(ctx, app, app.Generation); err != nil {
		return environmentPersistFailure(err)
	}
	return ctrl.Result{RequeueAfter: applicationRequeue}, nil
}

func (r *ApplicationReconciler) removeEnvironmentFinalizer(ctx context.Context, app *platformv1alpha1.Application) (ctrl.Result, error) {
	var current platformv1alpha1.Application
	if err := r.getAuthoritative(ctx, types.NamespacedName{Namespace: app.Namespace, Name: app.Name}, &current); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if current.UID != app.UID || current.Generation != app.Generation || current.ResourceVersion != app.ResourceVersion || !sameDeletionTimestamp(current.DeletionTimestamp, app.DeletionTimestamp) || !allEnvironmentBindingsCleaned(&current) {
		return ctrl.Result{RequeueAfter: staleInputRequeue}, nil
	}
	current.Finalizers = removeString(current.Finalizers, applicationFinalizer)
	return ctrl.Result{}, r.Client.Update(ctx, &current)
}
