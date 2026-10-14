// Package helmrelease installs verified, locally supplied Helm chart archives.
package helmrelease

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
)

const (
	maxArchiveBytes   = 50 << 20
	maxExpandedBytes  = 100 << 20
	maxArchiveEntries = 2048
	defaultTimeout    = 10 * time.Minute
	ownerLabel        = "platform.releaseyard.io/application-uid"
	claimLabel        = "platform.releaseyard.io/claim-uid"
	identityLabel     = "platform.releaseyard.io/chart-identity"
)

var (
	ErrRecoveryRequired = errors.New("helm release requires manual recovery")
	ErrNotOwned         = errors.New("helm release is not owned by this application claim")
	ErrInvalidInput     = errors.New("invalid helm release input")
)

// Request is the complete identity and data needed for one release operation.
// Digest is the lowercase or uppercase hexadecimal SHA-256 of Archive.
type Request struct {
	Namespace       string
	ReleaseName     string
	Repository      string
	ChartName       string
	ChartVersion    string
	Digest          string
	Archive         []byte
	Values          map[string]any
	ApplicationUID  string
	ClaimUID        string
	PreviousAttempt bool
}

// Installer operates only through the supplied REST getter; it never reads a kubeconfig
// or fetches charts. Hooks are disabled because this MVP does not support hook lifecycle.
type Installer struct {
	Getter  genericclioptions.RESTClientGetter
	Timeout time.Duration

	configFactory func(string) (*action.Configuration, error)
}

type namespaceBoundGetter interface {
	genericclioptions.RESTClientGetter
	BoundNamespace() string
}

// Outcome describes whether a release was installed, upgraded, unchanged, or removed.
type Outcome string

const (
	OutcomeInstalled   Outcome = "installed"
	OutcomeUpgraded    Outcome = "upgraded"
	OutcomeUnchanged   Outcome = "unchanged"
	OutcomeUninstalled Outcome = "uninstalled"
	OutcomeNotFound    Outcome = "not-found"
)

// Result records the state verified after the requested SDK operation.
type Result struct {
	Outcome         Outcome
	HelmRevision    int
	DesiredIdentity string
	Manifest        string
}

// Apply installs the release when no release history exists and upgrades an owned,
// healthy release only when its chart identity differs from the requested identity.
func (i *Installer) Apply(ctx context.Context, req Request) (Result, error) {
	if ctx == nil || ctx.Err() != nil {
		return Result{}, errors.New("helm operation canceled")
	}
	opCtx, cancel := context.WithTimeout(ctx, i.timeout()+60*time.Second)
	defer cancel()
	ch, identity, err := validateAndLoad(req)
	if err != nil {
		return Result{}, err
	}
	getter, err := i.operationGetter(opCtx, req.Namespace)
	if err != nil {
		return Result{}, errors.New("could not initialize helm client")
	}
	cfg, err := i.configurationWithGetter(req.Namespace, getter)
	if err != nil {
		return Result{}, errors.New("could not initialize helm client")
	}
	history, err := action.NewHistory(cfg).Run(req.ReleaseName)
	rawRevisions, rawErr := listRawReleaseRevisions(opCtx, getter, req.Namespace, req.ReleaseName)
	if rawErr != nil {
		return Result{}, ErrRecoveryRequired
	}
	if err != nil {
		if errors.Is(err, driver.ErrReleaseNotFound) {
			if reconcileRawHistory(rawRevisions, history) != nil {
				return Result{}, ErrRecoveryRequired
			}
		} else {
			if reconcileRawHistory(rawRevisions, history) != nil {
				return Result{}, ErrRecoveryRequired
			}
			return Result{}, errors.New("could not inspect helm release history")
		}
		if req.PreviousAttempt {
			return Result{}, ErrRecoveryRequired
		}
		if err := preflightRenderedChart(opCtx, cfg, req, ch, identity, false); err != nil {
			return Result{}, ErrRecoveryRequired
		}
		return i.install(opCtx, getter, cfg, req, ch, identity)
	}
	if reconcileRawHistory(rawRevisions, history) != nil {
		return Result{}, ErrRecoveryRequired
	}
	if len(history) == 0 {
		if req.PreviousAttempt {
			return Result{}, ErrRecoveryRequired
		}
		if err := preflightRenderedChart(opCtx, cfg, req, ch, identity, false); err != nil {
			return Result{}, ErrRecoveryRequired
		}
		return i.install(opCtx, getter, cfg, req, ch, identity)
	}
	latest, err := validateApplyHistory(history, req)
	if err != nil {
		return Result{}, err
	}
	if latest.Labels[identityLabel] == identity {
		return Result{Outcome: OutcomeUnchanged, HelmRevision: latest.Version, DesiredIdentity: identity, Manifest: latest.Manifest}, nil
	}
	if opCtx.Err() != nil {
		return Result{}, errors.New("helm operation canceled")
	}
	if err := preflightRenderedChart(opCtx, cfg, req, ch, identity, true); err != nil {
		return Result{}, ErrRecoveryRequired
	}
	upgrade := action.NewUpgrade(cfg)
	upgrade.Namespace, upgrade.Wait, upgrade.DisableHooks = req.Namespace, true, true
	upgrade.Timeout, upgrade.ResetValues, upgrade.Labels = i.timeout(), true, releaseLabels(req, identity)
	returned, err := upgrade.RunWithContext(context.WithoutCancel(opCtx), req.ReleaseName, ch, req.Values)
	if err != nil {
		if opCtx.Err() != nil {
			return Result{}, ErrRecoveryRequired
		}
		return Result{}, errors.New("helm upgrade failed; manual recovery may be required")
	}
	verified, err := verifyAppliedRelease(opCtx, getter, cfg, req, identity, returned)
	if err != nil || opCtx.Err() != nil {
		return Result{}, ErrRecoveryRequired
	}
	return Result{Outcome: OutcomeUpgraded, HelmRevision: returned.Version, DesiredIdentity: identity, Manifest: verified.Manifest}, nil
}

func (i *Installer) install(ctx context.Context, getter genericclioptions.RESTClientGetter, cfg *action.Configuration, req Request, ch *chart.Chart, identity string) (Result, error) {
	if ctx.Err() != nil {
		return Result{}, errors.New("helm operation canceled")
	}
	install := action.NewInstall(cfg)
	install.ReleaseName, install.Namespace = req.ReleaseName, req.Namespace
	install.CreateNamespace, install.Wait, install.DisableHooks = false, true, true
	install.Timeout, install.Labels = i.timeout(), releaseLabels(req, identity)
	returned, err := install.RunWithContext(context.WithoutCancel(ctx), ch, req.Values)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ErrRecoveryRequired
		}
		return Result{}, errors.New("helm installation failed; manual recovery may be required")
	}
	verified, err := verifyAppliedRelease(ctx, getter, cfg, req, identity, returned)
	if err != nil || ctx.Err() != nil {
		return Result{}, ErrRecoveryRequired
	}
	return Result{Outcome: OutcomeInstalled, HelmRevision: returned.Version, DesiredIdentity: identity, Manifest: verified.Manifest}, nil
}

// Uninstall removes only a release whose complete stored history is owned by this request.
func (i *Installer) Uninstall(ctx context.Context, req Request) (Result, error) {
	if ctx == nil || ctx.Err() != nil {
		return Result{}, errors.New("helm operation canceled")
	}
	opCtx, cancel := context.WithTimeout(ctx, i.timeout()+60*time.Second)
	defer cancel()
	if err := validateOwnershipRequest(req); err != nil {
		return Result{}, err
	}
	getter, err := i.operationGetter(opCtx, req.Namespace)
	if err != nil {
		return Result{}, errors.New("could not initialize helm client")
	}
	cfg, err := i.configurationWithGetter(req.Namespace, getter)
	if err != nil {
		return Result{}, errors.New("could not initialize helm client")
	}
	history, err := action.NewHistory(cfg).Run(req.ReleaseName)
	rawRevisions, rawErr := listRawReleaseRevisions(opCtx, getter, req.Namespace, req.ReleaseName)
	if rawErr != nil {
		return Result{}, ErrRecoveryRequired
	}
	if err != nil {
		if errors.Is(err, driver.ErrReleaseNotFound) {
			if reconcileRawHistory(rawRevisions, history) != nil {
				return Result{}, ErrRecoveryRequired
			}
			return Result{Outcome: OutcomeNotFound}, nil
		}
		if reconcileRawHistory(rawRevisions, history) != nil {
			return Result{}, ErrRecoveryRequired
		}
		return Result{}, errors.New("could not inspect helm release history")
	}
	if reconcileRawHistory(rawRevisions, history) != nil {
		return Result{}, ErrRecoveryRequired
	}
	if len(history) == 0 {
		return Result{Outcome: OutcomeNotFound}, nil
	}
	if err := validateUninstallHistory(history, req); err != nil {
		return Result{}, err
	}
	latest, err := validateOwnedHistory(history, req)
	if err != nil {
		return Result{}, err
	}
	if opCtx.Err() != nil {
		return Result{}, errors.New("helm operation canceled")
	}
	u := action.NewUninstall(cfg)
	u.DisableHooks, u.Wait, u.Timeout = true, true, i.timeout()
	if _, err := u.Run(req.ReleaseName); err != nil {
		if opCtx.Err() != nil {
			return Result{}, ErrRecoveryRequired
		}
		return Result{}, errors.New("helm uninstall failed; manual recovery may be required")
	}
	if verifyReleaseAbsent(opCtx, getter, cfg, req.Namespace, req.ReleaseName) != nil || opCtx.Err() != nil {
		return Result{}, ErrRecoveryRequired
	}
	return Result{Outcome: OutcomeUninstalled, HelmRevision: latest.Version}, nil
}

func (i *Installer) configuration(namespace string) (*action.Configuration, error) {
	if i == nil || namespace == "" {
		return nil, ErrInvalidInput
	}
	getter, ok := i.Getter.(namespaceBoundGetter)
	if !ok || getter.BoundNamespace() != namespace {
		return nil, ErrInvalidInput
	}
	return i.configurationWithGetter(namespace, i.Getter)
}

func (i *Installer) operationGetter(ctx context.Context, namespace string) (namespaceBoundGetter, error) {
	if i == nil {
		return nil, ErrInvalidInput
	}
	base, ok := i.Getter.(namespaceBoundGetter)
	if !ok || base.BoundNamespace() != namespace {
		return nil, ErrInvalidInput
	}
	return newOperationGetter(base, ctx)
}

func (i *Installer) configurationWithGetter(namespace string, getter genericclioptions.RESTClientGetter) (*action.Configuration, error) {
	if i.configFactory != nil {
		return i.configFactory(namespace)
	}
	cfg := new(action.Configuration)
	if err := cfg.Init(getter, namespace, "secret", func(string, ...any) {}); err != nil {
		return nil, err
	}
	return cfg, nil
}

func preflightRenderedChart(ctx context.Context, cfg *action.Configuration, req Request, ch *chart.Chart, identity string, upgrading bool) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrRecoveryRequired
	}
	var rel *release.Release
	var err error
	if upgrading {
		dryRun := action.NewUpgrade(cfg)
		dryRun.Namespace, dryRun.DryRun, dryRun.DryRunOption = req.Namespace, true, "server"
		dryRun.DisableHooks, dryRun.ResetValues = true, true
		dryRun.Labels = releaseLabels(req, identity)
		rel, err = dryRun.RunWithContext(context.WithoutCancel(ctx), req.ReleaseName, ch, req.Values)
	} else {
		dryRun := action.NewInstall(cfg)
		dryRun.ReleaseName, dryRun.Namespace = req.ReleaseName, req.Namespace
		dryRun.DryRun, dryRun.DryRunOption, dryRun.DisableHooks = true, "server", true
		dryRun.CreateNamespace = false
		rel, err = dryRun.RunWithContext(context.WithoutCancel(ctx), ch, req.Values)
	}
	if err != nil || rel == nil || len(rel.Hooks) != 0 {
		return ErrRecoveryRequired
	}
	if ctx.Err() != nil {
		return ErrRecoveryRequired
	}
	if err := validateRenderedManifest(rel.Manifest, req); err != nil {
		return err
	}
	if upgrading {
		return nil
	}
	resources, err := cfg.KubeClient.Build(strings.NewReader(rel.Manifest), false)
	if err != nil {
		return ErrRecoveryRequired
	}
	reservedName := "ry-claim-" + req.ReleaseName
	for _, info := range resources {
		if info == nil || info.Name == reservedName {
			return ErrRecoveryRequired
		}
		if err := info.Get(); err == nil || !apierrors.IsNotFound(err) {
			return ErrRecoveryRequired
		}
	}
	return nil
}

func validateRenderedManifest(manifest string, req Request) error {
	decoder := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	for {
		var object map[string]any
		if err := decoder.Decode(&object); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return ErrRecoveryRequired
		}
		if object == nil {
			continue
		}
		kind, _ := object["kind"].(string)
		if kind == "List" || strings.HasSuffix(kind, "List") {
			return ErrRecoveryRequired
		}
		if kind == "CustomResourceDefinition" {
			return ErrRecoveryRequired
		}
		if kind != "ConfigMap" {
			continue
		}
		metadata, _ := object["metadata"].(map[string]any)
		name, _ := metadata["name"].(string)
		namespace, _ := metadata["namespace"].(string)
		if name == "ry-claim-"+req.ReleaseName && (namespace == "" || namespace == req.Namespace) {
			return ErrRecoveryRequired
		}
	}
}

func verifyAppliedRelease(ctx context.Context, getter genericclioptions.RESTClientGetter, cfg *action.Configuration, req Request, identity string, returned *release.Release) (*release.Release, error) {
	if returned == nil || returned.Version <= 0 {
		return nil, ErrRecoveryRequired
	}
	history, err := action.NewHistory(cfg).Run(req.ReleaseName)
	if err != nil {
		return nil, ErrRecoveryRequired
	}
	rawRevisions, err := listRawReleaseRevisions(ctx, getter, req.Namespace, req.ReleaseName)
	if err != nil || reconcileRawHistory(rawRevisions, history) != nil {
		return nil, ErrRecoveryRequired
	}
	latest, err := validateOwnedHistory(history, req)
	if err != nil || latest.Version != returned.Version {
		return nil, ErrRecoveryRequired
	}
	var stored *release.Release
	for _, revision := range history {
		if revision.Version == returned.Version {
			stored = revision
			break
		}
	}
	if stored == nil || stored.Info == nil || stored.Info.Status != release.StatusDeployed || stored.Labels[ownerLabel] != req.ApplicationUID || stored.Labels[claimLabel] != req.ClaimUID || stored.Labels[identityLabel] != identity {
		return nil, ErrRecoveryRequired
	}
	return stored, nil
}

func verifyReleaseAbsent(ctx context.Context, getter genericclioptions.RESTClientGetter, cfg *action.Configuration, namespace, releaseName string) error {
	history, historyErr := action.NewHistory(cfg).Run(releaseName)
	rawRevisions, rawErr := listRawReleaseRevisions(ctx, getter, namespace, releaseName)
	if rawErr != nil || len(rawRevisions) != 0 || len(history) != 0 {
		return ErrRecoveryRequired
	}
	if historyErr != nil && !errors.Is(historyErr, driver.ErrReleaseNotFound) {
		return ErrRecoveryRequired
	}
	return nil
}

func listRawReleaseRevisions(ctx context.Context, getter genericclioptions.RESTClientGetter, namespace, releaseName string) ([]int, error) {
	if getter == nil || namespace == "" || releaseName == "" {
		return nil, ErrRecoveryRequired
	}
	cfg, err := getter.ToRESTConfig()
	if err != nil {
		return nil, errors.New("could not inspect raw helm release storage")
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, errors.New("could not inspect raw helm release storage")
	}
	prefix := "sh.helm.release.v1." + releaseName + ".v"
	var revisions []int
	seen := make(map[int]struct{})
	seenContinue := make(map[string]struct{})
	continueToken := ""
	for {
		secrets, err := client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{Limit: 500, Continue: continueToken})
		if err != nil {
			return nil, errors.New("could not inspect raw helm release storage")
		}
		for _, secret := range secrets.Items {
			if !strings.HasPrefix(secret.Name, prefix) {
				continue
			}
			if secret.Labels["owner"] != "helm" {
				return nil, ErrRecoveryRequired
			}
			if secret.Labels["name"] != releaseName {
				return nil, ErrRecoveryRequired
			}
			suffix := strings.TrimPrefix(secret.Name, prefix)
			version, err := strconv.Atoi(suffix)
			if err != nil || version <= 0 || strconv.Itoa(version) != suffix {
				return nil, ErrRecoveryRequired
			}
			if secret.Labels["version"] != suffix {
				return nil, ErrRecoveryRequired
			}
			if _, duplicate := seen[version]; duplicate {
				return nil, ErrRecoveryRequired
			}
			seen[version] = struct{}{}
			revisions = append(revisions, version)
		}
		continueToken = secrets.Continue
		if continueToken == "" {
			break
		}
		if _, repeated := seenContinue[continueToken]; repeated {
			return nil, ErrRecoveryRequired
		}
		seenContinue[continueToken] = struct{}{}
	}
	return revisions, nil
}

func reconcileRawHistory(rawRevisions []int, history []*release.Release) error {
	if len(rawRevisions) != len(history) {
		return ErrRecoveryRequired
	}
	raw := make(map[int]struct{}, len(rawRevisions))
	for _, version := range rawRevisions {
		if version <= 0 {
			return ErrRecoveryRequired
		}
		if _, duplicate := raw[version]; duplicate {
			return ErrRecoveryRequired
		}
		raw[version] = struct{}{}
	}
	decoded := make(map[int]struct{}, len(history))
	for _, revision := range history {
		if revision == nil || revision.Version <= 0 {
			return ErrRecoveryRequired
		}
		if _, duplicate := decoded[revision.Version]; duplicate {
			return ErrRecoveryRequired
		}
		if _, exists := raw[revision.Version]; !exists {
			return ErrRecoveryRequired
		}
		decoded[revision.Version] = struct{}{}
	}
	return nil
}

func (i *Installer) timeout() time.Duration {
	if i == nil || i.Timeout <= 0 || i.Timeout > 30*time.Minute {
		return defaultTimeout
	}
	return i.Timeout
}

func validateAndLoad(req Request) (*chart.Chart, string, error) {
	if err := validateIdentity(req); err != nil {
		return nil, "", err
	}
	if len(req.Archive) == 0 {
		return nil, "", ErrInvalidInput
	}
	digest := sha256.Sum256(req.Archive)
	want, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(req.Digest), "sha256:"))
	if err != nil || len(want) != sha256.Size || !bytes.Equal(want, digest[:]) {
		return nil, "", errors.New("chart archive digest mismatch")
	}
	if len(req.Archive) > maxArchiveBytes {
		return nil, "", errors.New("chart archive exceeds size limit")
	}
	if err := inspectArchive(req.Archive); err != nil {
		return nil, "", err
	}
	ch, err := loader.LoadArchive(bytes.NewReader(req.Archive))
	if err != nil || ch == nil || ch.Metadata == nil {
		return nil, "", errors.New("invalid chart archive")
	}
	if ch.Metadata.Name != req.ChartName || ch.Metadata.Version != req.ChartVersion || ch.Metadata.Type != "application" {
		return nil, "", errors.New("chart metadata does not match expected identity")
	}
	if len(ch.Metadata.Dependencies) != 0 {
		return nil, "", errors.New("chart dependencies are unsupported")
	}
	identity, err := DesiredIdentity(req.Repository, req.ChartName, req.ChartVersion, req.Digest, req.Values)
	if err != nil {
		return nil, "", errors.New("invalid chart values")
	}
	return ch, identity, nil
}

func validateIdentity(req Request) error {
	if req.Namespace == "" || req.ReleaseName == "" || req.Repository == "" || req.ChartName == "" || req.ChartVersion == "" || req.ApplicationUID == "" || req.ClaimUID == "" {
		return ErrInvalidInput
	}
	return nil
}

func validateOwnershipRequest(req Request) error {
	if req.Namespace == "" || req.ReleaseName == "" || req.ApplicationUID == "" || req.ClaimUID == "" {
		return ErrInvalidInput
	}
	return nil
}

// DesiredIdentity deterministically identifies a chart and its values without
// loading the archive or performing network or Helm operations. Digest accepts
// a hexadecimal SHA-256 digest, optionally prefixed with "sha256:".
func DesiredIdentity(repository, name, version, digest string, values map[string]any) (string, error) {
	if repository == "" || name == "" || version == "" {
		return "", ErrInvalidInput
	}
	digestBytes, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(digest), "sha256:"))
	if err != nil || len(digestBytes) != sha256.Size {
		return "", ErrInvalidInput
	}
	canonicalValues, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	identityInput, err := json.Marshal([]any{repository, name, version, hex.EncodeToString(digestBytes), json.RawMessage(canonicalValues)})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(identityInput)
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]), nil
}

func releaseLabels(req Request, identity string) map[string]string {
	return map[string]string{ownerLabel: req.ApplicationUID, claimLabel: req.ClaimUID, identityLabel: identity}
}

func owned(rel *release.Release, req Request) bool {
	return rel != nil && rel.Labels != nil && rel.Labels[ownerLabel] == req.ApplicationUID && rel.Labels[claimLabel] == req.ClaimUID
}

func validateOwnedHistory(history []*release.Release, req Request) (*release.Release, error) {
	if len(history) == 0 {
		return nil, ErrRecoveryRequired
	}
	seen := make(map[int]struct{}, len(history))
	for _, revision := range history {
		if revision == nil || revision.Version <= 0 || revision.Name != req.ReleaseName || revision.Namespace != req.Namespace || revision.Info == nil || !recognizedReleaseStatus(revision.Info.Status) {
			return nil, ErrRecoveryRequired
		}
		if _, exists := seen[revision.Version]; exists {
			return nil, ErrRecoveryRequired
		}
		seen[revision.Version] = struct{}{}
		if !owned(revision, req) {
			return nil, ErrNotOwned
		}
	}
	latest := history[0]
	for _, revision := range history[1:] {
		if revision.Version > latest.Version {
			latest = revision
		}
	}
	return latest, nil
}

func recognizedReleaseStatus(status release.Status) bool {
	switch status {
	case release.StatusDeployed, release.StatusUninstalled, release.StatusSuperseded, release.StatusFailed, release.StatusUninstalling, release.StatusPendingInstall, release.StatusPendingUpgrade, release.StatusPendingRollback:
		return true
	default:
		return false
	}
}

func validateApplyHistory(history []*release.Release, req Request) (*release.Release, error) {
	latest, err := validateOwnedHistory(history, req)
	if err != nil {
		return nil, err
	}
	if latest.Info == nil || latest.Info.Status != release.StatusDeployed {
		return nil, ErrRecoveryRequired
	}
	return latest, nil
}

func validateUninstallHistory(history []*release.Release, req Request) error {
	latest, err := validateOwnedHistory(history, req)
	if err != nil {
		return err
	}
	if latest.Info == nil {
		return ErrRecoveryRequired
	}
	switch latest.Info.Status {
	case release.StatusDeployed, release.StatusFailed, release.StatusUninstalling, release.StatusUninstalled:
		return nil
	default:
		return ErrRecoveryRequired
	}
}

func inspectArchive(data []byte) (result error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid chart archive")
	}
	defer func() {
		if err := gz.Close(); result == nil && err != nil {
			result = errors.New("invalid chart archive")
		}
	}()
	tr := tar.NewReader(gz)
	var expanded int64
	entries := 0
	seenPaths := make(map[string]struct{})
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("invalid chart archive")
		}
		entries++
		if entries > maxArchiveEntries {
			return errors.New("chart archive has too many entries")
		}
		components, err := validateArchiveHeader(h, seenPaths)
		if err != nil {
			return err
		}
		if h.Size < 0 || h.Size > maxExpandedBytes-expanded {
			return errors.New("chart archive exceeds expanded size limit")
		}
		expanded += h.Size
		if err := consumeArchiveEntry(tr, h, components); err != nil {
			return err
		}
	}
	return nil
}

func validateArchiveHeader(h *tar.Header, seen map[string]struct{}) ([]string, error) {
	normalized := strings.ReplaceAll(h.Name, "\\", "/")
	if h.Typeflag == tar.TypeDir {
		normalized = strings.TrimSuffix(normalized, "/")
	}
	clean := path.Clean(normalized)
	if h.Name == "" || normalized == "" || strings.HasPrefix(clean, "/") || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != normalized {
		return nil, errors.New("chart archive contains an unsafe path")
	}
	if _, exists := seen[clean]; exists {
		return nil, errors.New("chart archive contains duplicate paths")
	}
	seen[clean] = struct{}{}
	components := strings.Split(clean, "/")
	for _, component := range components {
		if component == "charts" || component == "crds" {
			return nil, errors.New("chart archive contains unsupported dependencies or crds")
		}
	}
	if path.Base(clean) == "values.schema.json" {
		return nil, errors.New("chart archive contains an unsupported values schema")
	}
	if h.Typeflag == tar.TypeSymlink || h.Typeflag == tar.TypeLink {
		return nil, errors.New("chart archive contains an unsupported link")
	}
	if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
		return nil, errors.New("chart archive contains an unsupported entry")
	}
	return components, nil
}

func consumeArchiveEntry(tr *tar.Reader, h *tar.Header, components []string) error {
	if h.Size == 0 {
		return nil
	}
	var dst = io.Discard
	if h.Typeflag == tar.TypeReg && containsPathComponent(components, "templates") {
		dst = new(bytes.Buffer)
	}
	if _, err := io.CopyN(dst, tr, h.Size); err != nil {
		return errors.New("invalid chart archive")
	}
	if content, ok := dst.(*bytes.Buffer); ok && bytes.Contains(content.Bytes(), []byte("helm.sh/hook")) {
		return errors.New("chart archive contains unsupported helm hooks")
	}
	return nil
}

func containsPathComponent(components []string, want string) bool {
	return slices.Contains(components, want)
}
