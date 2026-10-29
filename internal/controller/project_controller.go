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
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	"github.com/federicolepera/idp/internal/githubrepo"
)

const (
	projectRetryInterval           = 30 * time.Second
	projectRejectedRetryInterval   = 5 * time.Minute
	projectForbiddenRetryInterval  = 30 * time.Minute
	projectMarkerPrefix            = "[releaseyard-project-uid:"
	projectReasonConflict          = "Conflict"
	argocdApplicationManagedBy     = "releaseyard"
	argocdApplicationManagedLabel  = "platform.releaseyard.io/managed-by"
	argocdProjectLabel             = "platform.releaseyard.io/project"
	argocdProjectUIDAnnotation     = "platform.releaseyard.io/project-uid"
	argocdApplicationPath          = "apps"
	argocdDestinationServer        = "https://kubernetes.default.svc"
	argocdNamespaceField           = "namespace"
	projectNamespaceManagedByLabel = "app.kubernetes.io/managed-by"
	projectNamespaceDeliveryLabel  = "platform.releaseyard.io/delivery"
)

var errRequeue = errors.New("requeue")

type projectGit interface {
	GetRepository(context.Context, string, string) (githubrepo.Repository, error)
	CreateRepository(context.Context, githubrepo.CreateRepositoryRequest) (githubrepo.Repository, error)
	SeedRepository(context.Context, string, string, string, map[string]string) error
}

// ProjectReconciler provisions and periodically verifies private Git repositories.
type ProjectReconciler struct {
	client.Client
	APIReader                  client.Reader
	CredentialsNamespace       string
	Scheme                     *runtime.Scheme
	GitHubFactory              func(token string) projectGit
	ArgoCDNamespace            string
	ArgoCDDestinationNamespace string
}

// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=projects,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=projects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=gitconnections,verbs=get
// +kubebuilder:rbac:groups=argoproj.io,resources=applications,verbs=get;list;watch;create;update;patch

func (r *ProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var project platformv1alpha1.Project
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, types.NamespacedName{Name: req.Name}, &project); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !project.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	setCondition := func(status metav1.ConditionStatus, reason, message string) error {
		meta.SetStatusCondition(&project.Status.Conditions, metav1.Condition{Type: platformv1alpha1.ProjectConditionRepositoryReady, Status: status, ObservedGeneration: project.Generation, LastTransitionTime: metav1.Now(), Reason: reason, Message: message})
		return r.Client.Status().Update(ctx, &project)
	}
	if project.UID == "" {
		return ctrl.Result{}, setCondition(metav1.ConditionFalse, "InvalidProject", "Project UID is not assigned")
	}
	namespaceReady, namespaceResult, err := r.reconcileProjectNamespace(ctx, &project)
	if err != nil || !namespaceReady {
		return namespaceResult, err
	}
	project, err = r.freshProject(ctx, project.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if strings.Contains(project.Spec.Description, projectMarkerPrefix) {
		return ctrl.Result{}, setCondition(metav1.ConditionFalse, "InvalidDescription", "Description contains a reserved project marker")
	}
	marker := projectMarkerPrefix + string(project.UID) + "]"
	description := project.Spec.Description
	if description != "" {
		description += " "
	}
	description += marker
	if len([]rune(description)) > 350 {
		return ctrl.Result{}, setCondition(metav1.ConditionFalse, "InvalidDescription", "Description and project marker exceed GitHub's 350 character limit")
	}
	if r.CredentialsNamespace == "" || r.APIReader == nil {
		return ctrl.Result{RequeueAfter: projectRetryInterval}, setCondition(metav1.ConditionFalse, "CredentialsUnavailable", "Git credentials are not configured")
	}
	connectionNamespace := project.Spec.Repository.ConnectionRef.Namespace
	if connectionNamespace == "" {
		connectionNamespace = r.CredentialsNamespace
	}
	var connection platformv1alpha1.GitConnection
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: connectionNamespace, Name: project.Spec.Repository.ConnectionRef.Name}, &connection); err != nil {
		return ctrl.Result{RequeueAfter: projectRetryInterval}, setCondition(metav1.ConditionFalse, "ConnectionUnavailable", "Git connection is unavailable")
	}
	if connection.UID == "" {
		return ctrl.Result{RequeueAfter: projectRetryInterval}, setCondition(metav1.ConditionFalse, "ConnectionUnavailable", "Git connection UID is not assigned")
	}
	if connection.Spec.Provider != platformv1alpha1.GitProviderGitHub || connection.Spec.GitHub == nil {
		return ctrl.Result{RequeueAfter: projectRetryInterval}, setCondition(metav1.ConditionFalse, "UnsupportedProvider", "Git provider is not supported or is not configured")
	}
	repoName := project.Spec.Repository.Name
	if repoName == "" {
		repoName = project.Name
	}
	var secret corev1.Secret
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: connectionNamespace, Name: connection.Spec.TokenSecretRef.Name}, &secret); err != nil {
		return ctrl.Result{RequeueAfter: projectRetryInterval}, setCondition(metav1.ConditionFalse, "CredentialsUnavailable", "Git token Secret or token key is unavailable")
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		return ctrl.Result{RequeueAfter: projectRetryInterval}, setCondition(metav1.ConditionFalse, "CredentialsUnavailable", "Git token Secret or token key is unavailable")
	}
	factory := r.GitHubFactory
	if factory == nil {
		factory = func(token string) projectGit { return githubrepo.NewClient(token) }
	}
	gh := factory(token)
	return r.reconcileConnection(ctx, &project, &connection, repoName, marker, description, gh)
}

func (r *ProjectReconciler) reconcileProjectNamespace(ctx context.Context, project *platformv1alpha1.Project) (bool, ctrl.Result, error) {
	if project.Spec.Namespace == "" {
		err := r.setNamespaceCondition(ctx, project.Name, metav1.ConditionFalse, "InvalidNamespace", "Project namespace is required")
		return false, ctrl.Result{RequeueAfter: projectRetryInterval}, err
	}
	if errs := validation.IsDNS1123Label(project.Spec.Namespace); len(errs) > 0 {
		err := r.setNamespaceCondition(ctx, project.Name, metav1.ConditionFalse, "InvalidNamespace", "Project namespace is not a valid DNS-1123 label")
		return false, ctrl.Result{RequeueAfter: projectRetryInterval}, err
	}

	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var projects platformv1alpha1.ProjectList
	if err := reader.List(ctx, &projects); err != nil {
		conditionErr := r.setNamespaceCondition(ctx, project.Name, metav1.ConditionUnknown, "NamespaceUnavailable", "Projects could not be checked for namespace conflicts")
		return false, ctrl.Result{RequeueAfter: projectRetryInterval}, conditionErr
	}
	for i := range projects.Items {
		other := &projects.Items[i]
		if other.Name == project.Name || other.Spec.Namespace != project.Spec.Namespace {
			continue
		}
		conditionErr := r.setNamespaceCondition(ctx, project.Name, metav1.ConditionFalse, "NamespaceConflict", fmt.Sprintf("Namespace %q is already assigned to Project %q", project.Spec.Namespace, other.Name))
		return false, ctrl.Result{RequeueAfter: projectRetryInterval}, conditionErr
	}

	namespace := &corev1.Namespace{}
	key := types.NamespacedName{Name: project.Spec.Namespace}
	if err := reader.Get(ctx, key, namespace); err != nil {
		if !apierrors.IsNotFound(err) {
			conditionErr := r.setNamespaceCondition(ctx, project.Name, metav1.ConditionUnknown, "NamespaceUnavailable", "Project namespace could not be checked")
			return false, ctrl.Result{RequeueAfter: projectRetryInterval}, conditionErr
		}
		desired := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   project.Spec.Namespace,
			Labels: projectNamespaceLabels(project),
		}}
		if err := r.Create(ctx, desired); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return r.reconcileProjectNamespace(ctx, project)
			}
			conditionErr := r.setNamespaceCondition(ctx, project.Name, metav1.ConditionFalse, "NamespaceUnavailable", "Project namespace could not be created")
			return false, ctrl.Result{RequeueAfter: projectRetryInterval}, conditionErr
		}
		conditionErr := r.setNamespaceCondition(ctx, project.Name, metav1.ConditionTrue, "NamespaceReady", fmt.Sprintf("Namespace %q is ready", project.Spec.Namespace))
		return conditionErr == nil, ctrl.Result{}, conditionErr
	}

	owner := namespace.Labels[argocdProjectLabel]
	managedBy := namespace.Labels[projectNamespaceManagedByLabel]
	if owner != "" && owner != project.Name {
		conditionErr := r.setNamespaceCondition(ctx, project.Name, metav1.ConditionFalse, "NamespaceConflict", fmt.Sprintf("Namespace %q is owned by Project %q", project.Spec.Namespace, owner))
		return false, ctrl.Result{RequeueAfter: projectRetryInterval}, conditionErr
	}
	if managedBy != "" && managedBy != argocdApplicationManagedBy {
		conditionErr := r.setNamespaceCondition(ctx, project.Name, metav1.ConditionFalse, "NamespaceUnavailable", fmt.Sprintf("Namespace %q is managed by %q, not ReleaseYard", project.Spec.Namespace, managedBy))
		return false, ctrl.Result{RequeueAfter: projectRetryInterval}, conditionErr
	}
	if owner == "" && managedBy == "" {
		conditionErr := r.setNamespaceCondition(ctx, project.Name, metav1.ConditionFalse, "NamespaceUnavailable", fmt.Sprintf("Namespace %q already exists and is not managed by ReleaseYard", project.Spec.Namespace))
		return false, ctrl.Result{RequeueAfter: projectRetryInterval}, conditionErr
	}

	desiredLabels := projectNamespaceLabels(project)
	changed := false
	if namespace.Labels == nil {
		namespace.Labels = map[string]string{}
	}
	for key, value := range desiredLabels {
		if namespace.Labels[key] != value {
			namespace.Labels[key] = value
			changed = true
		}
	}
	if changed {
		if err := r.Update(ctx, namespace); err != nil {
			conditionErr := r.setNamespaceCondition(ctx, project.Name, metav1.ConditionFalse, "NamespaceUnavailable", "Project namespace labels could not be updated")
			return false, ctrl.Result{RequeueAfter: projectRetryInterval}, conditionErr
		}
	}
	conditionErr := r.setNamespaceCondition(ctx, project.Name, metav1.ConditionTrue, "NamespaceReady", fmt.Sprintf("Namespace %q is ready", project.Spec.Namespace))
	return conditionErr == nil, ctrl.Result{}, conditionErr
}

func projectNamespaceLabels(project *platformv1alpha1.Project) map[string]string {
	mode := project.Spec.Delivery.Mode
	if mode == "" {
		mode = platformv1alpha1.DeliveryModeDirect
	}
	return map[string]string{
		argocdProjectLabel:             project.Name,
		projectNamespaceManagedByLabel: argocdApplicationManagedBy,
		projectNamespaceDeliveryLabel:  string(mode),
	}
}

func (r *ProjectReconciler) setNamespaceCondition(ctx context.Context, name string, status metav1.ConditionStatus, reason, message string) error {
	project, err := r.freshProject(ctx, name)
	if err != nil {
		return err
	}
	if !meta.SetStatusCondition(&project.Status.Conditions, metav1.Condition{Type: platformv1alpha1.ProjectConditionNamespaceReady, Status: status, ObservedGeneration: project.Generation, LastTransitionTime: metav1.Now(), Reason: reason, Message: message}) {
		return nil
	}
	return r.Client.Status().Update(ctx, &project)
}

func (r *ProjectReconciler) reconcileConnection(ctx context.Context, project *platformv1alpha1.Project, connection *platformv1alpha1.GitConnection, repoName, marker, description string, gh projectGit) (ctrl.Result, error) {
	if checkpoint := project.Status.Provisioning; checkpoint != nil && checkpoint.RetryAfter != nil {
		if wait := time.Until(checkpoint.RetryAfter.Time); wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}
	if reason, message := provisioningConflict(project, connection, repoName); reason != "" {
		return ctrl.Result{}, r.setLatestCondition(ctx, project.Name, metav1.ConditionFalse, reason, message)
	}
	project, checkpoint, err := r.ensureCheckpoint(ctx, project, connection, repoName)
	if errors.Is(err, errRequeue) {
		return ctrl.Result{RequeueAfter: projectRetryInterval}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	return r.ensureRepository(ctx, project, checkpoint, marker, description, gh)
}

func provisioningConflict(project *platformv1alpha1.Project, connection *platformv1alpha1.GitConnection, repoName string) (string, string) {
	checkpoint := project.Status.Provisioning
	if checkpoint == nil || checkpoint.ConnectionUID == "" {
		return "", ""
	}
	if checkpoint.ConnectionUID != string(connection.UID) {
		return projectReasonConflict, "Referenced Git connection was recreated after provisioning began"
	}
	if connection.Spec.GitHub == nil {
		return "UnsupportedProvider", "Git provider is not configured"
	}
	if checkpoint.Owner != connection.Spec.GitHub.Organization || checkpoint.Repo != repoName {
		return projectReasonConflict, "Project provisioning identity differs from its immutable checkpoint"
	}
	return "", ""
}

func (r *ProjectReconciler) ensureCheckpoint(ctx context.Context, project *platformv1alpha1.Project, connection *platformv1alpha1.GitConnection, repoName string) (*platformv1alpha1.Project, *platformv1alpha1.ProjectProvisioningStatus, error) {
	checkpoint := project.Status.Provisioning
	if checkpoint != nil && checkpoint.ConnectionUID != "" {
		return project, checkpoint, nil
	}
	latest, err := r.freshProject(ctx, project.Name)
	if err != nil {
		return project, nil, err
	}
	if latest.Status.Provisioning != nil && latest.Status.Provisioning.Attempted {
		return &latest, latest.Status.Provisioning, nil
	}
	latest.Status.Provisioning = &platformv1alpha1.ProjectProvisioningStatus{
		ConnectionUID: string(connection.UID),
		Owner:         connection.Spec.GitHub.Organization,
		Repo:          repoName,
	}
	if err := r.Client.Status().Update(ctx, &latest); err != nil {
		return project, nil, err
	}
	return &latest, latest.Status.Provisioning, nil
}

func (r *ProjectReconciler) ensureRepository(ctx context.Context, project *platformv1alpha1.Project, checkpoint *platformv1alpha1.ProjectProvisioningStatus, marker, description string, gh projectGit) (ctrl.Result, error) {
	if checkpoint == nil {
		return ctrl.Result{RequeueAfter: projectRetryInterval}, r.setLatestCondition(ctx, project.Name, metav1.ConditionFalse, "ProvisioningIncomplete", "Project provisioning checkpoint is incomplete")
	}
	repo, getErr := gh.GetRepository(ctx, checkpoint.Owner, checkpoint.Repo)
	if getErr == nil {
		if !matchesProjectRepository(repo, checkpoint.Owner, checkpoint.Repo, marker) || (checkpoint.RepositoryID != 0 && repo.ID != checkpoint.RepositoryID) || (project.Status.Repository != nil && repo.ID != project.Status.Repository.ID) {
			return ctrl.Result{}, r.setLatestCondition(ctx, project.Name, metav1.ConditionFalse, projectReasonConflict, "Repository identity or ownership does not match this Project")
		}
		if project.Status.Repository == nil {
			if err := seedRepository(ctx, gh, project.Name, repo); err != nil {
				return ctrl.Result{RequeueAfter: projectRetryInterval}, r.setLatestCondition(ctx, project.Name, metav1.ConditionFalse, "RepositoryIncomplete", "Repository was created but its initial structure could not be written")
			}
		}
		return r.recordRepository(ctx, project, repo)
	}
	if !errors.Is(getErr, githubrepo.ErrNotFound) {
		return ctrl.Result{RequeueAfter: projectRetryInterval}, r.setLatestCondition(ctx, project.Name, metav1.ConditionUnknown, "GitHubUnavailable", "Git repository could not be checked")
	}
	if checkpoint.Attempted {
		return ctrl.Result{RequeueAfter: projectRetryInterval}, r.setLatestCondition(ctx, project.Name, metav1.ConditionFalse, "RecoveryRequired", "Repository creation may have occurred but the repository was not found; automatic retry is disabled")
	}
	latest, err := r.freshProject(ctx, project.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !latest.DeletionTimestamp.IsZero() || latest.UID != project.UID || !reflect.DeepEqual(latest.Spec, project.Spec) || latest.Status.Provisioning == nil || latest.Status.Provisioning.ConnectionUID != checkpoint.ConnectionUID || latest.Status.Provisioning.Attempted {
		return ctrl.Result{RequeueAfter: projectRetryInterval}, nil
	}
	latest.Status.Provisioning.Attempted = true
	if err := r.Client.Status().Update(ctx, &latest); err != nil {
		return ctrl.Result{}, err
	}
	created, createErr := gh.CreateRepository(ctx, githubrepo.CreateRepositoryRequest{Owner: checkpoint.Owner, Name: checkpoint.Repo, Description: description})
	if createErr != nil {
		return r.createFailure(ctx, project.Name, createErr)
	}
	if !matchesProjectRepository(created, checkpoint.Owner, checkpoint.Repo, marker) || created.ID <= 0 {
		return ctrl.Result{RequeueAfter: projectRetryInterval}, r.setLatestCondition(ctx, project.Name, metav1.ConditionFalse, projectReasonConflict, "Git provider returned a repository that does not match the requested Project")
	}
	if err := seedRepository(ctx, gh, project.Name, created); err != nil {
		return ctrl.Result{RequeueAfter: projectRetryInterval}, r.setLatestCondition(ctx, project.Name, metav1.ConditionFalse, "RepositoryIncomplete", "Repository was created but its initial structure could not be written")
	}
	return r.recordCreated(ctx, project.Name, created)
}

func seedRepository(ctx context.Context, gh projectGit, projectName string, repo githubrepo.Repository) error {
	branch := repo.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	files := map[string]string{
		"README.md":               fmt.Sprintf("# %s\n\nReleaseYard project repository.\n\nApplications live under `apps/<name>/`.\n", projectName),
		"kustomization.yaml":      "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - apps\n",
		"apps/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n",
	}
	return gh.SeedRepository(ctx, repo.Owner, repo.Name, branch, files)
}

func (r *ProjectReconciler) createFailure(ctx context.Context, name string, err error) (ctrl.Result, error) {
	if isDefiniteCreateFailure(err) {
		message := "Git provider rejected repository creation; retry after correcting credentials or configuration"
		retryAfter := projectRejectedRetryInterval
		if apiErr, ok := errors.AsType[*githubrepo.APIError](err); ok {
			ctrl.LoggerFrom(ctx).Info("Git provider rejected repository creation", "status", apiErr.StatusCode, "kind", apiErr.Kind)
			message = fmt.Sprintf("Git provider rejected repository creation (HTTP %d); retry after correcting credentials or configuration", apiErr.StatusCode)
			if apiErr.StatusCode == 403 {
				retryAfter = projectForbiddenRetryInterval
				message = "Git provider rejected repository creation (HTTP 403); possible secondary rate limit or token permission. Retry later."
			}
		}
		return ctrl.Result{RequeueAfter: retryAfter}, r.rejectRepository(ctx, name, "RepositoryRejected", message, retryAfter)
	}
	return ctrl.Result{RequeueAfter: projectRetryInterval}, r.setLatestCondition(ctx, name, metav1.ConditionUnknown, "RecoveryRequired", "Repository creation outcome is uncertain; checking for recovery")
}

func isDefiniteCreateFailure(err error) bool {
	return errors.Is(err, githubrepo.ErrForbidden) ||
		errors.Is(err, githubrepo.ErrNotFound) ||
		errors.Is(err, githubrepo.ErrConflict) ||
		errors.Is(err, githubrepo.ErrRateLimited)
}

func (r *ProjectReconciler) rejectRepository(ctx context.Context, name, reason, message string, retryAfter time.Duration) error {
	project, err := r.freshProject(ctx, name)
	if err != nil {
		return err
	}
	if project.Status.Provisioning != nil {
		project.Status.Provisioning.Attempted = false
		project.Status.Provisioning.RetryAfter = &metav1.Time{Time: time.Now().Add(retryAfter)}
	}
	meta.SetStatusCondition(&project.Status.Conditions, metav1.Condition{Type: platformv1alpha1.ProjectConditionRepositoryReady, Status: metav1.ConditionFalse, ObservedGeneration: project.Generation, LastTransitionTime: metav1.Now(), Reason: reason, Message: message})
	return r.Client.Status().Update(ctx, &project)
}

func (r *ProjectReconciler) freshProject(ctx context.Context, name string) (platformv1alpha1.Project, error) {
	var project platformv1alpha1.Project
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	err := reader.Get(ctx, types.NamespacedName{Name: name}, &project)
	return project, err
}

func (r *ProjectReconciler) setLatestCondition(ctx context.Context, name string, status metav1.ConditionStatus, reason, message string) error {
	project, err := r.freshProject(ctx, name)
	if err != nil {
		return err
	}
	meta.SetStatusCondition(&project.Status.Conditions, metav1.Condition{Type: platformv1alpha1.ProjectConditionRepositoryReady, Status: status, ObservedGeneration: project.Generation, LastTransitionTime: metav1.Now(), Reason: reason, Message: message})
	return r.Client.Status().Update(ctx, &project)
}

func (r *ProjectReconciler) recordRepository(ctx context.Context, project *platformv1alpha1.Project, repo githubrepo.Repository) (ctrl.Result, error) {
	if project.Status.Provisioning == nil {
		return ctrl.Result{}, errors.New("project provisioning checkpoint is missing")
	}
	project.Status.Provisioning.RepositoryID = repo.ID
	project.Status.Provisioning.RetryAfter = nil
	project.Status.Repository = projectRepositoryStatus(repo)
	meta.SetStatusCondition(&project.Status.Conditions, metav1.Condition{Type: platformv1alpha1.ProjectConditionRepositoryReady, Status: metav1.ConditionTrue, ObservedGeneration: project.Generation, LastTransitionTime: metav1.Now(), Reason: "RepositoryReady", Message: "Private Git repository is ready"})
	if err := r.Client.Status().Update(ctx, project); err != nil {
		return ctrl.Result{}, err
	}
	return r.ensureGitOpsApplication(ctx, project.Name, project.UID, repo)
}

func (r *ProjectReconciler) recordCreated(ctx context.Context, name string, created githubrepo.Repository) (ctrl.Result, error) {
	latest, err := r.freshProject(ctx, name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if latest.Status.Provisioning == nil {
		return ctrl.Result{}, errors.New("project provisioning checkpoint is missing")
	}
	latest.Status.Provisioning.RepositoryID = created.ID
	latest.Status.Provisioning.RetryAfter = nil
	latest.Status.Repository = projectRepositoryStatus(created)
	meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{Type: platformv1alpha1.ProjectConditionRepositoryReady, Status: metav1.ConditionTrue, ObservedGeneration: latest.Generation, LastTransitionTime: metav1.Now(), Reason: "RepositoryReady", Message: "Private Git repository is ready"})
	if err := r.Client.Status().Update(ctx, &latest); err != nil {
		return ctrl.Result{}, err
	}
	return r.ensureGitOpsApplication(ctx, latest.Name, latest.UID, created)
}

func projectRepositoryStatus(repo githubrepo.Repository) *platformv1alpha1.ProjectRepositoryStatus {
	return &platformv1alpha1.ProjectRepositoryStatus{
		ID:            repo.ID,
		Owner:         repo.Owner,
		Name:          repo.Name,
		URL:           repo.HTMLURL,
		CloneURL:      repo.CloneURL,
		SSHURL:        repo.SSHURL,
		DefaultBranch: repo.DefaultBranch,
		FullName:      repo.FullName,
	}
}

func matchesProjectRepository(repo githubrepo.Repository, owner, name, marker string) bool {
	if repo.Owner != owner || repo.Name != name || !repo.Private || repo.ID <= 0 {
		return false
	}
	return strings.Contains(repo.Description, marker)
}

func (r *ProjectReconciler) ensureGitOpsApplication(ctx context.Context, projectName string, projectUID types.UID, repo githubrepo.Repository) (ctrl.Result, error) {
	project, err := r.freshProject(ctx, projectName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if project.Spec.Delivery.Mode == platformv1alpha1.DeliveryModeDirect {
		return ctrl.Result{RequeueAfter: projectRetryInterval}, nil
	}
	namespace := r.ArgoCDNamespace
	if namespace == "" {
		namespace = "argocd"
	}
	destinationNamespace := project.Spec.Namespace
	if destinationNamespace == "" {
		destinationNamespace = r.ArgoCDDestinationNamespace
	}
	if destinationNamespace == "" {
		destinationNamespace = "releaseyard-team-platform"
	}
	if repo.CloneURL == "" || repo.DefaultBranch == "" {
		return ctrl.Result{RequeueAfter: projectRetryInterval}, r.setGitOpsCondition(ctx, projectName, metav1.ConditionFalse, "RepositoryIncomplete", "Project repository clone URL or default branch is unavailable")
	}
	desired := &unstructured.Unstructured{}
	desired.SetAPIVersion("argoproj.io/v1alpha1")
	desired.SetKind("Application")
	desired.SetName(projectName)
	desired.SetNamespace(namespace)
	desired.SetLabels(map[string]string{
		"app.kubernetes.io/managed-by": argocdApplicationManagedBy,
		argocdApplicationManagedLabel:  argocdApplicationManagedBy,
		argocdProjectLabel:             projectName,
	})
	desired.SetAnnotations(map[string]string{argocdProjectUIDAnnotation: string(projectUID)})
	desired.Object["spec"] = map[string]any{
		"project": "default",
		"source": map[string]any{
			"repoURL":        repo.CloneURL,
			"targetRevision": repo.DefaultBranch,
			"path":           argocdApplicationPath,
		},
		"destination": map[string]any{
			"server":             argocdDestinationServer,
			argocdNamespaceField: destinationNamespace,
		},
	}
	key := types.NamespacedName{Namespace: namespace, Name: projectName}
	existing := &unstructured.Unstructured{}
	existing.SetAPIVersion("argoproj.io/v1alpha1")
	existing.SetKind("Application")
	if err := r.Get(ctx, key, existing); err != nil {
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, desired); err != nil {
				return ctrl.Result{RequeueAfter: projectRetryInterval}, r.setGitOpsCondition(ctx, projectName, metav1.ConditionFalse, "GitOpsApplicationUnavailable", "Argo CD Application could not be created")
			}
			return ctrl.Result{RequeueAfter: projectRetryInterval}, r.setGitOpsCondition(ctx, projectName, metav1.ConditionTrue, "GitOpsApplicationReady", "Argo CD Application is ready with manual sync")
		}
		if meta.IsNoMatchError(err) {
			return ctrl.Result{RequeueAfter: projectRetryInterval}, r.setGitOpsCondition(ctx, projectName, metav1.ConditionFalse, "GitOpsApplicationUnavailable", "Argo CD Application API is not installed")
		}
		return ctrl.Result{RequeueAfter: projectRetryInterval}, r.setGitOpsCondition(ctx, projectName, metav1.ConditionUnknown, "GitOpsApplicationUnavailable", "Argo CD Application could not be checked")
	}
	uid := existing.GetAnnotations()[argocdProjectUIDAnnotation]
	managedBy := existing.GetLabels()[argocdApplicationManagedLabel]
	if (uid != "" && uid != string(projectUID)) || (uid == "" && managedBy != argocdApplicationManagedBy) {
		return ctrl.Result{}, r.setGitOpsCondition(ctx, projectName, metav1.ConditionFalse, "GitOpsApplicationConflict", "An Argo CD Application with this Project name already exists and is not managed by ReleaseYard")
	}
	before := existing.DeepCopy()
	labels := existing.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["app.kubernetes.io/managed-by"] = argocdApplicationManagedBy
	labels[argocdApplicationManagedLabel] = argocdApplicationManagedBy
	labels[argocdProjectLabel] = projectName
	existing.SetLabels(labels)
	annotations := existing.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[argocdProjectUIDAnnotation] = string(projectUID)
	existing.SetAnnotations(annotations)
	spec, _ := existing.Object["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
	}
	spec["project"] = "default"
	spec["source"] = map[string]any{
		"repoURL":        repo.CloneURL,
		"targetRevision": repo.DefaultBranch,
		"path":           argocdApplicationPath,
	}
	spec["destination"] = map[string]any{
		"server":             argocdDestinationServer,
		argocdNamespaceField: destinationNamespace,
	}
	existing.Object["spec"] = spec
	if !reflect.DeepEqual(before.Object, existing.Object) {
		if err := r.Update(ctx, existing); err != nil {
			return ctrl.Result{RequeueAfter: projectRetryInterval}, r.setGitOpsCondition(ctx, projectName, metav1.ConditionFalse, "GitOpsApplicationUnavailable", "Argo CD Application could not be updated")
		}
	}
	return ctrl.Result{RequeueAfter: projectRetryInterval}, r.setGitOpsCondition(ctx, projectName, metav1.ConditionTrue, "GitOpsApplicationReady", "Argo CD Application is ready with manual sync")
}

func (r *ProjectReconciler) setGitOpsCondition(ctx context.Context, name string, status metav1.ConditionStatus, reason, message string) error {
	project, err := r.freshProject(ctx, name)
	if err != nil {
		return err
	}
	meta.SetStatusCondition(&project.Status.Conditions, metav1.Condition{Type: platformv1alpha1.ProjectConditionGitOpsApplicationReady, Status: status, ObservedGeneration: project.Generation, LastTransitionTime: metav1.Now(), Reason: reason, Message: message})
	return r.Client.Status().Update(ctx, &project)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&platformv1alpha1.Project{}).Named("project").Complete(r)
}
