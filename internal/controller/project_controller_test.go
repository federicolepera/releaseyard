package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	"github.com/federicolepera/idp/internal/githubrepo"
)

type projectGitMock struct {
	repo          githubrepo.Repository
	getErr        error
	createRepo    githubrepo.Repository
	createErr     error
	creates       int
	createRequest githubrepo.CreateRepositoryRequest
	seedErr       error
	seeds         int
	seedBranch    string
	seedFiles     map[string]string
}

func (m *projectGitMock) GetRepository(context.Context, string, string) (githubrepo.Repository, error) {
	return m.repo, m.getErr
}

func (m *projectGitMock) CreateRepository(_ context.Context, req githubrepo.CreateRepositoryRequest) (githubrepo.Repository, error) {
	m.creates++
	m.createRequest = req
	return m.createRepo, m.createErr
}

func (m *projectGitMock) SeedRepository(_ context.Context, _, _, branch string, files map[string]string) error {
	m.seeds++
	m.seedBranch = branch
	m.seedFiles = files
	return m.seedErr
}

func setupProjectController(t *testing.T, project *platformv1alpha1.Project, connectionUID types.UID, secretExists bool, gh *projectGitMock) (*ProjectReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = platformv1alpha1.AddToScheme(scheme)
	connection := &platformv1alpha1.GitConnection{
		ObjectMeta: metav1.ObjectMeta{Namespace: "manager-ns", Name: "github", UID: connectionUID},
		Spec: platformv1alpha1.GitConnectionSpec{
			Provider:       platformv1alpha1.GitProviderGitHub,
			TokenSecretRef: platformv1alpha1.GitTokenSecretReference{Name: "token"},
			GitHub:         &platformv1alpha1.GitHubProviderConfig{Organization: "acme"},
		},
	}
	objects := []client.Object{project, connection}
	if secretExists {
		objects = append(objects, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "manager-ns", Name: "token"}, Data: map[string][]byte{"token": []byte("secret-token")}})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(project).WithObjects(objects...).Build()
	r := &ProjectReconciler{Client: c, APIReader: c, CredentialsNamespace: "manager-ns", Scheme: scheme, GitHubFactory: func(string) projectGit { return gh }}
	return r, c
}

func projectFixture() *platformv1alpha1.Project {
	return &platformv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", UID: "project-uid", Generation: 1},
		Spec: platformv1alpha1.ProjectSpec{
			Repository:  platformv1alpha1.ProjectRepositorySpec{ConnectionRef: platformv1alpha1.GitConnectionReference{Name: "github"}},
			Description: "a project",
		},
	}
}

func readyRepo() githubrepo.Repository {
	return githubrepo.Repository{
		ID: 42, Owner: "acme", Name: "demo", FullName: "acme/demo",
		HTMLURL: "https://github.com/acme/demo", CloneURL: "https://github.com/acme/demo.git",
		SSHURL: "git@github.com:acme/demo.git", DefaultBranch: "main", Private: true,
		Description: "[releaseyard-project-uid:project-uid]",
	}
}

func readyCheckpoint() *platformv1alpha1.ProjectProvisioningStatus {
	return &platformv1alpha1.ProjectProvisioningStatus{ConnectionUID: "connection-uid", Owner: "acme", Repo: "demo"}
}

func reconcileProject(t *testing.T, r *ProjectReconciler) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "demo"}})
	return err
}

func TestProjectCreatesAndSeedsPrivateMarkedRepository(t *testing.T) {
	project := projectFixture()
	gh := &projectGitMock{getErr: githubrepo.ErrNotFound, createRepo: readyRepo()}
	r, c := setupProjectController(t, project, "connection-uid", true, gh)
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.creates != 1 || gh.createRequest.Name != "demo" || gh.seeds != 1 || gh.seedBranch != "main" {
		t.Fatalf("create/seed calls = %d/%d request=%#v branch=%q", gh.creates, gh.seeds, gh.createRequest, gh.seedBranch)
	}
	if strings.Contains(gh.createRequest.Description, "\n") {
		t.Fatalf("repository description contains a newline: %q", gh.createRequest.Description)
	}
	if !strings.Contains(gh.seedFiles["README.md"], "# demo") {
		t.Fatalf("README was not seeded: %#v", gh.seedFiles)
	}
	if !strings.Contains(gh.seedFiles["kustomization.yaml"], "apps") || !strings.Contains(gh.seedFiles["apps/kustomization.yaml"], "Kustomization") {
		t.Fatalf("kustomize skeleton was not seeded: %#v", gh.seedFiles)
	}
	var got platformv1alpha1.Project
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo"}, &got); err != nil {
		t.Fatal(err)
	}
	repo := got.Status.Repository
	if repo == nil || repo.ID != 42 || repo.CloneURL == "" || repo.SSHURL == "" || repo.DefaultBranch != "main" || !got.Status.Provisioning.Attempted {
		t.Fatalf("repository/checkpoint not recorded: %#v", got.Status)
	}
	if condition := findProjectCondition(got.Status.Conditions); condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("RepositoryReady condition missing/false: %#v", condition)
	}
}

func TestProjectSupportsCustomRepositoryNameAndConnectionNamespace(t *testing.T) {
	project := projectFixture()
	project.Spec.Repository.Name = "custom-repo"
	project.Spec.Repository.ConnectionRef.Namespace = "platform-ns"
	gh := &projectGitMock{getErr: githubrepo.ErrNotFound, createRepo: githubrepo.Repository{
		ID: 43, Owner: "acme", Name: "custom-repo", DefaultBranch: "main", Private: true,
		Description: "[releaseyard-project-uid:project-uid]",
	}}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = platformv1alpha1.AddToScheme(scheme)
	connection := &platformv1alpha1.GitConnection{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform-ns", Name: "github", UID: "connection-uid"},
		Spec: platformv1alpha1.GitConnectionSpec{
			Provider:       platformv1alpha1.GitProviderGitHub,
			TokenSecretRef: platformv1alpha1.GitTokenSecretReference{Name: "token"},
			GitHub:         &platformv1alpha1.GitHubProviderConfig{Organization: "acme"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(project).WithObjects(project, connection,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "platform-ns", Name: "token"}, Data: map[string][]byte{"token": []byte("secret-token")}}).Build()
	r := &ProjectReconciler{Client: c, APIReader: c, CredentialsNamespace: "manager-ns", Scheme: scheme, GitHubFactory: func(string) projectGit { return gh }}
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.createRequest.Name != "custom-repo" {
		t.Fatalf("create request name = %q, want custom-repo", gh.createRequest.Name)
	}
}

func TestProjectDoesNotCreateUnrelatedExistingRepository(t *testing.T) {
	gh := &projectGitMock{repo: githubrepo.Repository{ID: 3, Owner: "acme", Name: "demo", Private: true, Description: "not ours"}}
	r, c := setupProjectController(t, projectFixture(), "connection-uid", true, gh)
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.creates != 0 || gh.seeds != 0 {
		t.Fatal("unexpected repository creation or seeding")
	}
	var got platformv1alpha1.Project
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo"}, &got); err != nil {
		t.Fatal(err)
	}
	if condition := findProjectCondition(got.Status.Conditions); condition == nil || condition.Reason != "Conflict" {
		t.Fatalf("expected Conflict condition, got %#v", condition)
	}
}

func TestProjectRecoversAmbiguousCreationWithoutSecondPost(t *testing.T) {
	project := projectFixture()
	project.Status.Provisioning = readyCheckpoint()
	project.Status.Provisioning.Attempted = true
	gh := &projectGitMock{repo: readyRepo()}
	r, c := setupProjectController(t, project, "connection-uid", true, gh)
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.creates != 0 || gh.seeds != 1 {
		t.Fatalf("recovery created=%d seeded=%d", gh.creates, gh.seeds)
	}
	var got platformv1alpha1.Project
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Repository == nil || got.Status.Repository.ID != 42 {
		t.Fatalf("repository was not recovered: %#v", got.Status)
	}
}

func TestProjectSeedsAfterAmbiguousPost(t *testing.T) {
	project := projectFixture()
	gh := &projectGitMock{getErr: githubrepo.ErrNotFound, createErr: errors.New("transport failed")}
	r, c := setupProjectController(t, project, "connection-uid", true, gh)
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.creates != 1 || gh.seeds != 0 {
		t.Fatalf("create=%d seed=%d", gh.creates, gh.seeds)
	}
	var checkpointed platformv1alpha1.Project
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo"}, &checkpointed); err != nil {
		t.Fatal(err)
	}
	if !checkpointed.Status.Provisioning.Attempted {
		t.Fatal("ambiguous POST did not persist the attempted checkpoint")
	}
	gh.getErr = nil
	gh.repo = readyRepo()
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.creates != 1 || gh.seeds != 1 {
		t.Fatalf("ambiguous POST was blindly retried or not seeded: create=%d seed=%d", gh.creates, gh.seeds)
	}
	var recovered platformv1alpha1.Project
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo"}, &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.Status.Repository == nil || recovered.Status.Repository.ID != 42 {
		t.Fatalf("repository was not recovered: %#v", recovered.Status)
	}
}

func TestProjectDefiniteCreateFailureCanRetry(t *testing.T) {
	project := projectFixture()
	gh := &projectGitMock{getErr: githubrepo.ErrNotFound, createErr: githubrepo.ErrConflict}
	r, c := setupProjectController(t, project, "connection-uid", true, gh)
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	var rejected platformv1alpha1.Project
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo"}, &rejected); err != nil {
		t.Fatal(err)
	}
	if rejected.Status.Provisioning == nil || rejected.Status.Provisioning.Attempted {
		t.Fatalf("definite rejection kept attempted checkpoint: %#v", rejected.Status.Provisioning)
	}
	if condition := findProjectCondition(rejected.Status.Conditions); condition == nil || condition.Reason != "RepositoryRejected" {
		t.Fatalf("expected RepositoryRejected, got %#v", condition)
	}
	if rejected.Status.Provisioning.RetryAfter == nil {
		t.Fatal("rejected project did not record a retry time")
	}
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.creates != 1 {
		t.Fatalf("rejected project retried immediately; calls = %d", gh.creates)
	}
	rejected.Status.Provisioning.RetryAfter = &metav1.Time{Time: time.Now().Add(-time.Second)}
	if err := c.Status().Update(context.Background(), &rejected); err != nil {
		t.Fatal(err)
	}
	gh.createErr = nil
	gh.createRepo = readyRepo()
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.creates != 2 || gh.seeds != 1 {
		t.Fatalf("create=%d seed=%d, want retry and seed", gh.creates, gh.seeds)
	}
}

func TestProjectForbiddenUsesLongBackoff(t *testing.T) {
	project := projectFixture()
	gh := &projectGitMock{getErr: githubrepo.ErrNotFound, createErr: &githubrepo.APIError{StatusCode: 403, Kind: githubrepo.ErrorForbidden}}
	r, c := setupProjectController(t, project, "connection-uid", true, gh)
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	var got platformv1alpha1.Project
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Provisioning == nil || got.Status.Provisioning.RetryAfter == nil {
		t.Fatalf("403 did not record a retry time: %#v", got.Status.Provisioning)
	}
	if got.Status.Provisioning.RetryAfter.Time.Before(time.Now().Add(20 * time.Minute)) {
		t.Fatalf("403 retry time is too soon: %v", got.Status.Provisioning.RetryAfter)
	}
	if condition := findProjectCondition(got.Status.Conditions); condition == nil || !strings.Contains(condition.Message, "HTTP 403") {
		t.Fatalf("expected HTTP 403 condition, got %#v", condition)
	}
}

func TestProjectAmbiguousCreateFailureDoesNotRetry(t *testing.T) {
	project := projectFixture()
	gh := &projectGitMock{getErr: githubrepo.ErrNotFound, createErr: githubrepo.ErrTransient}
	r, c := setupProjectController(t, project, "connection-uid", true, gh)
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	var got platformv1alpha1.Project
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Provisioning == nil || !got.Status.Provisioning.Attempted {
		t.Fatalf("ambiguous failure did not keep attempted checkpoint: %#v", got.Status.Provisioning)
	}
	if condition := findProjectCondition(got.Status.Conditions); condition == nil || condition.Reason != "RecoveryRequired" {
		t.Fatalf("expected RecoveryRequired, got %#v", condition)
	}
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.creates != 1 {
		t.Fatalf("ambiguous failure was retried; calls = %d", gh.creates)
	}
}

func TestProjectRetriesSeedingAfterCreation(t *testing.T) {
	project := projectFixture()
	gh := &projectGitMock{getErr: githubrepo.ErrNotFound, createRepo: readyRepo(), seedErr: githubrepo.ErrTransient}
	r, c := setupProjectController(t, project, "connection-uid", true, gh)
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	var incomplete platformv1alpha1.Project
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo"}, &incomplete); err != nil {
		t.Fatal(err)
	}
	if condition := findProjectCondition(incomplete.Status.Conditions); condition == nil || condition.Reason != "RepositoryIncomplete" {
		t.Fatalf("expected RepositoryIncomplete, got %#v", condition)
	}
	gh.getErr = nil
	gh.repo = readyRepo()
	gh.seedErr = nil
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.creates != 1 || gh.seeds != 2 {
		t.Fatalf("create=%d seed=%d, want one create and seed retry", gh.creates, gh.seeds)
	}
	var ready platformv1alpha1.Project
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo"}, &ready); err != nil {
		t.Fatal(err)
	}
	if condition := findProjectCondition(ready.Status.Conditions); condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("expected ready after seed retry, got %#v", condition)
	}
}

func TestProjectNeverRetriesMissingRepositoryAfterAttempt(t *testing.T) {
	project := projectFixture()
	project.Status.Provisioning = readyCheckpoint()
	project.Status.Provisioning.Attempted = true
	gh := &projectGitMock{getErr: githubrepo.ErrNotFound}
	r, c := setupProjectController(t, project, "connection-uid", true, gh)
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.creates != 0 || gh.seeds != 0 {
		t.Fatal("missing repository triggered second POST or seed")
	}
	var got platformv1alpha1.Project
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo"}, &got); err != nil {
		t.Fatal(err)
	}
	if condition := findProjectCondition(got.Status.Conditions); condition == nil || condition.Reason != "RecoveryRequired" {
		t.Fatalf("expected RecoveryRequired, got %#v", condition)
	}
}

func TestProjectFailsClosedForRecreatedConnectionAndMissingSecret(t *testing.T) {
	project := projectFixture()
	project.Status.Provisioning = readyCheckpoint()
	project.Status.Provisioning.ConnectionUID = "old-uid"
	gh := &projectGitMock{getErr: githubrepo.ErrNotFound}
	r, _ := setupProjectController(t, project, "new-uid", true, gh)
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.creates != 0 {
		t.Fatal("recreated connection triggered POST")
	}
	project = projectFixture()
	r, c := setupProjectController(t, project, "connection-uid", false, gh)
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.creates != 0 {
		t.Fatal("missing secret triggered POST")
	}
	var got platformv1alpha1.Project
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo"}, &got); err != nil {
		t.Fatal(err)
	}
	if condition := findProjectCondition(got.Status.Conditions); condition == nil || condition.Reason != "CredentialsUnavailable" {
		t.Fatalf("expected sanitized credential condition, got %#v", condition)
	}
}

func TestProjectRejectsUnsupportedProvider(t *testing.T) {
	project := projectFixture()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = platformv1alpha1.AddToScheme(scheme)
	connection := &platformv1alpha1.GitConnection{
		ObjectMeta: metav1.ObjectMeta{Namespace: "manager-ns", Name: "github", UID: "connection-uid"},
		Spec: platformv1alpha1.GitConnectionSpec{
			Provider:       platformv1alpha1.GitProvider("gitlab"),
			TokenSecretRef: platformv1alpha1.GitTokenSecretReference{Name: "token"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(project).WithObjects(project, connection,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "manager-ns", Name: "token"}, Data: map[string][]byte{"token": []byte("secret-token")}}).Build()
	gh := &projectGitMock{}
	r := &ProjectReconciler{Client: c, APIReader: c, CredentialsNamespace: "manager-ns", Scheme: scheme, GitHubFactory: func(string) projectGit { return gh }}
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.creates != 0 {
		t.Fatal("unsupported provider triggered creation")
	}
	var got platformv1alpha1.Project
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo"}, &got); err != nil {
		t.Fatal(err)
	}
	if condition := findProjectCondition(got.Status.Conditions); condition == nil || condition.Reason != "UnsupportedProvider" {
		t.Fatalf("expected UnsupportedProvider, got %#v", condition)
	}
}

func TestProjectIgnoresDeletionAndMissingProject(t *testing.T) {
	project := projectFixture()
	deleted := metav1.Now()
	project.DeletionTimestamp = &deleted
	project.Finalizers = []string{"unrelated"}
	gh := &projectGitMock{}
	r, _ := setupProjectController(t, project, "connection-uid", true, gh)
	if err := reconcileProject(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.creates != 0 {
		t.Fatal("deleting Project triggered GitHub operation")
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "absent"}}); err != nil && !errors.Is(err, apierrors.NewNotFound(platformv1alpha1.GroupVersion.WithResource("projects").GroupResource(), "absent")) {
		t.Fatal(err)
	}
}

func findProjectCondition(conditions []metav1.Condition) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == platformv1alpha1.ProjectConditionRepositoryReady {
			return &conditions[i]
		}
	}
	return nil
}
