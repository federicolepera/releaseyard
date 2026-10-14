package helmrelease

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	testReplicasKey     = "replicas"
	testReleaseName     = "demo"
	testNamespace       = "target"
	testHelmOwner       = "helm"
	testOwnerLabelKey   = "owner"
	testNameLabelKey    = "name"
	testVersionLabelKey = "version"
	testChartVersion    = "1.2.3"
	testRepoName        = "repo"
)

func testRequest(archive []byte) Request {
	sum := sha256.Sum256(archive)
	return Request{Namespace: testNamespace, ReleaseName: testReleaseName, Repository: testRepoName, ChartName: testReleaseName, ChartVersion: testChartVersion, Digest: hex.EncodeToString(sum[:]), Archive: archive, ApplicationUID: "app-uid", ClaimUID: "claim-uid"}
}

type testRESTGetter struct {
	config    *rest.Config
	namespace string
}

func (g testRESTGetter) ToRESTConfig() (*rest.Config, error) { return rest.CopyConfig(g.config), nil }
func (g testRESTGetter) BoundNamespace() string              { return g.namespace }
func (testRESTGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	return nil, nil
}
func (testRESTGetter) ToRESTMapper() (meta.RESTMapper, error)        { return nil, nil }
func (testRESTGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig { return nil }

func archive(entries ...tar.Header) []byte {
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		_ = tw.WriteHeader(&h)
		if h.Size > 0 {
			_, _ = tw.Write(bytes.Repeat([]byte("x"), int(h.Size)))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return out.Bytes()
}

func archiveWithContent(name string, content []byte) []byte {
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(content))})
	_, _ = tw.Write(content)
	_ = tw.Close()
	_ = gz.Close()
	return out.Bytes()
}

func TestDigestRejectedBeforeArchiveParsing(t *testing.T) {
	req := testRequest([]byte("not a gzip archive"))
	req.Digest = hex.EncodeToString(make([]byte, sha256.Size))
	_, _, err := validateAndLoad(req)
	if err == nil || err.Error() != "chart archive digest mismatch" {
		t.Fatalf("expected digest rejection before parsing, got %v", err)
	}
}

func TestArchiveLimitsAndUnsafeEntries(t *testing.T) {
	unsafe := archive(tar.Header{Name: "../escape", Typeflag: tar.TypeReg})
	if err := inspectArchive(unsafe); err == nil {
		t.Fatal("expected traversal path rejection")
	}
	link := archive(tar.Header{Name: "demo/link", Linkname: "target", Typeflag: tar.TypeSymlink})
	if err := inspectArchive(link); err == nil {
		t.Fatal("expected symlink rejection")
	}
	for _, name := range []string{"demo/charts/dependency/Chart.yaml", "demo/crds/resource.yaml", "demo/values.schema.json"} {
		if err := inspectArchive(archive(tar.Header{Name: name, Typeflag: tar.TypeReg})); err == nil {
			t.Errorf("expected unsupported chart content rejection for %q", name)
		}
	}
	duplicate := archive(tar.Header{Name: "demo/templates/resource.yaml", Typeflag: tar.TypeReg}, tar.Header{Name: "demo/templates/resource.yaml", Typeflag: tar.TypeReg})
	if err := inspectArchive(duplicate); err == nil {
		t.Fatal("expected duplicate path rejection")
	}
	hooks := archiveWithContent("demo/templates/hook.yaml", []byte("metadata:\n  annotations:\n    helm.sh/hook: pre-install\n"))
	if err := inspectArchive(hooks); err == nil {
		t.Fatal("expected hook template rejection")
	}
	tooLarge := archive(tar.Header{Name: "demo/file", Typeflag: tar.TypeReg, Size: maxExpandedBytes + 1})
	if err := inspectArchive(tooLarge); err == nil {
		t.Fatal("expected expanded size rejection")
	}
	oversized := bytes.Repeat([]byte("a"), maxArchiveBytes+1)
	if _, _, err := validateAndLoad(testRequest(oversized)); err == nil || err.Error() != "chart archive exceeds size limit" {
		t.Fatal("expected compressed size rejection")
	}
}

func TestChartMetadataMustMatchRequest(t *testing.T) {
	data := chartArchive("apiVersion: v2\nname: other\nversion: 1.2.3\ntype: application\n")
	req := testRequest(data)
	_, _, err := validateAndLoad(req)
	if err == nil || err.Error() != "chart metadata does not match expected identity" {
		t.Fatalf("expected metadata mismatch, got %v", err)
	}
}

func chartArchive(metadata string) []byte {
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "demo/Chart.yaml", Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(metadata))})
	_, _ = tw.Write([]byte(metadata))
	_ = tw.Close()
	_ = gz.Close()
	return out.Bytes()
}

func TestHistoryOwnershipAndRecoveryStates(t *testing.T) {
	req := testRequest(nil)
	labels := map[string]string{ownerLabel: req.ApplicationUID, claimLabel: req.ClaimUID}
	newRevision := func(version int, status release.Status) *release.Release {
		return &release.Release{Name: req.ReleaseName, Namespace: req.Namespace, Version: version, Labels: labels, Info: &release.Info{Status: status}}
	}
	rev9 := newRevision(9, release.StatusSuperseded)
	rev10 := newRevision(10, release.StatusDeployed)
	if latest, err := validateApplyHistory([]*release.Release{rev10, rev9}, req); err != nil || latest != rev10 {
		t.Fatalf("expected owned deployed history, got %v", err)
	}
	if latest, err := validateApplyHistory([]*release.Release{rev9, rev10}, req); err != nil || latest != rev10 {
		t.Fatalf("expected revision 10 to be selected independent of history order, got %v", err)
	}
	unmanaged := newRevision(1, release.StatusFailed)
	unmanaged.Labels = map[string]string{}
	if _, err := validateApplyHistory([]*release.Release{unmanaged}, req); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("expected unmanaged history to be rejected before state handling, got %v", err)
	}
	for _, status := range []release.Status{release.StatusFailed, release.StatusPendingInstall, release.StatusPendingUpgrade, release.StatusPendingRollback} {
		rel := newRevision(1, status)
		if _, err := validateApplyHistory([]*release.Release{rel}, req); !errors.Is(err, ErrRecoveryRequired) {
			t.Errorf("status %q: expected recovery required, got %v", status, err)
		}
	}
	previousUnmanaged := newRevision(9, release.StatusSuperseded)
	previousUnmanaged.Labels = map[string]string{}
	if _, err := validateApplyHistory([]*release.Release{previousUnmanaged, rev10}, req); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("expected unmanaged prior revision to block mutation, got %v", err)
	}
	duplicate := newRevision(10, release.StatusDeployed)
	if _, err := validateApplyHistory([]*release.Release{rev10, duplicate}, req); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("expected duplicate revisions to require recovery, got %v", err)
	}
	for _, status := range []release.Status{release.StatusFailed, release.StatusUninstalling, release.StatusUninstalled} {
		rel := newRevision(1, status)
		if err := validateUninstallHistory([]*release.Release{rel}, req); err != nil {
			t.Errorf("uninstall status %q should be recoverable: %v", status, err)
		}
	}
	pending := newRevision(10, release.StatusPendingUpgrade)
	if err := validateUninstallHistory([]*release.Release{rev9, pending}, req); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("expected highest pending revision to block uninstall, got %v", err)
	}
	minimalUninstallRequest := Request{Namespace: testNamespace, ReleaseName: testReleaseName, ApplicationUID: req.ApplicationUID, ClaimUID: req.ClaimUID}
	if err := validateOwnershipRequest(minimalUninstallRequest); err != nil {
		t.Fatalf("uninstall should not require chart coordinates or digest: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*release.Release)
	}{
		{name: "wrong name", mutate: func(rel *release.Release) { rel.Name = "other-release" }},
		{name: "wrong namespace", mutate: func(rel *release.Release) { rel.Namespace = "other-namespace" }},
		{name: "missing historical info", mutate: func(rel *release.Release) { rel.Info = nil }},
		{name: "unknown status", mutate: func(rel *release.Release) { rel.Info.Status = release.StatusUnknown }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rel := newRevision(9, release.StatusSuperseded)
			tc.mutate(rel)
			history := []*release.Release{rel, newRevision(10, release.StatusDeployed)}
			if _, err := validateApplyHistory(history, req); !errors.Is(err, ErrRecoveryRequired) {
				t.Errorf("apply accepted malformed history: %v", err)
			}
			if err := validateUninstallHistory(history, req); !errors.Is(err, ErrRecoveryRequired) {
				t.Errorf("uninstall accepted malformed history: %v", err)
			}
		})
	}
}

func TestRawHistoryReconciliationAcrossSecretPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		list := &corev1.SecretList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "SecretList"}}
		if r.URL.Query().Get("continue") == "page-2" {
			list.Items = []corev1.Secret{{ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.demo.v10", Labels: map[string]string{testOwnerLabelKey: testHelmOwner, testNameLabelKey: testReleaseName, testVersionLabelKey: "10"}}}}
		} else {
			list.Items = []corev1.Secret{{ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.demo.v9", Labels: map[string]string{testOwnerLabelKey: testHelmOwner, testNameLabelKey: testReleaseName, testVersionLabelKey: "9"}}}}
			list.Continue = "page-2"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	}))
	defer server.Close()
	getter := testRESTGetter{config: &rest.Config{Host: server.URL}}
	raw, err := listRawReleaseRevisions(context.Background(), getter, testNamespace, testReleaseName)
	if err != nil {
		t.Fatalf("listing raw release revisions failed: %v", err)
	}
	revisions := []*release.Release{{Version: 10}, {Version: 9}}
	if err := reconcileRawHistory(raw, revisions); err != nil {
		t.Fatalf("matching paginated raw history did not reconcile: %v", err)
	}
	if err := reconcileRawHistory(raw, revisions[:1]); err == nil {
		t.Fatal("expected a skipped/corrupt release secret to fail closed")
	}
}

func TestChartIdentityIncludesCanonicalValues(t *testing.T) {
	const alphaKey = "alpha"
	const betaKey = "beta"
	digest := sha256.Sum256([]byte("archive"))
	digestHex := hex.EncodeToString(digest[:])
	first, err := DesiredIdentity(testRepoName, testReleaseName, testChartVersion, digestHex, map[string]any{alphaKey: "a", betaKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := DesiredIdentity(testRepoName, testReleaseName, testChartVersion, digestHex, map[string]any{betaKey: "b", alphaKey: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if first != reordered {
		t.Fatal("map key order changed chart identity")
	}
	changed, err := DesiredIdentity(testRepoName, testReleaseName, testChartVersion, digestHex, map[string]any{alphaKey: "changed", betaKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if first == changed {
		t.Fatal("changed values did not change chart identity")
	}
	changedDigest := sha256.Sum256([]byte("different archive"))
	changed, err = DesiredIdentity(testRepoName, testReleaseName, testChartVersion, hex.EncodeToString(changedDigest[:]), map[string]any{alphaKey: "a", betaKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if first == changed {
		t.Fatal("changed digest did not change chart identity")
	}

	largeA, err := DesiredIdentity(testRepoName, testReleaseName, testChartVersion, digestHex, map[string]any{testReplicasKey: int64(9007199254740992)})
	if err != nil {
		t.Fatal(err)
	}
	largeB, err := DesiredIdentity(testRepoName, testReleaseName, testChartVersion, digestHex, map[string]any{testReplicasKey: int64(9007199254740993)})
	if err != nil {
		t.Fatal(err)
	}
	if largeA == largeB {
		t.Fatal("distinct int64 values above 2^53 produced the same identity")
	}
}

func TestDesiredIdentityMatchesValidatedArchiveIdentity(t *testing.T) {
	req := actionTestRequest("app-config")
	_, appliedIdentity, err := validateAndLoad(req)
	if err != nil {
		t.Fatal(err)
	}
	desiredIdentity, err := DesiredIdentity(req.Repository, req.ChartName, req.ChartVersion, req.Digest, req.Values)
	if err != nil {
		t.Fatal(err)
	}
	if desiredIdentity != appliedIdentity {
		t.Fatalf("helper identity %q differs from validated archive identity %q", desiredIdentity, appliedIdentity)
	}
}

func TestDesiredIdentityRejectsMalformedInput(t *testing.T) {
	validDigest := hex.EncodeToString(make([]byte, sha256.Size))
	for _, tc := range []struct {
		name       string
		repository string
		nameValue  string
		version    string
		digest     string
	}{
		{name: "empty repository", nameValue: testReleaseName, version: testChartVersion, digest: validDigest},
		{name: "empty chart name", repository: testRepoName, version: testChartVersion, digest: validDigest},
		{name: "empty version", repository: testRepoName, nameValue: testReleaseName, digest: validDigest},
		{name: "short digest", repository: testRepoName, nameValue: testReleaseName, version: testChartVersion, digest: "abcd"},
		{name: "non-hex digest", repository: testRepoName, nameValue: testReleaseName, version: testChartVersion, digest: strings.Repeat("z", sha256.Size*2)},
		{name: "oversized digest", repository: testRepoName, nameValue: testReleaseName, version: testChartVersion, digest: validDigest + "00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DesiredIdentity(tc.repository, tc.nameValue, tc.version, tc.digest, nil); err == nil {
				t.Fatal("expected malformed desired identity input to be rejected")
			}
		})
	}
	upperDigest := strings.ToUpper(validDigest)
	lowerIdentity, err := DesiredIdentity(testRepoName, testReleaseName, testChartVersion, validDigest, nil)
	if err != nil {
		t.Fatal(err)
	}
	upperIdentity, err := DesiredIdentity(testRepoName, testReleaseName, testChartVersion, "SHA256:"+upperDigest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if upperIdentity != lowerIdentity {
		t.Fatal("uppercase SHA-256 digest form changed identity")
	}
}

func TestIdentityLabelIsKubernetesLabelValue(t *testing.T) {
	digest := sha256.Sum256([]byte("archive"))
	identity, err := DesiredIdentity(testRepoName, testReleaseName, testChartVersion, hex.EncodeToString(digest[:]), map[string]any{testReplicasKey: int64(3)})
	if err != nil {
		t.Fatal(err)
	}
	if problems := validation.IsValidLabelValue(identity); len(problems) != 0 {
		t.Fatalf("identity label %q is invalid: %v", identity, problems)
	}
	if len(identity) > 63 {
		t.Fatalf("identity label length %d exceeds label limit", len(identity))
	}
}
