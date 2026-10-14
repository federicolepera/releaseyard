package releaseclaim

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const demoReleaseName = "demo"

func TestAcquireCreatesAndRecoversSameClaim(t *testing.T) {
	ctx := context.Background()
	identity := testIdentity()
	client := newFakeClient()

	created, err := Acquire(ctx, client, identity)
	if err != nil {
		t.Fatalf("Acquire create: %v", err)
	}
	if created.UID != types.UID("claim-uid") {
		t.Fatalf("created UID = %q, want claim-uid", created.UID)
	}
	if created.Name != ClaimName(identity.ReleaseName) || created.Namespace != identity.ReleaseNamespace {
		t.Fatalf("unexpected claim location: %#v", created)
	}
	if len(created.Name) > 63 || created.Name != "ry-claim-demo" {
		t.Fatalf("claim name = %q, want reserved prefixed name within 63 characters", created.Name)
	}
	recovered, err := Acquire(ctx, client, identity)
	if err != nil {
		t.Fatalf("Acquire recovery: %v", err)
	}
	if recovered.UID != created.UID {
		t.Fatalf("recovered UID = %q, want original %q", recovered.UID, created.UID)
	}
	if err := Verify(ctx, client, identity, recovered); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestAcquireRejectsMismatchedIdentity(t *testing.T) {
	ctx := context.Background()
	identity := testIdentity()
	client := newFakeClient()
	if _, err := Acquire(ctx, client, identity); err != nil {
		t.Fatalf("initial Acquire: %v", err)
	}
	contender := identity
	contender.ManagementNamespace = "other-management"
	if _, err := Acquire(ctx, client, contender); !errors.Is(err, ErrOwnershipConflict) {
		t.Fatalf("Acquire with mismatched identity = %v, want ownership conflict", err)
	}
	stored, err := client.CoreV1().ConfigMaps(identity.ReleaseNamespace).Get(ctx, ClaimName(identity.ReleaseName), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get original claim: %v", err)
	}
	if stored.Data[dataManagementNamespace] != identity.ManagementNamespace {
		t.Fatalf("existing claim was overwritten: %q", stored.Data[dataManagementNamespace])
	}
}

func TestAcquireRequiresHTTPSAPIServer(t *testing.T) {
	identity := testIdentity()
	identity.TargetAPIServer = "http://api.example.test:6443"
	if _, err := Acquire(context.Background(), newFakeClient(), identity); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("Acquire with HTTP target = %v, want invalid identity", err)
	}
}

func TestClaimNameMaximumReleaseNameAndChartConfigMapCollision(t *testing.T) {
	longReleaseName := "a1234567890123456789012345678901234567890123456789012"
	if len(longReleaseName) != 53 {
		t.Fatalf("test release name length = %d, want 53", len(longReleaseName))
	}
	if got := ClaimName(longReleaseName); len(got) > 63 || got != "ry-claim-"+longReleaseName {
		t.Fatalf("ClaimName(%q) = %q, want deterministic name of at most 63 characters", longReleaseName, got)
	}

	ctx := context.Background()
	identity := testIdentity()
	chartConfigMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: identity.ReleaseNamespace,
		Name:      identity.ReleaseName,
	}, Data: map[string]string{"chart-data": "preserved"}}
	client := newFakeClient(chartConfigMap)
	claim, err := Acquire(ctx, client, identity)
	if err != nil {
		t.Fatalf("Acquire alongside chart ConfigMap: %v", err)
	}
	if claim.Name != ClaimName(identity.ReleaseName) {
		t.Fatalf("claim name = %q, want %q", claim.Name, ClaimName(identity.ReleaseName))
	}
	stored, err := client.CoreV1().ConfigMaps(identity.ReleaseNamespace).Get(ctx, identity.ReleaseName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get chart ConfigMap: %v", err)
	}
	if stored.Data["chart-data"] != "preserved" {
		t.Fatalf("chart ConfigMap was changed: %#v", stored.Data)
	}
}

func TestVerifyDetectsClaimReplacement(t *testing.T) {
	ctx := context.Background()
	identity := testIdentity()
	client := newFakeClient()
	claim, err := Acquire(ctx, client, identity)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	replacement, err := client.CoreV1().ConfigMaps(claim.Namespace).Get(ctx, claim.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get claim: %v", err)
	}
	replacement.UID = "replacement-uid"
	if _, err = client.CoreV1().ConfigMaps(claim.Namespace).Update(ctx, replacement, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update replacement: %v", err)
	}
	if err := Verify(ctx, client, identity, claim); !errors.Is(err, ErrOwnershipConflict) {
		t.Fatalf("Verify replaced claim = %v, want ownership conflict", err)
	}
}

func TestDeleteUsesUIDAndResourceVersionPreconditions(t *testing.T) {
	ctx := context.Background()
	identity := testIdentity()
	client := newFakeClient()
	claim, err := Acquire(ctx, client, identity)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	var observed *metav1.DeleteOptions
	client.PrependReactor("delete", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		deleteAction, ok := action.(clienttesting.DeleteActionImpl)
		if !ok {
			t.Fatalf("delete action has unexpected type %T", action)
		}
		options := deleteAction.GetDeleteOptions()
		observed = &options
		return false, nil, nil
	})
	if err := Delete(ctx, client, identity, claim); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if observed == nil || observed.Preconditions == nil || observed.Preconditions.UID == nil ||
		*observed.Preconditions.UID != claim.UID || observed.Preconditions.ResourceVersion == nil ||
		*observed.Preconditions.ResourceVersion != claim.ResourceVersion {
		t.Fatalf("Delete preconditions = %#v, want UID %q and resourceVersion %q", observed, claim.UID, claim.ResourceVersion)
	}
	_, err = client.CoreV1().ConfigMaps(claim.Namespace).Get(ctx, claim.Name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("claim after Delete error = %v, want NotFound", err)
	}
}

func TestAcquireRejectsEmptyCreatedUID(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "target", Name: demoReleaseName}}, nil
	})
	if _, err := Acquire(context.Background(), client, testIdentity()); !errors.Is(err, ErrEmptyClaimUID) {
		t.Fatalf("Acquire with empty UID = %v, want empty UID error", err)
	}
}

func newFakeClient(objects ...runtime.Object) *fake.Clientset {
	client := fake.NewSimpleClientset(objects...)
	client.PrependReactor("create", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		object := action.(clienttesting.CreateAction).GetObject().(*corev1.ConfigMap)
		object.UID = "claim-uid"
		object.ResourceVersion = "7"
		return false, nil, nil
	})
	return client
}

func testIdentity() Identity {
	return Identity{
		ApplicationUID:         "application-uid",
		ManagementNamespace:    "management",
		ManagementName:         demoReleaseName,
		ClusterUID:             "cluster-uid",
		TargetAPIServer:        "https://api.example.test:6443",
		TargetTrustFingerprint: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ReleaseNamespace:       "workloads",
		ReleaseName:            demoReleaseName,
	}
}
