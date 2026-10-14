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
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
)

const probeInterval = 30 * time.Second

// ClusterReconciler probes remote clusters using credentials stored in the
// manager's namespace. APIReader intentionally bypasses the manager cache for
// Secret reads.
type ClusterReconciler struct {
	client.Client
	APIReader            client.Reader
	CredentialsNamespace string
	Scheme               *runtime.Scheme
}

// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=clusters/status,verbs=get;update;patch

func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cluster platformv1alpha1.Cluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	conditionStatus := metav1.ConditionFalse
	var reason, message string
	completedProbe := false
	connectedBefore := meta.FindStatusCondition(cluster.Status.Conditions, "Connected")
	staleInventory := connectedBefore == nil || connectedBefore.ObservedGeneration != cluster.Generation
	var secret corev1.Secret
	secretKey := types.NamespacedName{Namespace: r.CredentialsNamespace, Name: cluster.Spec.KubeconfigSecretRef.Name}
	var secretErr error
	if r.CredentialsNamespace != "" && secretKey.Name != "" && r.APIReader != nil {
		secretErr = r.APIReader.Get(ctx, secretKey, &secret)
		if ctx.Err() != nil {
			return ctrl.Result{}, ctx.Err()
		}
	}
	switch {
	case r.CredentialsNamespace == "":
		reason, message = "ConfigurationError", "Credentials namespace is not configured"
	case secretKey.Name == "":
		reason, message = "InvalidConfiguration", "Kubeconfig Secret name is not configured"
	case r.APIReader == nil:
		reason, message = "ConfigurationError", "Secret reader is not configured"
	case secretErr != nil:
		reason, message = "CredentialsUnavailable", "Kubeconfig Secret is unavailable"
	case len(secret.Data["kubeconfig"]) == 0:
		reason, message = "InvalidCredentials", "Kubeconfig Secret does not contain a kubeconfig"
	default:
		if err := probeKubeconfig(ctx, secret.Data["kubeconfig"]); err == nil {
			completedProbe = true
			conditionStatus, reason, message = metav1.ConditionTrue, "Connected", "Remote cluster authentication succeeded"
			if err := refreshNodeInventory(ctx, &cluster, secret.Data["kubeconfig"], r.CredentialsNamespace); err != nil {
				setNodesReadyCondition(&cluster, metav1.ConditionFalse, "NodeInventoryFailed", "Remote node inventory could not be refreshed")
				if staleInventory {
					cluster.Status.Nodes = nil
					cluster.Status.NodesObservedAt = nil
				}
			}
		} else {
			completedProbe = !errors.Is(err, errInvalidKubeconfig)
			reason, message = probeFailure(err)
		}
	}

	probeTime := metav1.Now()
	condition := metav1.Condition{
		Type: "Connected", Status: conditionStatus, ObservedGeneration: cluster.Generation,
		LastTransitionTime: probeTime, Reason: reason, Message: message,
	}
	meta.SetStatusCondition(&cluster.Status.Conditions, condition)
	if conditionStatus != metav1.ConditionTrue {
		setNodesReadyCondition(&cluster, metav1.ConditionFalse, "ConnectionFailed", "Remote node inventory could not be refreshed")
		if staleInventory {
			cluster.Status.Nodes = nil
			cluster.Status.NodesObservedAt = nil
		}
	}
	if completedProbe {
		cluster.Status.LastProbeTime = &probeTime
	}
	if err := r.Status().Update(ctx, &cluster); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: probeInterval}, nil
}

func refreshNodeInventory(ctx context.Context, cluster *platformv1alpha1.Cluster, kubeconfig []byte, credentialsNamespace string) error {
	config, _, err := parseRemoteClusterConfig(kubeconfig, credentialsNamespace)
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	summary := make([]platformv1alpha1.ClusterNode, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		ready := applicationWorkloadUnknown
		for _, condition := range node.Status.Conditions {
			if condition.Type != corev1.NodeReady {
				continue
			}
			switch condition.Status {
			case corev1.ConditionTrue:
				ready = applicationWorkloadTrue
			case corev1.ConditionFalse:
				ready = applicationWorkloadFalse
			default:
				ready = applicationWorkloadUnknown
			}
			break
		}
		summary = append(summary, platformv1alpha1.ClusterNode{Name: node.Name, Ready: ready})
	}
	slices.SortFunc(summary, func(a, b platformv1alpha1.ClusterNode) int { return cmp.Compare(a.Name, b.Name) })
	observedAt := metav1.Now()
	cluster.Status.Nodes = summary
	cluster.Status.NodesObservedAt = &observedAt
	setNodesReadyCondition(cluster, metav1.ConditionTrue, "InventoryRefreshed", "Remote node inventory was refreshed")
	return nil
}

func setNodesReadyCondition(cluster *platformv1alpha1.Cluster, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type: "NodesReady", Status: status, ObservedGeneration: cluster.Generation,
		LastTransitionTime: metav1.Now(), Reason: reason, Message: message,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.Cluster{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("cluster").
		Complete(r)
}
