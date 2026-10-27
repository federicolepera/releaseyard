package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	"github.com/federicolepera/idp/internal/githubrepo"
)

type applicationRequestGitMock struct {
	baseSHA, baseTree, branchSHA, treeSHA, commitSHA    string
	branchErr, appFileErr, kustomizationErr             error
	appFile, kustomization                              *githubrepo.File
	openPR                                              *githubrepo.PullRequest
	findPRErr                                           error
	createPRErr                                         error
	treeEntries                                         []githubrepo.TreeEntry
	createCommitMessage, createCommitTree, createParent string
	createdBranch, createdBranchSHA                     string
	createdPR                                           githubrepo.CreatePullRequestInput
	createPRCalled                                      bool
}

func (m *applicationRequestGitMock) GetBranchSHA(_ context.Context, _, _, branch string) (string, error) {
	if branch == "main" {
		return m.baseSHA, nil
	}
	if m.branchErr != nil {
		return "", m.branchErr
	}
	if m.branchSHA == "" {
		return "", githubrepo.ErrNotFound
	}
	return m.branchSHA, nil
}

func (m *applicationRequestGitMock) GetCommitTreeSHA(context.Context, string, string, string) (string, error) {
	return m.baseTree, nil
}

func (m *applicationRequestGitMock) GetFile(_ context.Context, _, _, _, path string) (githubrepo.File, error) {
	switch path {
	case "apps/test/application.yaml":
		if m.appFileErr != nil {
			return githubrepo.File{}, m.appFileErr
		}
		if m.appFile == nil {
			return githubrepo.File{}, githubrepo.ErrNotFound
		}
		return *m.appFile, nil
	case "apps/kustomization.yaml":
		if m.kustomizationErr != nil {
			return githubrepo.File{}, m.kustomizationErr
		}
		if m.kustomization == nil {
			return githubrepo.File{}, githubrepo.ErrNotFound
		}
		return *m.kustomization, nil
	default:
		return githubrepo.File{}, githubrepo.ErrNotFound
	}
}

func (m *applicationRequestGitMock) CreateTree(_ context.Context, _, _, _ string, entries []githubrepo.TreeEntry) (string, error) {
	m.treeEntries = entries
	return m.treeSHA, nil
}

func (m *applicationRequestGitMock) CreateCommit(_ context.Context, _, _, message, tree, parent string) (string, error) {
	m.createCommitMessage, m.createCommitTree, m.createParent = message, tree, parent
	return m.commitSHA, nil
}

func (m *applicationRequestGitMock) CreateBranch(_ context.Context, _, _, branch, sha string) error {
	m.createdBranch, m.createdBranchSHA = branch, sha
	return nil
}

func (m *applicationRequestGitMock) FindOpenPullRequest(context.Context, string, string, string) (githubrepo.PullRequest, error) {
	if m.findPRErr != nil {
		return githubrepo.PullRequest{}, m.findPRErr
	}
	if m.openPR == nil {
		return githubrepo.PullRequest{}, githubrepo.ErrNotFound
	}
	return *m.openPR, nil
}

func (m *applicationRequestGitMock) CreatePullRequest(_ context.Context, _, _ string, input githubrepo.CreatePullRequestInput) (githubrepo.PullRequest, error) {
	m.createPRCalled = true
	m.createdPR = input
	if m.createPRErr != nil {
		return githubrepo.PullRequest{}, m.createPRErr
	}
	return githubrepo.PullRequest{Number: 7, HTMLURL: "https://github.com/acme/demo/pull/7"}, nil
}

func applicationRequestFixture() *platformv1alpha1.ApplicationRequest {
	return &platformv1alpha1.ApplicationRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "idp-system", UID: "12345678-aaaa-bbbb-cccc-123456789abc", Generation: 1},
		Spec: platformv1alpha1.ApplicationRequestSpec{
			ProjectRef:           platformv1alpha1.ProjectRequestReference{Name: "project-sample"},
			ApplicationName:      "test",
			ApplicationNamespace: "releaseyard-team-platform",
			RequestedBy:          "flepera@sinthera.com",
			IdempotencyKey:       "idempotency",
			Application: platformv1alpha1.ApplicationSpec{
				ApplicationDefinitionRef: platformv1alpha1.ApplicationReference{Name: "applicationdefinition-sample"},
				ClusterRef:               &platformv1alpha1.ApplicationReference{Name: "releaseyard-managed-a"},
			},
		},
	}
}

func readyProjectFixture() *platformv1alpha1.Project {
	return &platformv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "project-sample"},
		Spec: platformv1alpha1.ProjectSpec{
			Repository: platformv1alpha1.ProjectRepositorySpec{
				ConnectionRef: platformv1alpha1.GitConnectionReference{Name: "acme"},
			},
			Delivery: platformv1alpha1.ProjectDeliverySpec{Mode: platformv1alpha1.DeliveryModeBoth},
		},
		Status: platformv1alpha1.ProjectStatus{
			Repository: &platformv1alpha1.ProjectRepositoryStatus{ID: 1, Owner: "acme", Name: "project-sample", DefaultBranch: "main"},
			Conditions: []metav1.Condition{{Type: platformv1alpha1.ProjectConditionRepositoryReady, Status: metav1.ConditionTrue}},
		},
	}
}

func setupApplicationRequestController(t *testing.T, request *platformv1alpha1.ApplicationRequest, gh *applicationRequestGitMock) (*ApplicationRequestReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = platformv1alpha1.AddToScheme(scheme)
	connection := &platformv1alpha1.GitConnection{
		ObjectMeta: metav1.ObjectMeta{Namespace: "idp-system", Name: "acme", UID: "connection-uid"},
		Spec: platformv1alpha1.GitConnectionSpec{
			Provider:       platformv1alpha1.GitProviderGitHub,
			TokenSecretRef: platformv1alpha1.GitTokenSecretReference{Name: "releaseyard-git-acme"},
			GitHub:         &platformv1alpha1.GitHubProviderConfig{Organization: "acme"},
		},
	}
	objects := []client.Object{
		request,
		readyProjectFixture(),
		connection,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "idp-system", Name: "releaseyard-git-acme"}, Data: map[string][]byte{"token": []byte("secret-token")}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(request).WithObjects(objects...).Build()
	r := &ApplicationRequestReconciler{
		Client: c, APIReader: c, CredentialsNamespace: "idp-system", Scheme: scheme,
		GitHubFactory: func(string) applicationRequestGit { return gh },
	}
	return r, c
}

func reconcileApplicationRequest(t *testing.T, r *ApplicationRequestReconciler) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "idp-system", Name: "test"}})
	return err
}

func TestApplicationRequestCreatesBranchCommitAndPullRequest(t *testing.T) {
	request := applicationRequestFixture()
	gh := &applicationRequestGitMock{
		baseSHA: "base-sha", baseTree: "base-tree", treeSHA: "tree-sha", commitSHA: "commit-sha",
		kustomization: &githubrepo.File{Path: "apps/kustomization.yaml", Content: []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n")},
	}
	r, c := setupApplicationRequestController(t, request, gh)
	if err := reconcileApplicationRequest(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.createdBranch != "releaseyard/test-12345678" || gh.createdBranchSHA != "commit-sha" {
		t.Fatalf("branch=%q sha=%q", gh.createdBranch, gh.createdBranchSHA)
	}
	if gh.createCommitMessage != "Add application test" || gh.createCommitTree != "tree-sha" || gh.createParent != "base-sha" {
		t.Fatalf("commit message=%q tree=%q parent=%q", gh.createCommitMessage, gh.createCommitTree, gh.createParent)
	}
	if len(gh.treeEntries) != 3 || gh.treeEntries[0].Path != "apps/test/application.yaml" || gh.treeEntries[1].Path != "apps/test/kustomization.yaml" || gh.treeEntries[2].Path != "apps/kustomization.yaml" {
		t.Fatalf("tree entries=%#v", gh.treeEntries)
	}
	if !strings.Contains(gh.treeEntries[1].Content, "application.yaml") {
		t.Fatalf("application kustomization not generated: %s", gh.treeEntries[1].Content)
	}
	if !strings.Contains(gh.treeEntries[2].Content, "- test") {
		t.Fatalf("apps kustomization not updated: %s", gh.treeEntries[2].Content)
	}
	var manifest platformv1alpha1.Application
	if err := yaml.Unmarshal([]byte(gh.treeEntries[0].Content), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Name != "test" || manifest.Namespace != "releaseyard-team-platform" || manifest.Spec.ApplicationDefinitionRef.Name != "applicationdefinition-sample" {
		t.Fatalf("application manifest=%#v", manifest)
	}
	if !gh.createPRCalled || gh.createdPR.Head != "releaseyard/test-12345678" || gh.createdPR.Base != "main" {
		t.Fatalf("pull request input=%#v", gh.createdPR)
	}
	var got platformv1alpha1.ApplicationRequest
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "idp-system", Name: "test"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != platformv1alpha1.ApplicationRequestChangeRequestOpen || got.Status.Branch != "releaseyard/test-12345678" || got.Status.CommitSHA != "commit-sha" || got.Status.ChangeRequestURL != "https://github.com/acme/demo/pull/7" {
		t.Fatalf("status=%#v", got.Status)
	}
}

func TestApplicationRequestReusesExistingPullRequest(t *testing.T) {
	request := applicationRequestFixture()
	gh := &applicationRequestGitMock{
		openPR:    &githubrepo.PullRequest{Number: 9, HTMLURL: "https://github.com/acme/demo/pull/9"},
		branchSHA: "existing-commit",
	}
	r, c := setupApplicationRequestController(t, request, gh)
	if err := reconcileApplicationRequest(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.createPRCalled || gh.createdBranch != "" || gh.treeEntries != nil {
		t.Fatalf("existing PR triggered a new change: %#v", gh)
	}
	var got platformv1alpha1.ApplicationRequest
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "idp-system", Name: "test"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != platformv1alpha1.ApplicationRequestChangeRequestOpen || got.Status.CommitSHA != "existing-commit" || got.Status.ChangeRequestURL != "https://github.com/acme/demo/pull/9" {
		t.Fatalf("status=%#v", got.Status)
	}
}

func TestApplicationRequestOpensPullRequestForExistingBranch(t *testing.T) {
	request := applicationRequestFixture()
	gh := &applicationRequestGitMock{branchSHA: "existing-branch-sha"}
	r, c := setupApplicationRequestController(t, request, gh)
	if err := reconcileApplicationRequest(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.createPRCalled == false || gh.createdBranch != "" || gh.treeEntries != nil || gh.createCommitMessage != "" {
		t.Fatalf("existing branch should only open a PR: %#v", gh)
	}
	var got platformv1alpha1.ApplicationRequest
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "idp-system", Name: "test"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != platformv1alpha1.ApplicationRequestChangeRequestOpen || got.Status.CommitSHA != "existing-branch-sha" || got.Status.Branch != "releaseyard/test-12345678" {
		t.Fatalf("status=%#v", got.Status)
	}
}

func TestApplicationRequestRejectsExistingApplication(t *testing.T) {
	request := applicationRequestFixture()
	gh := &applicationRequestGitMock{
		baseSHA: "base-sha", baseTree: "base-tree",
		appFile:       &githubrepo.File{Path: "apps/test/application.yaml", Content: []byte("existing")},
		kustomization: &githubrepo.File{Path: "apps/kustomization.yaml", Content: []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n")},
	}
	r, c := setupApplicationRequestController(t, request, gh)
	if err := reconcileApplicationRequest(t, r); err != nil {
		t.Fatal(err)
	}
	if gh.createPRCalled || gh.createdBranch != "" {
		t.Fatal("existing Application triggered a Git change")
	}
	var got platformv1alpha1.ApplicationRequest
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "idp-system", Name: "test"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != platformv1alpha1.ApplicationRequestRejected || !strings.Contains(got.Status.Message, "already exists") {
		t.Fatalf("status=%#v", got.Status)
	}
}

func TestApplicationRequestRejectsMissingProject(t *testing.T) {
	request := applicationRequestFixture()
	r, c := setupApplicationRequestController(t, request, &applicationRequestGitMock{})
	project := &platformv1alpha1.Project{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "project-sample"}, project); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	if err := reconcileApplicationRequest(t, r); err != nil {
		t.Fatal(err)
	}
	var got platformv1alpha1.ApplicationRequest
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "idp-system", Name: "test"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != platformv1alpha1.ApplicationRequestRejected {
		t.Fatalf("status=%#v", got.Status)
	}
}

func TestAddApplicationToKustomizationIsIdempotent(t *testing.T) {
	input := []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- test\n")
	updated, exists, err := addApplicationToKustomization(input, "test")
	if err != nil || !exists || string(updated) != string(input) {
		t.Fatalf("updated=%s exists=%v err=%v", updated, exists, err)
	}
	if _, _, err := addApplicationToKustomization([]byte("resources: not-a-list\n"), "test"); err == nil {
		t.Fatal("invalid resources unexpectedly accepted")
	}
	if !errors.Is(githubrepo.ErrNotFound, githubrepo.ErrNotFound) {
		t.Fatal("sanity")
	}
}
