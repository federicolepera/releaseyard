package controller

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

const applicationRolloutInProgressReason = "RolloutInProgress"

// collectApplicationResources records identities only: chart resources and the
// live controller-owned descendants that can be reached from those resources.
func collectApplicationResources(ctx context.Context, cfg *rest.Config, manifest, releaseNamespace string) ([]platformv1alpha1.ApplicationResource, error) {
	if cfg == nil {
		return nil, fmt.Errorf("remote Kubernetes REST config is required")
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create remote dynamic client: %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create remote discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	return collectApplicationResourcesWithClients(ctx, dc, mapper, manifest, releaseNamespace)
}

type inventoryIdentity struct {
	apiVersion  string
	kind        string
	namespace   string
	name        string
	uid         types.UID
	parentUID   types.UID
	deployment  *platformv1alpha1.ApplicationDeploymentHealth
	pod         *platformv1alpha1.ApplicationPodHealth
	statefulSet *platformv1alpha1.ApplicationStatefulSetHealth
	daemonSet   *platformv1alpha1.ApplicationDaemonSetHealth
	job         *platformv1alpha1.ApplicationJobHealth
}

type inventoryManifestObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
}

func collectApplicationResourcesWithClients(ctx context.Context, client dynamic.Interface, mapper meta.RESTMapper, manifest, releaseNamespace string) ([]platformv1alpha1.ApplicationResource, error) {
	roots, err := parseInventoryManifest(manifest, releaseNamespace)
	if err != nil {
		return nil, err
	}
	resultByKey, rootIdentities, namespaces, err := collectRootInventory(ctx, client, mapper, roots, releaseNamespace)
	if err != nil {
		return nil, err
	}
	if err := collectInventoryDescendants(ctx, client, resultByKey, rootIdentities, namespaces); err != nil {
		return nil, err
	}
	filterPodHealthProjections(resultByKey)
	foldOwnedPodHealth(resultByKey)
	result := make([]platformv1alpha1.ApplicationResource, 0, len(resultByKey))
	for _, resource := range resultByKey {
		result = append(result, resource)
	}
	slices.SortFunc(result, func(a, b platformv1alpha1.ApplicationResource) int {
		if a.Namespace != b.Namespace {
			return strings.Compare(a.Namespace, b.Namespace)
		}
		if a.APIVersion != b.APIVersion {
			return strings.Compare(a.APIVersion, b.APIVersion)
		}
		if a.Kind != b.Kind {
			return strings.Compare(a.Kind, b.Kind)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return result, nil
}

func collectRootInventory(ctx context.Context, client dynamic.Interface, mapper meta.RESTMapper, roots []inventoryIdentity, releaseNamespace string) (map[string]platformv1alpha1.ApplicationResource, []inventoryIdentity, map[string]struct{}, error) {
	resultByKey := make(map[string]platformv1alpha1.ApplicationResource)
	rootIdentities := make([]inventoryIdentity, 0, len(roots))
	namespaces := make(map[string]struct{})
	for _, root := range roots {
		gvr, namespaced, err := inventoryMapping(mapper, root.apiVersion, root.kind)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("map chart resource %s %s: %w", root.kind, root.name, err)
		}
		ns := root.namespace
		if namespaced && ns == "" {
			ns = releaseNamespace
		}
		if namespaced && ns == "" {
			return nil, nil, nil, fmt.Errorf("chart resource %s %s has no namespace", root.kind, root.name)
		}
		if namespaced {
			root.namespace = ns
			namespaces[ns] = struct{}{}
		} else {
			root.namespace = ""
		}
		resourceClient := client.Resource(gvr)
		var obj *unstructured.Unstructured
		var getErr error
		if namespaced {
			obj, getErr = resourceClient.Namespace(ns).Get(ctx, root.name, metav1.GetOptions{})
		} else {
			obj, getErr = resourceClient.Get(ctx, root.name, metav1.GetOptions{})
		}
		if getErr != nil && !errors.IsNotFound(getErr) {
			return nil, nil, nil, fmt.Errorf("get chart resource %s %s: %w", root.kind, root.name, getErr)
		}
		if getErr == nil {
			root.uid = obj.GetUID()
		}
		projectRootHealth(&root, obj, getErr == nil)
		rootIdentities = append(rootIdentities, root)
		addInventoryIdentity(resultByKey, root)
	}
	return resultByKey, rootIdentities, namespaces, nil
}

func collectInventoryDescendants(ctx context.Context, client dynamic.Interface, resultByKey map[string]platformv1alpha1.ApplicationResource, rootIdentities []inventoryIdentity, namespaces map[string]struct{}) error {
	// Only query controller resource kinds relevant to chart roots, and only in their namespaces.
	needRS, needJobs, needPods := false, false, false
	for _, root := range rootIdentities {
		switch root.kind {
		case applicationWorkloadDeploymentKind:
			needRS, needPods = true, true
		case applicationWorkloadStatefulSetKind, applicationWorkloadDaemonSetKind, applicationWorkloadReplicaSetKind, applicationWorkloadJobKind:
			needPods = true
		case applicationWorkloadCronJobKind:
			needJobs, needPods = true, true
		}
	}
	parents := append([]inventoryIdentity(nil), rootIdentities...)
	if needRS {
		children, err := listOwnedInventoryObjects(ctx, client, schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}, "apps/v1", "ReplicaSet", namespaces, parents)
		if err != nil {
			return err
		}
		for _, child := range children {
			addInventoryIdentity(resultByKey, child)
		}
		parents = append(parents, children...)
	}
	if needJobs {
		children, err := listOwnedInventoryObjects(ctx, client, schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, "batch/v1", applicationWorkloadJobKind, namespaces, parents)
		if err != nil {
			return err
		}
		for _, child := range children {
			addInventoryIdentity(resultByKey, child)
		}
		parents = append(parents, children...)
	}
	if needPods {
		children, err := listOwnedInventoryObjects(ctx, client, schema.GroupVersionResource{Version: "v1", Resource: "pods"}, "v1", "Pod", namespaces, parents)
		if err != nil {
			return err
		}
		for _, child := range children {
			addInventoryIdentity(resultByKey, child)
		}
	}
	return nil
}

func filterPodHealthProjections(resultByKey map[string]platformv1alpha1.ApplicationResource) {
	rootDeploymentUIDs, rootStatefulSetUIDs, rootDaemonSetUIDs, observedJobUIDs := map[types.UID]struct{}{}, map[types.UID]struct{}{}, map[types.UID]struct{}{}, map[types.UID]struct{}{}
	replicaSetDeploymentUIDs := make(map[types.UID]types.UID)
	for _, resource := range resultByKey {
		if resource.APIVersion == "apps/v1" && resource.Kind == applicationWorkloadDeploymentKind && resource.UID != "" {
			rootDeploymentUIDs[resource.UID] = struct{}{}
		}
		if resource.APIVersion == "apps/v1" && resource.Kind == applicationWorkloadStatefulSetKind && resource.UID != "" {
			rootStatefulSetUIDs[resource.UID] = struct{}{}
		}
		if resource.APIVersion == "apps/v1" && resource.Kind == applicationWorkloadDaemonSetKind && resource.UID != "" {
			rootDaemonSetUIDs[resource.UID] = struct{}{}
		}
		if resource.APIVersion == "batch/v1" && resource.Kind == applicationWorkloadJobKind && resource.Job != nil && resource.UID != "" {
			observedJobUIDs[resource.UID] = struct{}{}
		}
		if resource.APIVersion == "apps/v1" && resource.Kind == applicationWorkloadReplicaSetKind {
			replicaSetDeploymentUIDs[resource.UID] = resource.ParentUID
		}
	}
	for key, resource := range resultByKey {
		if resource.APIVersion != "v1" || resource.Kind != applicationWorkloadPodKind || resource.Pod == nil || resource.ParentUID == "" {
			continue
		}
		deploymentUID, ok := replicaSetDeploymentUIDs[resource.ParentUID]
		_, deploymentParent := rootDeploymentUIDs[deploymentUID]
		_, statefulParent := rootStatefulSetUIDs[resource.ParentUID]
		_, daemonParent := rootDaemonSetUIDs[resource.ParentUID]
		_, jobParent := observedJobUIDs[resource.ParentUID]
		if !(ok && deploymentParent) && !statefulParent && !daemonParent && !jobParent {
			resource.Pod = nil
		}
		resultByKey[key] = resource
	}
}

func foldOwnedPodHealth(resources map[string]platformv1alpha1.ApplicationResource) {
	rootKeys := make(map[types.UID]string)
	replicaSetParents := make(map[types.UID]types.UID)
	for key, resource := range resources {
		if resource.UID == "" {
			continue
		}
		if resource.APIVersion == "apps/v1" && ((resource.Kind == applicationWorkloadDeploymentKind && resource.Deployment != nil) || (resource.Kind == applicationWorkloadStatefulSetKind && resource.StatefulSet != nil) || (resource.Kind == applicationWorkloadDaemonSetKind && resource.DaemonSet != nil)) {
			rootKeys[resource.UID] = key
		}
		if resource.APIVersion == "apps/v1" && resource.Kind == applicationWorkloadReplicaSetKind {
			replicaSetParents[resource.UID] = resource.ParentUID
		}
	}
	for _, pod := range resources {
		if pod.APIVersion != "v1" || pod.Kind != applicationWorkloadPodKind || pod.Pod == nil || pod.Pod.Reason == "PodTerminating" || pod.ParentUID == "" {
			continue
		}
		parentUID := pod.ParentUID
		if deploymentUID, ok := replicaSetParents[parentUID]; ok {
			parentUID = deploymentUID
		}
		parentKey, ok := rootKeys[parentUID]
		if !ok {
			continue
		}
		parent := resources[parentKey]
		foldPodStateIntoParent(&parent, pod.Pod.State)
		resources[parentKey] = parent
	}
}

func foldPodStateIntoParent(parent *platformv1alpha1.ApplicationResource, podState string) {
	state, reason := parentWorkloadState(parent)
	if podState == applicationWorkloadDegraded {
		state, reason = applicationWorkloadDegraded, "PodFailure"
	} else if state == applicationWorkloadHealthy && podState == applicationWorkloadProgressing {
		state, reason = applicationWorkloadProgressing, "PodProgressing"
	} else if state != applicationWorkloadDegraded && podState == applicationWorkloadUnknown {
		state, reason = applicationWorkloadUnknown, applicationMalformedStatusReason
	} else {
		return
	}
	setParentWorkloadState(parent, state, reason)
}

func parentWorkloadState(parent *platformv1alpha1.ApplicationResource) (string, string) {
	switch {
	case parent.Deployment != nil:
		return parent.Deployment.State, parent.Deployment.Reason
	case parent.StatefulSet != nil:
		return parent.StatefulSet.State, parent.StatefulSet.Reason
	case parent.DaemonSet != nil:
		return parent.DaemonSet.State, parent.DaemonSet.Reason
	default:
		return applicationWorkloadUnknown, ""
	}
}

func setParentWorkloadState(parent *platformv1alpha1.ApplicationResource, state, reason string) {
	switch {
	case parent.Deployment != nil:
		parent.Deployment.State, parent.Deployment.Reason = state, reason
	case parent.StatefulSet != nil:
		parent.StatefulSet.State, parent.StatefulSet.Reason = state, reason
	case parent.DaemonSet != nil:
		parent.DaemonSet.State, parent.DaemonSet.Reason = state, reason
	}
}

func parseInventoryManifest(manifest, defaultNamespace string) ([]inventoryIdentity, error) {
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	seen := make(map[string]struct{})
	var roots []inventoryIdentity
	for {
		var obj inventoryManifestObject
		if err := decoder.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("decode Helm manifest resource: %w", err)
		}
		if obj.APIVersion == "" || obj.Kind == "" || obj.Metadata.Name == "" {
			continue
		}
		if obj.Metadata.Namespace == "" {
			obj.Metadata.Namespace = defaultNamespace
		}
		root := inventoryIdentity{apiVersion: obj.APIVersion, kind: obj.Kind, namespace: obj.Metadata.Namespace, name: obj.Metadata.Name}
		key := inventoryIdentityKey(root)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		roots = append(roots, root)
	}
	return roots, nil
}

func inventoryMapping(mapper meta.RESTMapper, apiVersion, kind string) (schema.GroupVersionResource, bool, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionResource{}, false, err
	}
	mapping, err := mapper.RESTMapping(gv.WithKind(kind).GroupKind(), gv.Version)
	if err != nil {
		return schema.GroupVersionResource{}, false, err
	}
	return mapping.Resource, mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

func listOwnedInventoryObjects(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, apiVersion, kind string, namespaces map[string]struct{}, parents []inventoryIdentity) ([]inventoryIdentity, error) {
	parentKeys := make(map[string]inventoryIdentity, len(parents))
	for _, parent := range parents {
		if parent.uid != "" {
			parentKeys[inventoryOwnerKey(parent.apiVersion, parent.kind, parent.name, parent.uid)] = parent
		}
	}
	if len(parentKeys) == 0 {
		return nil, nil
	}
	nss := make([]string, 0, len(namespaces))
	for ns := range namespaces {
		nss = append(nss, ns)
	}
	slices.Sort(nss)
	var found []inventoryIdentity
	for _, ns := range nss {
		list, err := client.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list %s in namespace %s: %w", kind, ns, err)
		}
		for i := range list.Items {
			obj := &list.Items[i]
			if obj.GetName() == "" {
				continue
			}
			var matched *inventoryIdentity
			for _, owner := range obj.GetOwnerReferences() {
				if owner.Controller == nil || !*owner.Controller {
					continue
				}
				if parent, ok := parentKeys[inventoryOwnerKey(owner.APIVersion, owner.Kind, owner.Name, owner.UID)]; ok && parent.namespace == obj.GetNamespace() {
					p := parent
					matched = &p
					break
				}
			}
			if matched == nil {
				continue
			}
			child := inventoryIdentity{apiVersion: apiVersion, kind: kind, namespace: obj.GetNamespace(), name: obj.GetName(), uid: obj.GetUID(), parentUID: matched.uid}
			if kind == applicationWorkloadPodKind {
				child.pod = podHealth(obj)
			}
			if apiVersion == "batch/v1" && kind == applicationWorkloadJobKind && matched.apiVersion == "batch/v1" && matched.kind == applicationWorkloadCronJobKind {
				child.job = jobHealth(obj, true, child.uid != "")
			}
			found = append(found, child)
		}
	}
	return found, nil
}

func addInventoryIdentity(dst map[string]platformv1alpha1.ApplicationResource, identity inventoryIdentity) {
	key := inventoryIdentityKey(identity)
	if _, exists := dst[key]; exists {
		return
	}
	dst[key] = platformv1alpha1.ApplicationResource{APIVersion: identity.apiVersion, Kind: identity.kind, Namespace: identity.namespace, Name: identity.name, UID: identity.uid, ParentUID: identity.parentUID, Deployment: identity.deployment, Pod: identity.pod, StatefulSet: identity.statefulSet, DaemonSet: identity.daemonSet, Job: identity.job}
}

func projectRootHealth(root *inventoryIdentity, obj *unstructured.Unstructured, exists bool) {
	if root.apiVersion == "apps/v1" {
		switch root.kind {
		case applicationWorkloadStatefulSetKind:
			root.statefulSet = statefulSetHealth(obj, exists, root.uid != "")
		case applicationWorkloadDaemonSetKind:
			root.daemonSet = daemonSetHealth(obj, exists, root.uid != "")
		}
	}
	if root.apiVersion == "batch/v1" && root.kind == applicationWorkloadJobKind {
		root.job = jobHealth(obj, exists, root.uid != "")
	}
	if root.apiVersion == "apps/v1" && root.kind == applicationWorkloadDeploymentKind {
		if exists {
			root.deployment = deploymentHealth(obj)
		} else {
			root.deployment = &platformv1alpha1.ApplicationDeploymentHealth{State: applicationWorkloadProgressing, Reason: applicationResourceMissingReason}
		}
	}
	if root.apiVersion == "v1" && root.kind == applicationWorkloadPodKind && exists {
		root.pod = podHealth(obj)
	}
}

func optionalInt32(obj *unstructured.Unstructured, fields ...string) (*int32, bool) {
	value, found, err := unstructured.NestedInt64(obj.Object, fields...)
	if err != nil || value > int64(^uint32(0)>>1) || value < -int64(^uint32(0)>>1)-1 {
		return nil, true
	}
	if !found {
		return nil, false
	}
	result := int32(value)
	return &result, false
}

func int32PointerValue(value int32) *int32 { return &value }

func statefulSetHealth(obj *unstructured.Unstructured, exists, hasUID bool) *platformv1alpha1.ApplicationStatefulSetHealth {
	if !exists {
		return &platformv1alpha1.ApplicationStatefulSetHealth{State: applicationWorkloadProgressing, Reason: applicationResourceMissingReason}
	}
	desired, malformedDesired := optionalInt32(obj, applicationSpecField, applicationDefinitionReplicasPath)
	if desired == nil && !malformedDesired {
		one := int32(1)
		desired = &one
	}
	ready, malformedReady := optionalInt32(obj, "status", "readyReplicas")
	replicas, malformedReplicas := optionalInt32(obj, "status", "replicas")
	state, reason := applicationWorkloadProgressing, applicationRolloutInProgressReason
	observed, observedFound, observedErr := unstructured.NestedInt64(obj.Object, "status", applicationObservedGenerationField)
	if malformedDesired || malformedReady || malformedReplicas || observedErr != nil {
		state, reason = applicationWorkloadUnknown, applicationMalformedStatusReason
	} else if observedFound && obj.GetGeneration() > 0 && observed >= obj.GetGeneration() && hasUID && desired != nil {
		if *desired == 0 {
			if ready == nil {
				ready = int32PointerValue(0)
			}
			if replicas == nil {
				replicas = int32PointerValue(0)
			}
		}
		if ready != nil && replicas != nil && *desired == *ready && *desired == *replicas {
			state, reason = applicationWorkloadHealthy, ""
		}
	}
	return &platformv1alpha1.ApplicationStatefulSetHealth{DesiredReplicas: desired, ReadyReplicas: ready, State: state, Reason: reason}
}

func daemonSetHealth(obj *unstructured.Unstructured, exists, hasUID bool) *platformv1alpha1.ApplicationDaemonSetHealth {
	if !exists {
		return &platformv1alpha1.ApplicationDaemonSetHealth{State: applicationWorkloadProgressing, Reason: applicationResourceMissingReason}
	}
	desired, malformedDesired := optionalInt32(obj, "status", "desiredNumberScheduled")
	ready, malformedReady := optionalInt32(obj, "status", "numberReady")
	current, malformedCurrent := optionalInt32(obj, "status", "currentNumberScheduled")
	misscheduled, malformedMisscheduled := optionalInt32(obj, "status", "numberMisscheduled")
	state, reason := applicationWorkloadProgressing, applicationRolloutInProgressReason
	observed, observedFound, observedErr := unstructured.NestedInt64(obj.Object, "status", applicationObservedGenerationField)
	if malformedDesired || malformedReady || malformedCurrent || malformedMisscheduled || observedErr != nil {
		state, reason = applicationWorkloadUnknown, applicationMalformedStatusReason
	} else if observedFound && obj.GetGeneration() > 0 && observed >= obj.GetGeneration() && hasUID {
		if desired == nil {
			desired = int32PointerValue(0)
		}
		if *desired == 0 {
			if ready == nil {
				ready = int32PointerValue(0)
			}
			if current == nil {
				current = int32PointerValue(0)
			}
		}
		if *desired == 0 && (ready == nil || *ready == 0) && (current == nil || *current == 0) && (misscheduled == nil || *misscheduled == 0) {
			state, reason = applicationWorkloadHealthy, "NoEligibleNodes"
		} else if ready != nil && current != nil && *desired == *ready && *desired == *current && (misscheduled == nil || *misscheduled == 0) {
			state, reason = applicationWorkloadHealthy, ""
		}
	}
	return &platformv1alpha1.ApplicationDaemonSetHealth{DesiredNumberScheduled: desired, NumberReady: ready, State: state, Reason: reason}
}

func jobHealth(obj *unstructured.Unstructured, exists, hasUID bool) *platformv1alpha1.ApplicationJobHealth {
	if !exists {
		return &platformv1alpha1.ApplicationJobHealth{State: applicationWorkloadProgressing, Reason: applicationResourceMissingReason}
	}
	completions, badCompletions := optionalInt32(obj, applicationSpecField, "completions")
	active, badActive := optionalInt32(obj, "status", "active")
	succeeded, badSucceeded := optionalInt32(obj, "status", "succeeded")
	failed, badFailed := optionalInt32(obj, "status", "failed")
	state, reason := applicationWorkloadProgressing, applicationJobRunningReason
	malformed := badCompletions || badActive || badSucceeded || badFailed
	if malformed {
		state, reason = applicationWorkloadUnknown, applicationMalformedStatusReason
	}
	conditions, _, conditionErr := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if conditionErr != nil {
		malformed = true
		state, reason = applicationWorkloadUnknown, applicationMalformedStatusReason
	}
	complete, finishing, failedCondition := false, false, ""
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok {
			malformed = true
			state, reason = applicationWorkloadUnknown, applicationMalformedStatusReason
			continue
		}
		typeName, _, _ := unstructured.NestedString(condition, "type")
		status, _, _ := unstructured.NestedString(condition, applicationStatusField)
		if status != applicationWorkloadTrue {
			continue
		}
		switch typeName {
		case applicationJobCompleteReason:
			complete = true
		case applicationJobFailedConditionType, applicationJobFailureTargetType:
			failedCondition = typeName
		case "SuccessCriteriaMet":
			finishing = true
		}
	}
	if malformed {
		state, reason = applicationWorkloadUnknown, applicationMalformedStatusReason
	} else if complete {
		state, reason = applicationWorkloadHealthy, applicationJobCompleteReason
	} else if failedCondition != "" {
		state, reason = applicationWorkloadDegraded, failedCondition
	} else if state != applicationWorkloadUnknown {
		if finishing {
			reason = applicationJobFinishingReason
		}
		if suspended, found, err := unstructured.NestedBool(obj.Object, applicationSpecField, "suspend"); err != nil {
			state, reason = applicationWorkloadUnknown, "MalformedSpec"
		} else if found && suspended {
			reason = applicationJobSuspendedReason
		} else if parallelism, found, err := unstructured.NestedInt64(obj.Object, applicationSpecField, "parallelism"); err != nil {
			state, reason = applicationWorkloadUnknown, "MalformedSpec"
		} else if found && parallelism == 0 {
			reason = applicationJobPausedReason
		}
	}
	if !hasUID {
		state, reason = applicationWorkloadProgressing, applicationResourceMissingReason
	}
	return &platformv1alpha1.ApplicationJobHealth{Completions: completions, Active: active, Succeeded: succeeded, Failed: failed, State: state, Reason: reason}
}

func deploymentHealth(obj *unstructured.Unstructured) *platformv1alpha1.ApplicationDeploymentHealth {
	desired, found, _ := unstructured.NestedInt64(obj.Object, applicationSpecField, applicationDefinitionReplicasPath)
	if !found {
		desired = 1
	}
	ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyReplicas")
	state, reason := applicationWorkloadProgressing, applicationRolloutInProgressReason
	observed, observedFound, _ := unstructured.NestedInt64(obj.Object, "status", applicationObservedGenerationField)
	generation := obj.GetGeneration()
	if observedFound && generation > 0 && observed >= generation {
		replicas, _, _ := unstructured.NestedInt64(obj.Object, "status", "replicas")
		updated, _, _ := unstructured.NestedInt64(obj.Object, "status", "updatedReplicas")
		available, _, _ := unstructured.NestedInt64(obj.Object, "status", "availableReplicas")
		if desired == replicas && desired == updated && desired == ready && desired == available && obj.GetUID() != "" {
			state, reason = applicationWorkloadHealthy, ""
		}
	}
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, item := range conditions {
		condition, _ := item.(map[string]any)
		typeName, _, _ := unstructured.NestedString(condition, "type")
		status, _, _ := unstructured.NestedString(condition, "status")
		reasonValue, _, _ := unstructured.NestedString(condition, applicationReasonField)
		if typeName == applicationWorkloadProgressing && reasonValue == "ProgressDeadlineExceeded" && status == applicationWorkloadFalse {
			state, reason = applicationWorkloadDegraded, "ProgressDeadlineExceeded"
		}
		if typeName == "ReplicaFailure" && status == applicationWorkloadTrue {
			state, reason = applicationWorkloadDegraded, "ReplicaFailure"
		}
	}
	return &platformv1alpha1.ApplicationDeploymentHealth{DesiredReplicas: int32(desired), ReadyReplicas: int32(ready), State: state, Reason: reason}
}

func podHealth(obj *unstructured.Unstructured) *platformv1alpha1.ApplicationPodHealth {
	ready := applicationWorkloadUnknown
	state, reason, message := applicationWorkloadProgressing, "PodStarting", "Pod is starting"
	if obj.GetDeletionTimestamp() != nil {
		return &platformv1alpha1.ApplicationPodHealth{Ready: ready, State: applicationWorkloadProgressing, Reason: "PodTerminating", Message: "Pod is terminating"}
	}
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	if phase == "Succeeded" {
		return &platformv1alpha1.ApplicationPodHealth{Ready: "False", State: "Healthy"}
	}
	if phase == applicationJobFailedConditionType {
		state, reason, message = applicationWorkloadDegraded, "PodFailed", "Pod has failed"
	}
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, item := range conditions {
		c, _ := item.(map[string]any)
		t, _, _ := unstructured.NestedString(c, "type")
		s, _, _ := unstructured.NestedString(c, "status")
		if t == "Ready" {
			switch s {
			case applicationWorkloadTrue, applicationWorkloadFalse:
				ready = s
			default:
				ready = "Unknown"
			}
		}
		reasonValue, _, _ := unstructured.NestedString(c, "reason")
		if t == "PodScheduled" && s == applicationWorkloadFalse && reasonValue == "Unschedulable" {
			state, reason, message = "Degraded", "Unschedulable", "Pod cannot be scheduled"
		}
	}
	for _, statusField := range []string{"initContainerStatuses", "containerStatuses"} {
		statuses, _, _ := unstructured.NestedSlice(obj.Object, "status", statusField)
		for _, item := range statuses {
			cs, _ := item.(map[string]any)
			waiting, _, _ := unstructured.NestedMap(cs, "state", "waiting")
			waitReason, _, _ := unstructured.NestedString(waiting, "reason")
			switch waitReason {
			case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError":
				state, reason, message = "Degraded", waitReason, "Pod container is unable to start"
			}
		}
	}
	if ready == applicationWorkloadTrue && state != applicationWorkloadDegraded {
		state, reason, message = applicationWorkloadHealthy, "", ""
	}
	return &platformv1alpha1.ApplicationPodHealth{Ready: ready, State: state, Reason: reason, Message: message}
}

func inventoryIdentityKey(i inventoryIdentity) string {
	return i.apiVersion + "\x00" + i.kind + "\x00" + i.namespace + "\x00" + i.name
}
func inventoryOwnerKey(apiVersion, kind, name string, uid types.UID) string {
	return apiVersion + "\x00" + kind + "\x00" + name + "\x00" + string(uid)
}
