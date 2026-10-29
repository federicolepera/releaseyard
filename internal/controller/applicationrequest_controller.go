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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/yaml"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	"github.com/federicolepera/idp/internal/githubrepo"
)

const (
	applicationRequestRetryInterval     = 30 * time.Second
	applicationRequestConflictInterval  = 10 * time.Minute
	applicationRequestForbiddenInterval = 30 * time.Minute
	applicationRequestAppPath           = "apps/%s/application.yaml"
	applicationRequestAppKustomization  = "apps/%s/kustomization.yaml"
	applicationRequestKustomization     = "apps/kustomization.yaml"
	applicationRequestFileMode          = "100644"
	applicationRequestFileType          = "blob"
)

type applicationRequestGit interface {
	GetBranchSHA(context.Context, string, string, string) (string, error)
	GetCommitTreeSHA(context.Context, string, string, string) (string, error)
	GetFile(context.Context, string, string, string, string) (githubrepo.File, error)
	CreateTree(context.Context, string, string, string, []githubrepo.TreeEntry) (string, error)
	CreateCommit(context.Context, string, string, string, string, string) (string, error)
	CreateBranch(context.Context, string, string, string, string) error
	FindOpenPullRequest(context.Context, string, string, string) (githubrepo.PullRequest, error)
	CreatePullRequest(context.Context, string, string, githubrepo.CreatePullRequestInput) (githubrepo.PullRequest, error)
}

// ApplicationRequestReconciler turns durable GitOps requests into reviewed Git changes.
type ApplicationRequestReconciler struct {
	client.Client
	APIReader            client.Reader
	CredentialsNamespace string
	Scheme               *runtime.Scheme
	GitHubFactory        func(token string) applicationRequestGit
}

// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=applicationrequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=applicationrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=projects,verbs=get
// +kubebuilder:rbac:groups=platform.releaseyard.io,resources=gitconnections,verbs=get

func (r *ApplicationRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var request platformv1alpha1.ApplicationRequest
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, req.NamespacedName, &request); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !request.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	switch request.Status.Phase {
	case platformv1alpha1.ApplicationRequestChangeRequestOpen:
		if request.Status.ChangeRequestURL != "" {
			return ctrl.Result{}, nil
		}
	case platformv1alpha1.ApplicationRequestConflict, platformv1alpha1.ApplicationRequestRejected:
		return ctrl.Result{}, nil
	}
	if err := r.setStatus(ctx, req.NamespacedName, platformv1alpha1.ApplicationRequestProcessing, "Preparing GitOps change request", "", "", ""); err != nil {
		return ctrl.Result{}, err
	}
	return r.reconcileRequest(ctx, &request)
}

func (r *ApplicationRequestReconciler) reconcileRequest(ctx context.Context, request *platformv1alpha1.ApplicationRequest) (ctrl.Result, error) {
	if r.CredentialsNamespace == "" || r.APIReader == nil {
		return r.retry(ctx, request, "Git credentials are not configured")
	}
	var project platformv1alpha1.Project
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: request.Spec.ProjectRef.Name}, &project); err != nil {
		return r.reject(ctx, request, "Project is unavailable")
	}
	if !project.DeletionTimestamp.IsZero() {
		return r.reject(ctx, request, "Project is being deleted")
	}
	if project.Spec.Namespace == "" || request.Namespace != project.Spec.Namespace {
		return r.reject(ctx, request, "Request namespace does not match the Project namespace")
	}
	if request.Spec.ApplicationNamespace != project.Spec.Namespace {
		return r.reject(ctx, request, "Application namespace does not match the Project namespace")
	}
	if project.Spec.Delivery.Mode == platformv1alpha1.DeliveryModeDirect {
		return r.reject(ctx, request, "Project delivery mode is direct")
	}
	if message := projectCatalogDenial(&project, request.Spec.Application); message != "" {
		return r.reject(ctx, request, message)
	}
	if project.Status.Repository == nil {
		return r.retry(ctx, request, "Project repository is not ready")
	}
	if !meta.IsStatusConditionTrue(project.Status.Conditions, platformv1alpha1.ProjectConditionRepositoryReady) {
		return r.retry(ctx, request, "Project repository is not ready")
	}
	repo := project.Status.Repository
	if repo.Owner == "" || repo.Name == "" {
		return r.reject(ctx, request, "Project repository identity is incomplete")
	}
	connectionNamespace := project.Spec.Repository.ConnectionRef.Namespace
	if connectionNamespace == "" {
		connectionNamespace = r.CredentialsNamespace
	}
	var connection platformv1alpha1.GitConnection
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: connectionNamespace, Name: project.Spec.Repository.ConnectionRef.Name}, &connection); err != nil {
		return r.retry(ctx, request, "Git connection is unavailable")
	}
	if connection.Spec.Provider != platformv1alpha1.GitProviderGitHub || connection.Spec.GitHub == nil {
		return r.reject(ctx, request, "Git provider is not supported")
	}
	var secret corev1.Secret
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: connectionNamespace, Name: connection.Spec.TokenSecretRef.Name}, &secret); err != nil {
		return r.retry(ctx, request, "Git token Secret is unavailable")
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		return r.retry(ctx, request, "Git token Secret is unavailable")
	}
	factory := r.GitHubFactory
	if factory == nil {
		factory = func(token string) applicationRequestGit { return githubrepo.NewClient(token) }
	}
	gh := factory(token)
	return r.applyChange(ctx, request, repo, gh)
}

func (r *ApplicationRequestReconciler) applyChange(ctx context.Context, request *platformv1alpha1.ApplicationRequest, repo *platformv1alpha1.ProjectRepositoryStatus, gh applicationRequestGit) (ctrl.Result, error) {
	appName := request.Spec.ApplicationName
	if appName == "" || strings.Contains(appName, "/") {
		return r.reject(ctx, request, "Application name is invalid")
	}
	branch := applicationRequestBranch(request)
	appPath := fmt.Sprintf(applicationRequestAppPath, appName)
	appKustomizationPath := fmt.Sprintf(applicationRequestAppKustomization, appName)
	baseBranch := repo.DefaultBranch
	if baseBranch == "" {
		baseBranch = "main"
	}
	if pr, err := gh.FindOpenPullRequest(ctx, repo.Owner, repo.Name, branch); err == nil {
		branchSHA, _ := gh.GetBranchSHA(ctx, repo.Owner, repo.Name, branch)
		return r.open(ctx, request, branch, branchSHA, pr.HTMLURL)
	} else if !errors.Is(err, githubrepo.ErrNotFound) {
		return r.providerFailure(ctx, request, err)
	}
	if branchSHA, err := gh.GetBranchSHA(ctx, repo.Owner, repo.Name, branch); err == nil {
		if err := r.setStatus(ctx, requestKey(request), platformv1alpha1.ApplicationRequestBranchPushed, "Branch already exists; opening change request", branch, branchSHA, ""); err != nil {
			return ctrl.Result{}, err
		}
		return r.createPullRequest(ctx, request, repo, gh, branch, branchSHA, baseBranch)
	} else if !errors.Is(err, githubrepo.ErrNotFound) {
		return r.providerFailure(ctx, request, err)
	}

	baseSHA, err := gh.GetBranchSHA(ctx, repo.Owner, repo.Name, baseBranch)
	if err != nil {
		return r.providerFailure(ctx, request, err)
	}
	baseTree, err := gh.GetCommitTreeSHA(ctx, repo.Owner, repo.Name, baseSHA)
	if err != nil {
		return r.providerFailure(ctx, request, err)
	}
	if _, err := gh.GetFile(ctx, repo.Owner, repo.Name, baseBranch, appPath); err == nil {
		return r.reject(ctx, request, "Application already exists in the Project repository")
	} else if !errors.Is(err, githubrepo.ErrNotFound) {
		return r.providerFailure(ctx, request, err)
	}
	kustomization, err := gh.GetFile(ctx, repo.Owner, repo.Name, baseBranch, applicationRequestKustomization)
	if err != nil {
		if errors.Is(err, githubrepo.ErrNotFound) {
			return r.reject(ctx, request, "Project repository is missing apps/kustomization.yaml")
		}
		return r.providerFailure(ctx, request, err)
	}
	updatedKustomization, alreadyListed, err := addApplicationToKustomization(kustomization.Content, appName)
	if err != nil {
		return r.reject(ctx, request, "Project apps/kustomization.yaml is invalid")
	}
	if alreadyListed {
		return r.reject(ctx, request, "Application is already listed in the Project repository")
	}
	applicationYAML, err := applicationManifest(request)
	if err != nil {
		return ctrl.Result{}, err
	}
	treeSHA, err := gh.CreateTree(ctx, repo.Owner, repo.Name, baseTree, []githubrepo.TreeEntry{
		{Path: appPath, Mode: applicationRequestFileMode, Type: applicationRequestFileType, Content: string(applicationYAML)},
		{Path: appKustomizationPath, Mode: applicationRequestFileMode, Type: applicationRequestFileType, Content: applicationKustomizationManifest()},
		{Path: applicationRequestKustomization, Mode: applicationRequestFileMode, Type: applicationRequestFileType, Content: string(updatedKustomization)},
	})
	if err != nil {
		return r.providerFailure(ctx, request, err)
	}
	commitSHA, err := gh.CreateCommit(ctx, repo.Owner, repo.Name, fmt.Sprintf("Add application %s", appName), treeSHA, baseSHA)
	if err != nil {
		return r.providerFailure(ctx, request, err)
	}
	if err := gh.CreateBranch(ctx, repo.Owner, repo.Name, branch, commitSHA); err != nil {
		return r.providerFailure(ctx, request, err)
	}
	if err := r.setStatus(ctx, requestKey(request), platformv1alpha1.ApplicationRequestBranchPushed, "Branch pushed; opening change request", branch, commitSHA, ""); err != nil {
		return ctrl.Result{}, err
	}
	return r.createPullRequest(ctx, request, repo, gh, branch, commitSHA, baseBranch)
}

func (r *ApplicationRequestReconciler) createPullRequest(ctx context.Context, request *platformv1alpha1.ApplicationRequest, repo *platformv1alpha1.ProjectRepositoryStatus, gh applicationRequestGit, branch, commitSHA, baseBranch string) (ctrl.Result, error) {
	pr, err := gh.CreatePullRequest(ctx, repo.Owner, repo.Name, githubrepo.CreatePullRequestInput{
		Title: fmt.Sprintf("Add application %s", request.Spec.ApplicationName),
		Head:  branch,
		Base:  baseBranch,
		Body:  applicationRequestBody(request),
	})
	if err != nil {
		if errors.Is(err, githubrepo.ErrConflict) {
			if existing, findErr := gh.FindOpenPullRequest(ctx, repo.Owner, repo.Name, branch); findErr == nil {
				return r.open(ctx, request, branch, commitSHA, existing.HTMLURL)
			}
		}
		return r.providerFailure(ctx, request, err)
	}
	return r.open(ctx, request, branch, commitSHA, pr.HTMLURL)
}

func (r *ApplicationRequestReconciler) open(ctx context.Context, request *platformv1alpha1.ApplicationRequest, branch, commitSHA, url string) (ctrl.Result, error) {
	if err := r.setStatus(ctx, requestKey(request), platformv1alpha1.ApplicationRequestChangeRequestOpen, "Pull request opened", branch, commitSHA, url); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *ApplicationRequestReconciler) retry(ctx context.Context, request *platformv1alpha1.ApplicationRequest, message string) (ctrl.Result, error) {
	if err := r.setStatus(ctx, requestKey(request), platformv1alpha1.ApplicationRequestAccepted, message, "", "", ""); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: applicationRequestRetryInterval}, nil
}

func (r *ApplicationRequestReconciler) reject(ctx context.Context, request *platformv1alpha1.ApplicationRequest, message string) (ctrl.Result, error) {
	if err := r.setStatus(ctx, requestKey(request), platformv1alpha1.ApplicationRequestRejected, message, "", "", ""); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *ApplicationRequestReconciler) providerFailure(ctx context.Context, request *platformv1alpha1.ApplicationRequest, err error) (ctrl.Result, error) {
	apiErr, ok := errors.AsType[*githubrepo.APIError](err)
	if !ok {
		return r.retry(ctx, request, "Git provider is temporarily unavailable")
	}
	switch apiErr.Kind {
	case githubrepo.ErrorRateLimited, githubrepo.ErrorForbidden:
		message := fmt.Sprintf("Git provider rejected the request (HTTP %d); retry later", apiErr.StatusCode)
		if err := r.setStatus(ctx, requestKey(request), platformv1alpha1.ApplicationRequestAccepted, message, "", "", ""); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: applicationRequestForbiddenInterval}, nil
	case githubrepo.ErrorTransient:
		return r.retry(ctx, request, "Git provider is temporarily unavailable")
	case githubrepo.ErrorConflict:
		if err := r.setStatus(ctx, requestKey(request), platformv1alpha1.ApplicationRequestConflict, "Git provider reported a conflict", "", "", ""); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: applicationRequestConflictInterval}, nil
	case githubrepo.ErrorNotFound:
		return r.reject(ctx, request, "Git repository or branch was not found")
	default:
		message := fmt.Sprintf("Git provider rejected the request (HTTP %d)", apiErr.StatusCode)
		return r.reject(ctx, request, message)
	}
}

func (r *ApplicationRequestReconciler) setStatus(ctx context.Context, key types.NamespacedName, phase platformv1alpha1.ApplicationRequestPhase, message, branch, commitSHA, url string) error {
	var latest platformv1alpha1.ApplicationRequest
	if err := r.Get(ctx, key, &latest); err != nil {
		return client.IgnoreNotFound(err)
	}
	if latest.Status.Phase == phase && latest.Status.Message == message && latest.Status.Branch == branch && latest.Status.CommitSHA == commitSHA && latest.Status.ChangeRequestURL == url {
		return nil
	}
	latest.Status = platformv1alpha1.ApplicationRequestStatus{Phase: phase, Message: message, Branch: branch, CommitSHA: commitSHA, ChangeRequestURL: url}
	return r.Status().Update(ctx, &latest)
}

func requestKey(request *platformv1alpha1.ApplicationRequest) types.NamespacedName {
	return types.NamespacedName{Namespace: request.Namespace, Name: request.Name}
}

func applicationRequestBranch(request *platformv1alpha1.ApplicationRequest) string {
	short := strings.ReplaceAll(string(request.UID), "-", "")
	if short == "" {
		sum := sha256.Sum256([]byte(request.Spec.IdempotencyKey))
		short = hex.EncodeToString(sum[:])
	}
	if len(short) > 8 {
		short = short[:8]
	}
	return "releaseyard/" + request.Spec.ApplicationName + "-" + short
}

func applicationManifest(request *platformv1alpha1.ApplicationRequest) ([]byte, error) {
	app := platformv1alpha1.Application{
		TypeMeta: metav1.TypeMeta{APIVersion: platformv1alpha1.GroupVersion.String(), Kind: "Application"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      request.Spec.ApplicationName,
			Namespace: request.Spec.ApplicationNamespace,
		},
		Spec: request.Spec.Application,
	}
	return yaml.Marshal(app)
}

func addApplicationToKustomization(content []byte, appName string) ([]byte, bool, error) {
	var document map[string]any
	if err := yaml.Unmarshal(content, &document); err != nil {
		return nil, false, err
	}
	resources := make([]any, 0)
	if raw, ok := document["resources"]; ok && raw != nil {
		values, ok := raw.([]any)
		if !ok {
			return nil, false, errors.New("resources is not a list")
		}
		resources = values
	}
	for _, value := range resources {
		if value == appName {
			return content, true, nil
		}
	}
	document["resources"] = append(resources, appName)
	updated, err := yaml.Marshal(document)
	if err != nil {
		return nil, false, err
	}
	return updated, false, nil
}

func applicationKustomizationManifest() string {
	return "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - application.yaml\n"
}

func applicationRequestBody(request *platformv1alpha1.ApplicationRequest) string {
	return fmt.Sprintf("## ReleaseYard GitOps request\n\n- Application: `%s`\n- Namespace: `%s`\n- Project: `%s`\n- Requested by: `%s`\n- Request: `%s/%s`\n",
		request.Spec.ApplicationName, request.Spec.ApplicationNamespace, request.Spec.ProjectRef.Name,
		request.Spec.RequestedBy, request.Namespace, request.Name)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ApplicationRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.ApplicationRequest{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("applicationrequest").
		Complete(r)
}

func projectCatalogDenial(project *platformv1alpha1.Project, application platformv1alpha1.ApplicationSpec) string {
	if !project.Spec.Catalog.AllowsApplicationDefinition(application.ApplicationDefinitionRef.Name) {
		return "Application definition is not in the Project catalog"
	}
	if application.ClusterRef != nil && !project.Spec.Catalog.AllowsCluster(application.ClusterRef.Name) {
		return "Target cluster is not in the Project catalog"
	}
	for _, target := range application.Clusters {
		if !project.Spec.Catalog.AllowsCluster(target.Name) {
			return "Target cluster is not in the Project catalog"
		}
	}
	if application.Environment != nil {
		for _, target := range application.Environment.Targets {
			if !project.Spec.Catalog.AllowsCluster(target.Name) {
				return "Environment target is not in the Project catalog"
			}
		}
	}
	return ""
}
