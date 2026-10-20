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
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
)

const (
	resourceConditionReady           = "Ready"
	resourceReasonCreating           = "Creating"
	resourceReasonAvailable          = "Available"
	resourceReasonNotReady           = "NotReady"
	resourceReasonDefinitionNotFound = "DefinitionNotFound"
	resourceReasonTargetNotFound     = "TargetNotFound"
	resourceReasonApplyFailed        = "ApplyFailed"

	resourceRequeueInterval = 10 * time.Second
)

// ResourceReconciler reconciles a Resource object
type ResourceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=resources,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=resources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=resources/finalizers,verbs=update
// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=resourcedefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=cloudtargets,verbs=get;list;watch
// +kubebuilder:rbac:groups=s3.aws.m.upbound.io,resources=buckets,verbs=get;list;watch;create;update;patch;delete

// Reconcile resolves the ResourceDefinition and CloudTarget for a Resource,
// applies the managed resources they describe and reports their status.
func (r *ResourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var resource platformv1alpha1.Resource
	if err := r.Get(ctx, req.NamespacedName, &resource); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	definition := &platformv1alpha1.ResourceDefinition{}
	if err := r.Get(ctx, types.NamespacedName{Name: resource.Spec.DefinitionRef.Name}, definition); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.setCondition(ctx, &resource, metav1.ConditionFalse, resourceReasonDefinitionNotFound,
				fmt.Sprintf("ResourceDefinition %q was not found", resource.Spec.DefinitionRef.Name))
		}
		return ctrl.Result{}, err
	}

	target := &platformv1alpha1.CloudTarget{}
	if err := r.Get(ctx, types.NamespacedName{Name: resource.Spec.TargetRef.Name}, target); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.setCondition(ctx, &resource, metav1.ConditionFalse, resourceReasonTargetNotFound,
				fmt.Sprintf("CloudTarget %q was not found", resource.Spec.TargetRef.Name))
		}
		return ctrl.Result{}, err
	}

	composed := make([]platformv1alpha1.ComposedResource, 0, len(definition.Spec.Resources))
	managedResources := make([]*unstructured.Unstructured, 0, len(definition.Spec.Resources))
	for _, template := range definition.Spec.Resources {
		managed, ref, err := r.applyTemplate(ctx, &resource, target, template)
		if err != nil {
			return ctrl.Result{}, r.setCondition(ctx, &resource, metav1.ConditionFalse, resourceReasonApplyFailed,
				fmt.Sprintf("Could not apply %q: %v", template.Name, err))
		}
		composed = append(composed, ref)
		managedResources = append(managedResources, managed)
	}

	resource.Status.Composed = composed
	resource.Status.Outputs = collectOutputs(definition.Spec.Outputs, definition.Spec.Resources, managedResources)

	ready, reason, message := managedResourcesReady(managedResources)
	if !ready {
		if err := r.setCondition(ctx, &resource, metav1.ConditionFalse, reason, message); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: resourceRequeueInterval}, nil
	}

	return ctrl.Result{}, r.setCondition(ctx, &resource, metav1.ConditionTrue, resourceReasonAvailable, message)
}

func (r *ResourceReconciler) applyTemplate(
	ctx context.Context,
	resource *platformv1alpha1.Resource,
	target *platformv1alpha1.CloudTarget,
	template platformv1alpha1.ResourceTemplate,
) (*unstructured.Unstructured, platformv1alpha1.ComposedResource, error) {
	managed, err := buildManagedResource(resource, target, template, r.Scheme)
	if err != nil {
		return nil, platformv1alpha1.ComposedResource{}, err
	}

	desiredSpec, _ := managed.Object["spec"].(map[string]any)
	desiredLabels := managed.GetLabels()
	desiredAnnotations := managed.GetAnnotations()
	desiredOwnerReferences := managed.GetOwnerReferences()

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, managed, func() error {
		managed.Object["spec"] = desiredSpec
		managed.SetLabels(mergeStringMap(managed.GetLabels(), desiredLabels))
		managed.SetAnnotations(mergeStringMap(managed.GetAnnotations(), desiredAnnotations))
		managed.SetOwnerReferences(desiredOwnerReferences)
		return nil
	}); err != nil {
		return nil, platformv1alpha1.ComposedResource{}, err
	}

	if err := r.Get(ctx, client.ObjectKeyFromObject(managed), managed); err != nil {
		return nil, platformv1alpha1.ComposedResource{}, err
	}

	return managed, platformv1alpha1.ComposedResource{
		Name:       template.Name,
		APIVersion: template.APIVersion,
		Kind:       template.Kind,
		Namespace:  managed.GetNamespace(),
	}, nil
}

func (r *ResourceReconciler) setCondition(
	ctx context.Context,
	resource *platformv1alpha1.Resource,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type:               resourceConditionReady,
		Status:             status,
		ObservedGeneration: resource.Generation,
		Reason:             reason,
		Message:            message,
	})
	return r.Status().Update(ctx, resource)
}

func managedResourcesReady(resources []*unstructured.Unstructured) (bool, string, string) {
	for _, managed := range resources {
		condition, found := readyCondition(managed)
		if !found {
			return false, resourceReasonCreating, "Waiting for the managed resource to report Ready"
		}
		if condition.Status != metav1.ConditionTrue {
			reason := condition.Reason
			if reason == "" {
				reason = resourceReasonNotReady
			}
			return false, reason, condition.Message
		}
	}
	return true, resourceReasonAvailable, "All managed resources are ready"
}

func readyCondition(managed *unstructured.Unstructured) (metav1.Condition, bool) {
	conditions, found, err := unstructured.NestedSlice(managed.Object, "status", "conditions")
	if err != nil || !found {
		return metav1.Condition{}, false
	}
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok {
			continue
		}
		conditionType, _, _ := unstructured.NestedString(condition, "type")
		if conditionType != resourceConditionReady {
			continue
		}
		status, _, _ := unstructured.NestedString(condition, "status")
		reason, _, _ := unstructured.NestedString(condition, "reason")
		message, _, _ := unstructured.NestedString(condition, "message")
		return metav1.Condition{
			Type:    resourceConditionReady,
			Status:  metav1.ConditionStatus(status),
			Reason:  reason,
			Message: message,
		}, true
	}
	return metav1.Condition{}, false
}

func collectOutputs(
	outputs []platformv1alpha1.ResourceOutput,
	templates []platformv1alpha1.ResourceTemplate,
	resources []*unstructured.Unstructured,
) map[string]string {
	if len(outputs) == 0 || len(resources) == 0 {
		return nil
	}
	collected := make(map[string]string, len(outputs))
	for _, output := range outputs {
		index := 0
		if output.Template != "" {
			index = -1
			for i, template := range templates {
				if template.Name == output.Template {
					index = i
					break
				}
			}
			if index < 0 || index >= len(resources) {
				continue
			}
		}
		value, found, err := nestedValue(resources[index].Object, output.From)
		if err != nil || !found {
			continue
		}
		if text, ok := stringify(value); ok {
			collected[output.Name] = text
		}
	}
	if len(collected) == 0 {
		return nil
	}
	return collected
}

func stringify(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case bool:
		return strconv.FormatBool(typed), true
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case int64:
		return strconv.FormatInt(typed, 10), true
	case int32:
		return strconv.FormatInt(int64(typed), 10), true
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return "", false
		}
		return string(encoded), true
	}
}

func mergeStringMap(existing, desired map[string]string) map[string]string {
	if existing == nil {
		existing = map[string]string{}
	}
	for key, value := range desired {
		existing[key] = value
	}
	return existing
}

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.Resource{}).
		Named("resource").
		Complete(r)
}
