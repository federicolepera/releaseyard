// Package releaseclaim implements a small remote ownership claim for one Helm
// release. The ConfigMap is a collision sentinel, not a lock: callers must
// still verify the claim immediately before each Helm mutation.
package releaseclaim

import (
	"context"
	"errors"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

var (
	// ErrInvalidIdentity indicates required identity fields are missing or invalid.
	ErrInvalidIdentity = errors.New("invalid release claim identity")
	// ErrOwnershipConflict indicates a claim belongs to another identity or UID.
	ErrOwnershipConflict = errors.New("release claim ownership conflict")
	// ErrClaimNotFound indicates no claim exists when one was required.
	ErrClaimNotFound = errors.New("release claim not found")
	// ErrClaimUnavailable is a sanitized classification for remote API failures.
	ErrClaimUnavailable = errors.New("release claim unavailable")
	// ErrEmptyClaimUID indicates the API did not return a UID for a created claim.
	ErrEmptyClaimUID = errors.New("release claim has empty UID")
)

const (
	claimNamePrefix            = "ry-claim-"
	dataApplicationUID         = "application-uid"
	dataManagementNamespace    = "management-namespace"
	dataManagementName         = "management-name"
	dataClusterUID             = "cluster-uid"
	dataTargetAPIServer        = "target-api-server"
	dataTargetTrustFingerprint = "target-trust-fingerprint"
	dataReleaseNamespace       = "release-namespace"
	dataReleaseName            = "release-name"
)

// Identity binds the remote release claim to both the management-side objects
// and the exact target cluster and Helm release. Values stored by this package
// are identifiers only; credentials and trust material are never stored.
type Identity struct {
	ApplicationUID         types.UID
	ManagementNamespace    string
	ManagementName         string
	ClusterUID             types.UID
	TargetAPIServer        string
	TargetTrustFingerprint string
	ReleaseNamespace       string
	ReleaseName            string
}

// Claim is the minimum claim metadata needed to bind the caller's status to
// the remote object. A claim is not a mutex and does not establish Helm state.
type Claim struct {
	Namespace       string
	Name            string
	UID             types.UID
	ResourceVersion string
}

// Manager is the narrow controller-facing surface for remote claim lifecycle.
// Delete's documented precondition applies to every implementation: the Helm
// release must already have been observed absent by the caller.
type Manager interface {
	Acquire(ctx context.Context, identity Identity) (Claim, error)
	Verify(ctx context.Context, identity Identity, claim Claim) error
	Delete(ctx context.Context, identity Identity, claim Claim) error
}

// KubernetesManager implements Manager using the supplied target-cluster
// client. Inject a fake kubernetes.Interface in unit tests.
type KubernetesManager struct {
	Client kubernetes.Interface
}

// NewKubernetesManager binds the claim protocol to a target-cluster client.
func NewKubernetesManager(client kubernetes.Interface) KubernetesManager {
	return KubernetesManager{Client: client}
}

// Acquire implements Manager.
func (m KubernetesManager) Acquire(ctx context.Context, identity Identity) (Claim, error) {
	return Acquire(ctx, m.Client, identity)
}

// Verify implements Manager.
func (m KubernetesManager) Verify(ctx context.Context, identity Identity, claim Claim) error {
	return Verify(ctx, m.Client, identity, claim)
}

// Delete implements Manager. The caller must first establish that the Helm
// release is absent.
func (m KubernetesManager) Delete(ctx context.Context, identity Identity, claim Claim) error {
	return Delete(ctx, m.Client, identity, claim)
}

// ClaimName returns the deterministic, reserved ConfigMap name for a Helm
// release. Valid release names are at most 53 characters, so the prefixed name
// remains within Kubernetes' 63-character ConfigMap name limit.
func ClaimName(releaseName string) string {
	return claimNamePrefix + releaseName
}

// Acquire atomically creates the per-release ConfigMap. If it already exists,
// acquisition succeeds only when every identity field matches exactly. It
// never adopts, updates, or overwrites an existing claim.
func Acquire(ctx context.Context, client kubernetes.Interface, identity Identity) (Claim, error) {
	if client == nil || validate(identity) != nil {
		return Claim{}, ErrInvalidIdentity
	}
	name := ClaimName(identity.ReleaseName)
	object := configMap(identity, name)
	created, err := client.CoreV1().ConfigMaps(identity.ReleaseNamespace).Create(ctx, object, metav1.CreateOptions{})
	if err == nil {
		claim := claimFrom(created)
		if claim.UID == "" {
			return Claim{}, ErrEmptyClaimUID
		}
		return claim, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return Claim{}, ErrClaimUnavailable
	}
	current, getErr := client.CoreV1().ConfigMaps(identity.ReleaseNamespace).Get(ctx, name, metav1.GetOptions{})
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return Claim{}, ErrOwnershipConflict
		}
		return Claim{}, ErrClaimUnavailable
	}
	if !matches(current, identity) || current.UID == "" {
		return Claim{}, ErrOwnershipConflict
	}
	return claimFrom(current), nil
}

// Verify re-reads a claim and confirms its identity and exact Kubernetes UID.
// Call it before every Helm operation that mutates the release.
func Verify(ctx context.Context, client kubernetes.Interface, identity Identity, claim Claim) error {
	if client == nil || validate(identity) != nil || !validClaimReference(identity, claim) {
		return ErrInvalidIdentity
	}
	current, err := client.CoreV1().ConfigMaps(claim.Namespace).Get(ctx, claim.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ErrClaimNotFound
		}
		return ErrClaimUnavailable
	}
	if current.UID != claim.UID || !matches(current, identity) {
		return ErrOwnershipConflict
	}
	return nil
}

// Delete removes a verified claim using UID and resourceVersion preconditions.
// The caller MUST first establish that the corresponding Helm release is
// absent. This function never uninstalls a release or deletes a namespace.
func Delete(ctx context.Context, client kubernetes.Interface, identity Identity, claim Claim) error {
	if client == nil || validate(identity) != nil || !validClaimReference(identity, claim) {
		return ErrInvalidIdentity
	}
	current, err := client.CoreV1().ConfigMaps(claim.Namespace).Get(ctx, claim.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ErrClaimNotFound
		}
		return ErrClaimUnavailable
	}
	if current.UID != claim.UID || !matches(current, identity) {
		return ErrOwnershipConflict
	}
	uid, resourceVersion := current.UID, current.ResourceVersion
	err = client.CoreV1().ConfigMaps(claim.Namespace).Delete(ctx, claim.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion},
	})
	if err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return ErrOwnershipConflict
		}
		return ErrClaimUnavailable
	}
	return nil
}

func validate(i Identity) error {
	if i.ApplicationUID == "" || i.ClusterUID == "" || i.ManagementNamespace == "" ||
		i.ManagementName == "" || i.TargetAPIServer == "" || i.TargetTrustFingerprint == "" ||
		i.ReleaseNamespace == "" || i.ReleaseName == "" || len(i.ReleaseName) > 53 ||
		!validDNSLabel(i.ReleaseName) || !validDNSLabel(i.ReleaseNamespace) ||
		!validDNSLabel(i.ManagementNamespace) || !validDNSSubdomain(i.ManagementName) {
		return ErrInvalidIdentity
	}
	parsed, err := url.Parse(i.TargetAPIServer)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		parsed.Path != "" && parsed.Path != "/" || !validTrustFingerprint(i.TargetTrustFingerprint) {
		return ErrInvalidIdentity
	}
	return nil
}

func validTrustFingerprint(fingerprint string) bool {
	if len(fingerprint) != len("sha256:")+64 || !strings.HasPrefix(fingerprint, "sha256:") {
		return false
	}
	for _, char := range fingerprint[len("sha256:"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validDNSLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 || !isLowerAlphaNumeric(s[0]) {
		return false
	}
	if !isLowerAlphaNumeric(s[len(s)-1]) {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func isLowerAlphaNumeric(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= '0' && char <= '9'
}

func validDNSSubdomain(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for part := range strings.SplitSeq(s, ".") {
		if !validDNSLabel(part) {
			return false
		}
	}
	return true
}

func validClaimReference(i Identity, claim Claim) bool {
	return claim.Name == ClaimName(i.ReleaseName) && claim.Namespace == i.ReleaseNamespace && claim.UID != ""
}

func configMap(identity Identity, name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: identity.ReleaseNamespace, Name: name},
		Data: map[string]string{
			dataApplicationUID:         string(identity.ApplicationUID),
			dataManagementNamespace:    identity.ManagementNamespace,
			dataManagementName:         identity.ManagementName,
			dataClusterUID:             string(identity.ClusterUID),
			dataTargetAPIServer:        identity.TargetAPIServer,
			dataTargetTrustFingerprint: identity.TargetTrustFingerprint,
			dataReleaseNamespace:       identity.ReleaseNamespace,
			dataReleaseName:            identity.ReleaseName,
		},
	}
}

func matches(object *corev1.ConfigMap, identity Identity) bool {
	if object == nil || object.Namespace != identity.ReleaseNamespace || object.Name != ClaimName(identity.ReleaseName) {
		return false
	}
	want := configMap(identity, ClaimName(identity.ReleaseName)).Data
	if len(object.Data) != len(want) {
		return false
	}
	for key, value := range want {
		if object.Data[key] != value {
			return false
		}
	}
	return true
}

func claimFrom(object *corev1.ConfigMap) Claim {
	return Claim{Namespace: object.Namespace, Name: object.Name, UID: object.UID, ResourceVersion: object.ResourceVersion}
}
