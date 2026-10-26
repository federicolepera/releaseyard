/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("Application access admission", func() {
	const (
		testUser          = "vap-test-user"
		allowedDefinition = "allowed-definition"
		allowedCluster    = "allowed-cluster"
		deniedDefinition  = "denied-definition"
		deniedCluster     = "denied-cluster"
	)

	var (
		namespace  string
		userClient client.Client
	)

	BeforeEach(func() {
		namespace = fmt.Sprintf("vap-test-%d", time.Now().UnixNano())

		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())

		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "application-creator", Namespace: namespace},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{"platform.releaseyard.io"},
				Resources: []string{"applications"},
				Verbs:     []string{"create"},
			}},
		}
		Expect(k8sClient.Create(ctx, role)).To(Succeed())

		roleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "application-creator", Namespace: namespace},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "Role",
				Name:     role.Name,
			},
			Subjects: []rbacv1.Subject{{
				APIGroup: rbacv1.GroupName,
				Kind:     "User",
				Name:     testUser,
			}},
		}
		Expect(k8sClient.Create(ctx, roleBinding)).To(Succeed())

		accessName := "vap-access-" + namespace
		clusterRole := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: accessName},
			Rules: []rbacv1.PolicyRule{
				{
					APIGroups:     []string{"platform.releaseyard.io"},
					Resources:     []string{"applicationdefinitions"},
					ResourceNames: []string{allowedDefinition},
					Verbs:         []string{"get"},
				},
				{
					APIGroups:     []string{"platform.releaseyard.io"},
					Resources:     []string{"clusters"},
					ResourceNames: []string{allowedCluster},
					Verbs:         []string{"get"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, clusterRole)).To(Succeed())

		clusterRoleBinding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: accessName},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "ClusterRole",
				Name:     clusterRole.Name,
			},
			Subjects: []rbacv1.Subject{{
				APIGroup: rbacv1.GroupName,
				Kind:     "User",
				Name:     testUser,
			}},
		}
		Expect(k8sClient.Create(ctx, clusterRoleBinding)).To(Succeed())

		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, clusterRoleBinding)).To(Succeed())
			Expect(k8sClient.Delete(ctx, clusterRole)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: namespace},
			})).To(Succeed())
		})

		userConfig := rest.CopyConfig(cfg)
		userConfig.Impersonate = rest.ImpersonationConfig{UserName: testUser}
		var err error
		userClient, err = client.New(userConfig, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
	})

	It("allows an Application whose definition and cluster are readable", func() {
		Expect(userClient.Create(ctx, testApplication(namespace, "allowed-app", allowedDefinition, allowedCluster))).To(Succeed())
	})

	It("rejects an Application whose definition is not readable", func() {
		err := userClient.Create(ctx, testApplication(namespace, "denied-definition-app", deniedDefinition, allowedCluster))
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "expected Forbidden, got %v", err)
		Expect(err.Error()).To(ContainSubstring("ApplicationDefinition"))
	})

	It("rejects an Application whose cluster is not readable", func() {
		err := userClient.Create(ctx, testApplication(namespace, "denied-cluster-app", allowedDefinition, deniedCluster))
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "expected Forbidden, got %v", err)
		Expect(err.Error()).To(ContainSubstring("Cluster"))
	})

	It("rejects an environment Application when any target cluster is not readable", func() {
		app := testApplication(namespace, "denied-environment-app", allowedDefinition, allowedCluster)
		spec := app.Object["spec"].(map[string]any)
		delete(spec, "clusterRef")
		spec["environment"] = map[string]any{
			"name": "production",
			"targets": []any{
				map[string]any{"name": allowedCluster, "uid": "allowed-uid"},
				map[string]any{"name": deniedCluster, "uid": "denied-uid"},
			},
		}

		err := userClient.Create(ctx, app)
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "expected Forbidden, got %v", err)
		Expect(err.Error()).To(ContainSubstring("environment target"))
	})

	It("allows an Application whose selected clusters are readable", func() {
		app := testApplication(namespace, "allowed-clusters-app", allowedDefinition, allowedCluster)
		spec := app.Object["spec"].(map[string]any)
		delete(spec, "clusterRef")
		spec["clusters"] = []any{map[string]any{"name": allowedCluster, "uid": "allowed-uid"}}

		Expect(userClient.Create(ctx, app)).To(Succeed())
	})

	It("rejects an Application when any selected cluster is not readable", func() {
		app := testApplication(namespace, "denied-clusters-app", allowedDefinition, allowedCluster)
		spec := app.Object["spec"].(map[string]any)
		delete(spec, "clusterRef")
		spec["clusters"] = []any{
			map[string]any{"name": allowedCluster, "uid": "allowed-uid"},
			map[string]any{"name": deniedCluster, "uid": "denied-uid"},
		}

		err := userClient.Create(ctx, app)
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "expected Forbidden, got %v", err)
		Expect(err.Error()).To(ContainSubstring("selected Cluster"))
	})

	It("allows a dynamic environment Application without targets", func() {
		app := testApplication(namespace, "dynamic-environment-app", allowedDefinition, allowedCluster)
		spec := app.Object["spec"].(map[string]any)
		delete(spec, "clusterRef")
		spec["environment"] = map[string]any{"name": "production"}

		Expect(userClient.Create(ctx, app)).To(Succeed())
	})

	It("allows an environment Application whose target clusters are readable", func() {
		app := testApplication(namespace, "allowed-environment-app", allowedDefinition, allowedCluster)
		spec := app.Object["spec"].(map[string]any)
		delete(spec, "clusterRef")
		spec["environment"] = map[string]any{
			"name": "production",
			"targets": []any{
				map[string]any{"name": allowedCluster, "uid": "allowed-uid"},
			},
		}

		Expect(userClient.Create(ctx, app)).To(Succeed())
	})
})

func testApplication(namespace, name, definition, cluster string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "platform.releaseyard.io/v1alpha1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{
			"applicationDefinitionRef": map[string]any{"name": definition},
			"clusterRef":               map[string]any{"name": cluster},
		},
	}}
}
