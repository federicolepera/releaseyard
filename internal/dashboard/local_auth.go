package dashboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"

	"golang.org/x/crypto/bcrypt"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	localAdminHashKey            = "password-hash"
	localAdminInitialPasswordKey = "initial-password"
	localAdminBcryptCost         = 12
)

func verifyLocalAdminPassword(storedHash []byte, password string) bool {
	if len(storedHash) == 0 || password == "" {
		return false
	}
	err := bcrypt.CompareHashAndPassword(storedHash, []byte(password))
	return err == nil
}

func generateLocalAdminCredentials() (string, []byte, error) {
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", nil, err
	}
	password := base64.RawURLEncoding.EncodeToString(randomBytes)
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), localAdminBcryptCost)
	if err != nil {
		return "", nil, err
	}
	return password, hashedPassword, nil
}

func readLocalAdminHash(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("local admin hash path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	hash := bytes.TrimSpace(data)
	cost, err := bcrypt.Cost(hash)
	if err != nil || cost < localAdminBcryptCost {
		return nil, errors.New("local admin hash is invalid or has insufficient cost")
	}
	return hash, nil
}

func newLocalAdminSecret(namespace, name, password string, hash []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			localAdminHashKey:            hash,
			localAdminInitialPasswordKey: []byte(password),
		},
		Type: corev1.SecretTypeOpaque,
	}
}
func EnsureLocalAdminSecret(ctx context.Context, k8s client.Client, namespace, name string) error {
	key := types.NamespacedName{Namespace: namespace, Name: name}
	existing := &corev1.Secret{}

	err := k8s.Get(ctx, key, existing)
	if err == nil {
		return validateLocalAdminSecret(existing)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read local admin Secret: %w", err)
	}

	password, hash, err := generateLocalAdminCredentials()
	if err != nil {
		return fmt.Errorf("generate local admin credentials: %w", err)
	}
	if err := k8s.Create(ctx, newLocalAdminSecret(namespace, name, password, hash)); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create local admin Secret: %w", err)
	}

	// Un altro bootstrap lo ha creato nel frattempo: non sovrascriverlo.
	if err := k8s.Get(ctx, key, existing); err != nil {
		return fmt.Errorf("read concurrently created local admin Secret: %w", err)
	}
	return validateLocalAdminSecret(existing)
}

func validateLocalAdminSecret(secret *corev1.Secret) error {
	if secret.Type != corev1.SecretTypeOpaque {
		return errors.New("invalid local admin Secret type")
	}
	hash := secret.Data[localAdminHashKey]
	cost, err := bcrypt.Cost(hash)
	if err != nil || cost < localAdminBcryptCost {
		return errors.New("invalid local admin password hash")
	}
	if initial, exists := secret.Data[localAdminInitialPasswordKey]; exists {
		if len(initial) == 0 || bcrypt.CompareHashAndPassword(hash, initial) != nil {
			return errors.New("initial local admin password does not match its hash")
		}
	}
	return nil
}
