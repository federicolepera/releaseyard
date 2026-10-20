package main

import (
	"log"
	"net/http"
	"os"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	"github.com/federicolepera/idp/internal/dashboard"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("configure in-cluster Kubernetes client: %v", err)
	}
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		log.Fatalf("register platform API types: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		log.Fatalf("register core Kubernetes types: %v", err)
	}
	if err := authorizationv1.AddToScheme(scheme); err != nil {
		log.Fatalf("register authorization API types: %v", err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		log.Fatalf("register rbac API types: %v", err)
	}
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		log.Fatalf("register apiextensions API types: %v", err)
	}
	k8sClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("create Kubernetes client: %v", err)
	}

	// The dashboard impersonates the logged-in user so that the management
	// cluster's RBAC authorizes writes.
	clientFactory := func(userName string, groups []string) (client.Client, error) {
		impersonating := rest.CopyConfig(config)
		impersonating.Impersonate = rest.ImpersonationConfig{UserName: userName, Groups: groups}
		return client.New(impersonating, client.Options{Scheme: scheme})
	}

	handler, err := dashboard.NewLocalHandler(k8sClient, "idp-system", dashboard.LocalAuthConfig{
		HashPath:                   os.Getenv("LOCAL_ADMIN_HASH_PATH"),
		PublicURL:                  os.Getenv("DASHBOARD_PUBLIC_URL"),
		IdentityProviderSecretName: os.Getenv("IDENTITY_PROVIDER_SECRET_NAME"),
		TeamNamespacePrefix:        envOr("TEAM_NAMESPACE_PREFIX", "releaseyard-team-"),
		ApplicationEditorRole:      envOr("APPLICATION_EDITOR_ROLE", "releaseyard-application-editor-role"),
		ClientFactory:              clientFactory,
	})
	if err != nil {
		log.Fatalf("configure dashboard local authentication: %v", err)
	}
	log.Printf("dashboard listening on :8080")
	if err := http.ListenAndServe(":8080", handler); err != nil {
		log.Fatalf("dashboard server stopped: %v", err)
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
