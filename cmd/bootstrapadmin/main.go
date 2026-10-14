package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/federicolepera/idp/internal/dashboard"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		log.Fatal("POD_NAMESPACE is required")
	}
	secretName := os.Getenv("LOCAL_ADMIN_SECRET_NAME")
	if secretName == "" {
		log.Fatal("LOCAL_ADMIN_SECRET_NAME is required")
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("configure in-cluster Kubernetes client: %v", err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		log.Fatalf("register core Kubernetes types: %v", err)
	}
	k8sClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("create Kubernetes client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := dashboard.EnsureLocalAdminSecret(ctx, k8sClient, namespace, secretName); err != nil {
		log.Fatalf("bootstrap local admin Secret failed")
	}
	log.Printf("Local admin Secret bootstrap completed")
}
