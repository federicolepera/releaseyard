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
	"strings"
	"time"

	"github.com/federicolepera/idp/internal/contract"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
)

const (
	applicationDefinitionReplicasPath   = "replicas"
	applicationDefinitionConditionType  = "Validated"
	applicationDefinitionReasonInvalid  = "InvalidContract"
	applicationDefinitionReasonFetch    = "ChartFetchFailed"
	applicationDefinitionReasonValid    = "Validated"
	applicationDefinitionFetchMessage   = "Chart could not be listed or downloaded"
	applicationDefinitionInvalidMessage = "ApplicationDefinition contract is invalid"
	applicationDefinitionValidMessage   = "ApplicationDefinition contract is valid and chart archive bytes were downloaded"
	applicationDefinitionFetchRetry     = 2 * time.Minute
	applicationDefinitionRecheck        = 10 * time.Minute
)

// ChartPuller fetches the chart artifact and returns its content digest.
type ChartPuller interface {
	Pull(ctx context.Context, chart platformv1alpha1.ApplicationDefinitionChart) (digest string, err error)
}

// ApplicationDefinitionReconciler reconciles an ApplicationDefinition by
// validating its declared contract and verifying its chart artifact.
type ApplicationDefinitionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Puller ChartPuller
}

// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=applicationdefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=applicationdefinitions/status,verbs=get;update;patch

func (r *ApplicationDefinitionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	definition := &platformv1alpha1.ApplicationDefinition{}
	if err := r.Get(ctx, req.NamespacedName, definition); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	checkedGeneration := definition.Generation
	checkedUID := definition.UID

	if err := contract.Validate(definition); err != nil {
		return r.updateStatus(ctx, req.NamespacedName, checkedUID, checkedGeneration,
			metav1.ConditionFalse, applicationDefinitionReasonInvalid,
			applicationDefinitionInvalidMessage, "", ctrl.Result{})
	}

	if ctx.Err() != nil {
		return ctrl.Result{}, nil
	}
	if r.Puller == nil {
		return r.updateStatus(ctx, req.NamespacedName, checkedUID, checkedGeneration,
			metav1.ConditionFalse, applicationDefinitionReasonFetch,
			applicationDefinitionFetchMessage, "", ctrl.Result{RequeueAfter: applicationDefinitionFetchRetry})
	}

	digest, err := r.Puller.Pull(ctx, definition.Spec.Chart)
	if ctx.Err() != nil {
		return ctrl.Result{}, nil
	}
	if err != nil || !validChartDigest(digest) {
		return r.updateStatus(ctx, req.NamespacedName, checkedUID, checkedGeneration,
			metav1.ConditionFalse, applicationDefinitionReasonFetch,
			applicationDefinitionFetchMessage, "", ctrl.Result{RequeueAfter: applicationDefinitionFetchRetry})
	}

	return r.updateStatus(ctx, req.NamespacedName, checkedUID, checkedGeneration,
		metav1.ConditionTrue, applicationDefinitionReasonValid,
		applicationDefinitionValidMessage, digest, ctrl.Result{RequeueAfter: applicationDefinitionRecheck})
}

func (r *ApplicationDefinitionReconciler) updateStatus(
	ctx context.Context,
	key types.NamespacedName,
	checkedUID types.UID,
	checkedGeneration int64,
	conditionStatus metav1.ConditionStatus,
	reason, message, digest string,
	result ctrl.Result,
) (ctrl.Result, error) {
	current := &platformv1alpha1.ApplicationDefinition{}
	if err := r.Get(ctx, key, current); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if current.UID != checkedUID || current.Generation != checkedGeneration {
		// A replacement object or new generation supersedes the result just checked.
		// Its create or generation event will cause the current object to be processed.
		return ctrl.Result{}, nil
	}

	now := metav1.Now()
	condition := metav1.Condition{
		Type:               applicationDefinitionConditionType,
		Status:             conditionStatus,
		ObservedGeneration: checkedGeneration,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	}
	meta.SetStatusCondition(&current.Status.Conditions, condition)
	current.Status.Digest = digest
	current.Status.LastCheckedTime = now
	current.Status.ObservedGeneration = checkedGeneration
	if err := r.Status().Update(ctx, current); err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

func validChartDigest(digest string) bool {
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return false
	}
	for _, character := range digest[len("sha256:"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *ApplicationDefinitionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.ApplicationDefinition{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("applicationdefinition").
		Complete(r)
}
