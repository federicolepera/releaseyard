package dashboard

import (
	"bytes"
	"context"
	"testing"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGenerateLocalAdminCredentialsUsesRequiredCost(t *testing.T) {
	password, hash, err := generateLocalAdminCredentials()
	if err != nil {
		t.Fatal("generate local admin credentials failed")
	}
	cost, err := bcrypt.Cost(hash)
	if err != nil || cost < localAdminBcryptCost {
		t.Fatalf("generated password hash cost=%d, expected at least %d", cost, localAdminBcryptCost)
	}
	if !verifyLocalAdminPassword(hash, password) {
		t.Fatal("generated password did not verify against its hash")
	}
}

func TestEnsureLocalAdminSecretCreatesOnceAndPreservesCredentials(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal("register core types failed")
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()
	const namespace, name = "idp-system", "dashboard-local-admin"
	if err := EnsureLocalAdminSecret(ctx, k8s, namespace, name); err != nil {
		t.Fatal("create local admin Secret failed")
	}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	first := &corev1.Secret{}
	if err := k8s.Get(ctx, key, first); err != nil {
		t.Fatal("read generated local admin Secret failed")
	}
	firstHash := append([]byte(nil), first.Data[localAdminHashKey]...)
	firstPassword := append([]byte(nil), first.Data[localAdminInitialPasswordKey]...)
	if len(firstHash) == 0 || len(firstPassword) == 0 || !verifyLocalAdminPassword(firstHash, string(firstPassword)) {
		t.Fatal("generated Secret credentials were missing or inconsistent")
	}

	if err := EnsureLocalAdminSecret(ctx, k8s, namespace, name); err != nil {
		t.Fatal("repeat local admin Secret ensure failed")
	}
	second := &corev1.Secret{}
	if err := k8s.Get(ctx, key, second); err != nil {
		t.Fatal("read repeated local admin Secret failed")
	}
	if !bytes.Equal(firstHash, second.Data[localAdminHashKey]) || !bytes.Equal(firstPassword, second.Data[localAdminInitialPasswordKey]) {
		t.Fatal("repeat ensure changed existing local admin credentials")
	}
}

func TestEnsureLocalAdminSecretRejectsMalformedExistingSecretWithoutOverwrite(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal("register core types failed")
	}
	weakHash, err := bcrypt.GenerateFromPassword([]byte("test-only-password"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal("create test hash failed")
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dashboard-local-admin", Namespace: "idp-system"},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			localAdminHashKey:            weakHash,
			localAdminInitialPasswordKey: []byte("test-only-password"),
		},
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	ctx := context.Background()
	if err := EnsureLocalAdminSecret(ctx, k8s, "idp-system", "dashboard-local-admin"); err == nil {
		t.Fatal("expected malformed existing Secret to be rejected")
	}
	stored := &corev1.Secret{}
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: "idp-system", Name: "dashboard-local-admin"}, stored); err != nil {
		t.Fatal("read existing local admin Secret after rejection failed")
	}
	if !bytes.Equal(weakHash, stored.Data[localAdminHashKey]) || !bytes.Equal([]byte("test-only-password"), stored.Data[localAdminInitialPasswordKey]) {
		t.Fatal("rejected existing Secret was overwritten")
	}
}
