package helmrelease

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/rest"
)

type actionTestKube struct {
	client       resource.RESTClient
	resourceName string
	waitError    error
	waitStarted  chan struct{}
	releaseWait  chan struct{}
	mu           sync.Mutex
	creates      int
	updates      int
}

func (k *actionTestKube) IsReachable() error { return nil }
func (k *actionTestKube) Build(io.Reader, bool) (kube.ResourceList, error) {
	return kube.ResourceList{&resource.Info{
		Client:    k.client,
		Mapping:   &meta.RESTMapping{Resource: corev1.SchemeGroupVersion.WithResource("configmaps"), GroupVersionKind: corev1.SchemeGroupVersion.WithKind("ConfigMap"), Scope: meta.RESTScopeNamespace},
		Namespace: testNamespace,
		Name:      k.resourceName,
		Object:    &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: k.resourceName, Namespace: testNamespace}},
	}}, nil
}
func (k *actionTestKube) Create(kube.ResourceList) (*kube.Result, error) {
	k.mu.Lock()
	k.creates++
	k.mu.Unlock()
	return &kube.Result{}, nil
}
func (k *actionTestKube) WaitWithJobs(resources kube.ResourceList, timeout time.Duration) error {
	return k.Wait(resources, timeout)
}
func (*actionTestKube) Delete(kube.ResourceList) (*kube.Result, []error) {
	return &kube.Result{}, nil
}
func (*actionTestKube) WatchUntilReady(kube.ResourceList, time.Duration) error { return nil }
func (k *actionTestKube) Update(kube.ResourceList, kube.ResourceList, bool) (*kube.Result, error) {
	k.mu.Lock()
	k.updates++
	k.mu.Unlock()
	return &kube.Result{}, nil
}
func (k *actionTestKube) WaitAndGetCompletedPodPhase(string, time.Duration) (corev1.PodPhase, error) {
	return corev1.PodSucceeded, nil
}

type finalWriteLossDriver struct{ *driver.Memory }

func (d *finalWriteLossDriver) Create(key string, rel *release.Release) error {
	return d.Memory.Create(key, cloneActionRelease(rel))
}

func (d *finalWriteLossDriver) Update(key string, rel *release.Release) error {
	if rel.Info != nil && rel.Info.Status == release.StatusDeployed {
		return nil
	}
	return d.Memory.Update(key, cloneActionRelease(rel))
}

func cloneActionRelease(rel *release.Release) *release.Release {
	copy := *rel
	if rel.Info != nil {
		info := *rel.Info
		copy.Info = &info
	}
	if rel.Labels != nil {
		copy.Labels = make(map[string]string, len(rel.Labels))
		maps.Copy(copy.Labels, rel.Labels)
	}
	return &copy
}

func newActionFixture(t *testing.T, resourceName string, existing bool, waitError error, waitStarted, releaseWait chan struct{}, loseFinalWrite bool) (*Installer, *action.Configuration, *actionTestKube, func()) {
	t.Helper()
	var cfg *action.Configuration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/secrets") {
			list := &corev1.SecretList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "SecretList"}}
			if cfg != nil {
				if history, err := cfg.Releases.History(testReleaseName); err == nil {
					for _, item := range history {
						list.Items = append(list.Items, corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("sh.helm.release.v1.%s.v%d", testReleaseName, item.Version), Labels: map[string]string{testOwnerLabelKey: testHelmOwner, testNameLabelKey: testReleaseName, testVersionLabelKey: fmt.Sprint(item.Version)}}})
					}
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(list)
			return
		}
		if strings.Contains(r.URL.Path, "/configmaps/") && existing {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(&corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: testNamespace}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(&metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound})
	}))

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	restClient, err := rest.RESTClientFor(&rest.Config{
		Host:    server.URL,
		APIPath: "/api",
		ContentConfig: rest.ContentConfig{
			GroupVersion:         &corev1.SchemeGroupVersion,
			NegotiatedSerializer: serializer.NewCodecFactory(scheme),
			ContentType:          runtime.ContentTypeJSON,
		},
	})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	kubeClient := &actionTestKube{client: restClient, resourceName: resourceName, waitError: waitError, waitStarted: waitStarted, releaseWait: releaseWait}
	mem := driver.NewMemory()
	mem.SetNamespace(testNamespace)
	var helmDriver driver.Driver = mem
	if loseFinalWrite {
		helmDriver = &finalWriteLossDriver{Memory: mem}
	}
	cfg = &action.Configuration{Releases: storage.Init(helmDriver), KubeClient: kubeClient, Capabilities: chartutil.DefaultCapabilities.Copy(), Log: func(string, ...any) {}}
	getter := testRESTGetter{config: &rest.Config{Host: server.URL, Timeout: 30 * time.Second}, namespace: testNamespace}
	installer := &Installer{Getter: getter, Timeout: time.Second, configFactory: func(string) (*action.Configuration, error) { return cfg, nil }}
	return installer, cfg, kubeClient, server.Close
}

func (k *actionTestKube) Wait(resources kube.ResourceList, timeout time.Duration) error {
	if k.waitStarted != nil {
		close(k.waitStarted)
		<-k.releaseWait
	}
	return k.waitError
}

func actionTestRequest(resourceName string) Request {
	archive := actionTestChart(resourceName)
	req := testRequest(archive)
	req.Values = map[string]any{}
	return req
}

func actionTestChart(resourceName string) []byte {
	return actionTestChartWithTemplate(fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n", resourceName))
}

func actionTestChartWithTemplate(template string) []byte {
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	files := []struct{ name, content string }{
		{"demo/Chart.yaml", "apiVersion: v2\nname: demo\nversion: 1.2.3\ntype: application\n"},
		{"demo/templates/configmap.yaml", template},
	}
	for _, file := range files {
		data := []byte(file.content)
		_ = tw.WriteHeader(&tar.Header{Name: file.name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(data))})
		_, _ = tw.Write(data)
	}
	_ = tw.Close()
	_ = gz.Close()
	return out.Bytes()
}

func TestRenderedHooksAndCRDsRejectedForInstallAndUpgrade(t *testing.T) {
	hookTemplate := "{{- if .Values.makeHook }}\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: dynamic-hook\n  annotations:\n    {{ .Values.annotationKey }}: pre-install\n{{- end }}\n"
	crdTemplate := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: widgets.example.com\nspec:\n  group: example.com\n  names:\n    kind: Widget\n    plural: widgets\n  scope: Namespaced\n  versions: []\n"
	reservedTemplate := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: ry-claim-demo\n  namespace: target\n"
	crdListTemplate := "apiVersion: v1\nkind: List\nitems:\n  - apiVersion: apiextensions.k8s.io/v1\n    kind: CustomResourceDefinition\n    metadata:\n      name: widgets.example.com\n    spec:\n      group: example.com\n      names:\n        kind: Widget\n        plural: widgets\n      scope: Namespaced\n      versions: []\n"
	reservedListTemplate := "apiVersion: v1\nkind: List\nitems:\n  - apiVersion: v1\n    kind: ConfigMap\n    metadata:\n      name: ry-claim-demo\n      namespace: target\n"
	for _, tc := range []struct {
		name     string
		template string
		values   map[string]any
	}{
		{name: "dynamic hook annotation", template: hookTemplate, values: map[string]any{"makeHook": true, "annotationKey": "helm.sh/hook"}},
		{name: "rendered crd", template: crdTemplate, values: map[string]any{}},
		{name: "reserved claim configmap", template: reservedTemplate, values: map[string]any{}},
		{name: "list containing rendered crd", template: crdListTemplate, values: map[string]any{}},
		{name: "list containing reserved claim configmap", template: reservedListTemplate, values: map[string]any{}},
	} {
		for _, upgrade := range []bool{false, true} {
			operation := "install"
			if upgrade {
				operation = "upgrade"
			}
			t.Run(tc.name+"/"+operation, func(t *testing.T) {
				installer, cfg, kubeClient, closeServer := newActionFixture(t, "app-config", false, nil, nil, nil, false)
				defer closeServer()
				if upgrade {
					if _, err := installer.Apply(context.Background(), actionTestRequest("app-config")); err != nil {
						t.Fatalf("baseline install failed: %v", err)
					}
				}
				req := testRequest(actionTestChartWithTemplate(tc.template))
				req.Values = tc.values
				if _, err := installer.Apply(context.Background(), req); !errors.Is(err, ErrRecoveryRequired) {
					t.Fatalf("expected rendered content rejection, got %v", err)
				}
				history, historyErr := cfg.Releases.History(testReleaseName)
				if upgrade {
					if historyErr != nil || len(history) != 1 || history[0].Info.Status != release.StatusDeployed {
						t.Fatalf("rejected upgrade changed history: %#v err=%v", history, historyErr)
					}
				} else if !errors.Is(historyErr, driver.ErrReleaseNotFound) {
					t.Fatalf("rejected install wrote history: %v", historyErr)
				}
				kubeClient.mu.Lock()
				creates, updates := kubeClient.creates, kubeClient.updates
				kubeClient.mu.Unlock()
				if creates != boolToInt(upgrade) || updates != 0 {
					t.Fatalf("rejected %s mutated resources: creates=%d updates=%d", operation, creates, updates)
				}
			})
		}
	}
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestHelmActionPersistsOwnershipLabelsAndRevision(t *testing.T) {
	installer, cfg, _, closeServer := newActionFixture(t, "app-config", false, nil, nil, nil, false)
	defer closeServer()
	req := actionTestRequest("app-config")
	result, err := installer.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if result.Outcome != OutcomeInstalled || result.HelmRevision != 1 || result.DesiredIdentity == "" {
		t.Fatalf("unexpected install result: %#v", result)
	}
	history, err := cfg.Releases.History(req.ReleaseName)
	if err != nil || len(history) != 1 {
		t.Fatalf("expected one stored release, history=%d err=%v", len(history), err)
	}
	stored := history[0]
	if stored.Info.Status != release.StatusDeployed || stored.Labels[ownerLabel] != req.ApplicationUID || stored.Labels[claimLabel] != req.ClaimUID || stored.Labels[identityLabel] != result.DesiredIdentity {
		t.Fatalf("stored release identity/status incorrect: status=%q labels=%v", stored.Info.Status, stored.Labels)
	}
	if result.Manifest == "" || result.Manifest != stored.Manifest {
		t.Fatalf("install result did not return the verified release manifest: got %q, want %q", result.Manifest, stored.Manifest)
	}
	again, err := installer.Apply(context.Background(), req)
	if err != nil || again.Outcome != OutcomeUnchanged || again.HelmRevision != 1 {
		t.Fatalf("expected idempotent verified outcome, result=%#v err=%v", again, err)
	}
	if again.Manifest != stored.Manifest {
		t.Fatalf("unchanged result manifest = %q, want %q", again.Manifest, stored.Manifest)
	}
}

func TestHelmUpgradeVerifiesNewOwnedRevision(t *testing.T) {
	installer, cfg, _, closeServer := newActionFixture(t, "app-config", false, nil, nil, nil, false)
	defer closeServer()
	req := actionTestRequest("app-config")
	req.Values = map[string]any{"mode": "first"}
	if _, err := installer.Apply(context.Background(), req); err != nil {
		t.Fatalf("initial install failed: %v", err)
	}
	req.Values = map[string]any{"mode": "second"}
	result, err := installer.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("upgrade failed: %v", err)
	}
	if result.Outcome != OutcomeUpgraded || result.HelmRevision != 2 {
		t.Fatalf("unexpected upgrade result: %#v", result)
	}
	history, err := cfg.Releases.History(req.ReleaseName)
	if err != nil || len(history) != 2 {
		t.Fatalf("expected two revisions, history=%d err=%v", len(history), err)
	}
	latest := history[1]
	if latest.Info.Status != release.StatusDeployed || latest.Labels[ownerLabel] != req.ApplicationUID || latest.Labels[claimLabel] != req.ClaimUID || latest.Labels[identityLabel] != result.DesiredIdentity {
		t.Fatalf("upgraded revision failed ownership verification: status=%q labels=%v", latest.Info.Status, latest.Labels)
	}
	if result.Manifest == "" || result.Manifest != latest.Manifest {
		t.Fatalf("upgrade result did not return the verified release manifest: got %q, want %q", result.Manifest, latest.Manifest)
	}
}

func TestHelmUninstallVerifiesHistoryAndSecretRemoval(t *testing.T) {
	installer, _, _, closeServer := newActionFixture(t, "app-config", false, nil, nil, nil, false)
	defer closeServer()
	req := actionTestRequest("app-config")
	if _, err := installer.Apply(context.Background(), req); err != nil {
		t.Fatalf("install before uninstall failed: %v", err)
	}
	uninstallReq := Request{Namespace: req.Namespace, ReleaseName: req.ReleaseName, ApplicationUID: req.ApplicationUID, ClaimUID: req.ClaimUID}
	result, err := installer.Uninstall(context.Background(), uninstallReq)
	if err != nil {
		t.Fatalf("uninstall failed verification: %v", err)
	}
	if result.Outcome != OutcomeUninstalled || result.HelmRevision != 1 {
		t.Fatalf("unexpected uninstall result: %#v", result)
	}
	if result.Manifest != "" {
		t.Fatalf("uninstall result unexpectedly returned a manifest: %q", result.Manifest)
	}
}

func TestHelmDryRunBlocksExistingAndReservedResources(t *testing.T) {
	for _, tc := range []struct {
		name         string
		resourceName string
		existing     bool
	}{
		{name: "existing object", resourceName: "app-config", existing: true},
		{name: "reserved claim name", resourceName: "ry-claim-demo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installer, cfg, kubeClient, closeServer := newActionFixture(t, tc.resourceName, tc.existing, nil, nil, nil, false)
			defer closeServer()
			_, err := installer.Apply(context.Background(), actionTestRequest(tc.resourceName))
			if !errors.Is(err, ErrRecoveryRequired) {
				t.Fatalf("expected recovery-required preflight rejection, got %v", err)
			}
			if _, err := cfg.Releases.History(testReleaseName); !errors.Is(err, driver.ErrReleaseNotFound) {
				t.Fatalf("dry-run unexpectedly wrote Helm history: %v", err)
			}
			kubeClient.mu.Lock()
			creates := kubeClient.creates
			kubeClient.mu.Unlock()
			if creates != 0 {
				t.Fatalf("dry-run unexpectedly created %d resources", creates)
			}
		})
	}
}

func TestPreviousAttemptWithoutHistoryNeverFreshInstalls(t *testing.T) {
	installer, cfg, kubeClient, closeServer := newActionFixture(t, "app-config", false, nil, nil, nil, false)
	defer closeServer()
	req := actionTestRequest("app-config")
	req.PreviousAttempt = true
	if _, err := installer.Apply(context.Background(), req); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("expected prior-attempt recovery guard, got %v", err)
	}
	if _, err := cfg.Releases.History(testReleaseName); !errors.Is(err, driver.ErrReleaseNotFound) {
		t.Fatalf("previous-attempt guard wrote Helm history: %v", err)
	}
	kubeClient.mu.Lock()
	defer kubeClient.mu.Unlock()
	if kubeClient.creates != 0 {
		t.Fatalf("previous-attempt guard created %d resources", kubeClient.creates)
	}
}

func TestHelmFailedAndUnrecordedFinalRevisionsAreNotReportedSuccessful(t *testing.T) {
	t.Run("failed revision retains ownership labels", func(t *testing.T) {
		installer, cfg, _, closeServer := newActionFixture(t, "app-config", false, errors.New("test wait failure"), nil, nil, false)
		defer closeServer()
		req := actionTestRequest("app-config")
		if _, err := installer.Apply(context.Background(), req); err == nil {
			t.Fatal("expected failed install")
		}
		history, err := cfg.Releases.History(testReleaseName)
		if err != nil || len(history) != 1 {
			t.Fatalf("expected stored failed release, history=%d err=%v", len(history), err)
		}
		failed := history[0]
		if failed.Info.Status != release.StatusFailed || failed.Labels[ownerLabel] != req.ApplicationUID || failed.Labels[claimLabel] != req.ClaimUID {
			t.Fatalf("failed revision lost ownership labels: status=%q labels=%v", failed.Info.Status, failed.Labels)
		}
	})
	t.Run("dropped deployed write is recovery required", func(t *testing.T) {
		installer, cfg, _, closeServer := newActionFixture(t, "app-config", false, nil, nil, nil, true)
		defer closeServer()
		result, err := installer.Apply(context.Background(), actionTestRequest("app-config"))
		if !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("expected missing final write to fail verification, result=%#v err=%v", result, err)
		}
		history, historyErr := cfg.Releases.History(testReleaseName)
		if historyErr != nil || len(history) != 1 || history[0].Info.Status != release.StatusPendingInstall {
			t.Fatalf("expected final-write loss to leave pending revision, history=%#v err=%v", history, historyErr)
		}
		if history[0].Labels[ownerLabel] != "app-uid" || history[0].Labels[claimLabel] != "claim-uid" {
			t.Fatalf("pending revision lost ownership labels: %v", history[0].Labels)
		}
	})
}

func TestCancelledInstallJoinsHelmWorkerBeforeReturning(t *testing.T) {
	started, resume := make(chan struct{}), make(chan struct{})
	installer, _, kubeClient, closeServer := newActionFixture(t, "app-config", false, nil, started, resume, false)
	defer closeServer()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := installer.Apply(ctx, actionTestRequest("app-config"))
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		t.Fatalf("Apply returned before the SDK worker completed: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(resume)
	err := <-done
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("expected canceled but joined operation to report recovery, got %v", err)
	}
	kubeClient.mu.Lock()
	countAtReturn := kubeClient.creates
	kubeClient.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	kubeClient.mu.Lock()
	defer kubeClient.mu.Unlock()
	if kubeClient.creates != countAtReturn || countAtReturn != 1 {
		t.Fatalf("resource mutation continued after Apply returned: at-return=%d later=%d", countAtReturn, kubeClient.creates)
	}
}

func TestPreCancelledApplyDoesNotMutate(t *testing.T) {
	installer, cfg, kubeClient, closeServer := newActionFixture(t, "app-config", false, nil, nil, nil, false)
	defer closeServer()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := installer.Apply(ctx, actionTestRequest("app-config")); err == nil {
		t.Fatal("expected pre-cancelled Apply to fail")
	}
	if _, err := cfg.Releases.History(testReleaseName); !errors.Is(err, driver.ErrReleaseNotFound) {
		t.Fatalf("pre-cancelled Apply wrote Helm history: %v", err)
	}
	kubeClient.mu.Lock()
	defer kubeClient.mu.Unlock()
	if kubeClient.creates != 0 || kubeClient.updates != 0 {
		t.Fatalf("pre-cancelled Apply mutated resources: creates=%d updates=%d", kubeClient.creates, kubeClient.updates)
	}
}

func TestConfigurationRequiresMatchingBoundNamespace(t *testing.T) {
	called := false
	installer := &Installer{Getter: testRESTGetter{namespace: "other"}, configFactory: func(string) (*action.Configuration, error) {
		called = true
		return nil, nil
	}}
	if _, err := installer.configuration(testNamespace); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected bound namespace rejection, got %v", err)
	}
	if called {
		t.Fatal("configuration factory called despite namespace mismatch")
	}
}
