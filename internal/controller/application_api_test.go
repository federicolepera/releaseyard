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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const (
	apiTestNameKey               = "name"
	apiTestNamespaceKey          = "namespace"
	apiTestResourcesKey          = "resources"
	apiTestCPUKey                = "cpu"
	apiTestLimitsKey             = "limits"
	apiTestApplicationsName      = "applications"
	apiTestChartRepository       = "https://charts.example.test/platform"
	apiTestRepositoryKey         = "repository"
	apiTestVersionKey            = "version"
	apiTestObservedGenerationKey = applicationObservedGenerationField
	apiTestReasonKey             = applicationReasonField
	apiTestSpecKey               = applicationSpecField
)

var _ = Describe("Application API", func() {
	var managementNamespace string

	BeforeEach(func() {
		managementNamespace = fmt.Sprintf("application-%d", time.Now().UnixNano())
		namespace := &unstructured.Unstructured{}
		namespace.SetAPIVersion("v1")
		namespace.SetKind("Namespace")
		namespace.SetName(managementNamespace)
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
	})

	AfterEach(func() {
		namespace := &unstructured.Unstructured{}
		namespace.SetAPIVersion("v1")
		namespace.SetKind("Namespace")
		namespace.SetName(managementNamespace)
		Expect(k8sClient.Delete(ctx, namespace)).To(Succeed())
	})

	It("creates and reads an application while preserving nested values", func() {
		application := validApplication(managementNamespace)
		Expect(k8sClient.Create(ctx, application)).To(Succeed())

		readBack := &unstructured.Unstructured{}
		readBack.SetGroupVersionKind(applicationGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: managementNamespace, Name: application.GetName()}, readBack)).To(Succeed())
		Expect(readBack.Object["spec"]).To(Equal(application.Object["spec"]))
	})

	It("creates and reads an environment application with its sorted target snapshot", func() {
		application := validEnvironmentApplication(managementNamespace)
		Expect(k8sClient.Create(ctx, application)).To(Succeed())

		readBack := &unstructured.Unstructured{}
		readBack.SetGroupVersionKind(applicationGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: managementNamespace, Name: application.GetName()}, readBack)).To(Succeed())
		Expect(readBack.Object["spec"].(map[string]any)["environment"]).To(Equal(application.Object["spec"].(map[string]any)["environment"]))
	})

	It("allows environment application values-only updates", func() {
		application := validEnvironmentApplication(managementNamespace)
		Expect(k8sClient.Create(ctx, application)).To(Succeed())

		application.Object["spec"].(map[string]any)["values"] = map[string]any{
			applicationDefinitionReplicasPath: int64(4),
			apiTestResourcesKey:               map[string]any{apiTestLimitsKey: map[string]any{apiTestCPUKey: "750m"}},
		}
		Expect(k8sClient.Update(ctx, application)).To(Succeed())
	})

	It("accepts an Application name exactly 53 characters long", func() {
		application := validApplication(managementNamespace)
		application.SetName(strings.Repeat("a", 53))
		Expect(k8sClient.Create(ctx, application)).To(Succeed())
	})

	DescribeTable("rejects invalid application fields", func(mutate func(*unstructured.Unstructured)) {
		application := validApplication(managementNamespace)
		mutate(application)
		err := k8sClient.Create(ctx, application)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected API schema validation error, got %v", err)
	},
		Entry("missing application definition reference", func(application *unstructured.Unstructured) {
			delete(application.Object["spec"].(map[string]any), "applicationDefinitionRef")
		}),
		Entry("missing cluster reference", func(application *unstructured.Unstructured) {
			delete(application.Object["spec"].(map[string]any), "clusterRef")
		}),
		Entry("both cluster and environment selectors", func(application *unstructured.Unstructured) {
			application.Object["spec"].(map[string]any)["environment"] = validEnvironmentSelection()
		}),
		Entry("empty environment name", func(application *unstructured.Unstructured) {
			delete(application.Object["spec"].(map[string]any), "clusterRef")
			application.Object["spec"].(map[string]any)["environment"] = map[string]any{"name": "", "targets": validEnvironmentTargets()}
		}),
		Entry("invalid environment name", func(application *unstructured.Unstructured) {
			delete(application.Object["spec"].(map[string]any), "clusterRef")
			application.Object["spec"].(map[string]any)["environment"] = map[string]any{"name": "prod/blue", "targets": validEnvironmentTargets()}
		}),
		Entry("empty environment target list", func(application *unstructured.Unstructured) {
			delete(application.Object["spec"].(map[string]any), "clusterRef")
			application.Object["spec"].(map[string]any)["environment"] = map[string]any{"name": "production", "targets": []any{}}
		}),
		Entry("duplicate environment target names", func(application *unstructured.Unstructured) {
			delete(application.Object["spec"].(map[string]any), "clusterRef")
			application.Object["spec"].(map[string]any)["environment"] = map[string]any{"name": "production", "targets": []any{
				map[string]any{"name": "cluster-a", "uid": "cluster-uid-a"},
				map[string]any{"name": "cluster-a", "uid": "cluster-uid-b"},
			}}
		}),
		Entry("empty environment target UID", func(application *unstructured.Unstructured) {
			delete(application.Object["spec"].(map[string]any), "clusterRef")
			application.Object["spec"].(map[string]any)["environment"] = map[string]any{"name": "production", "targets": []any{
				map[string]any{"name": "cluster-a", "uid": "cluster-uid-a"},
				map[string]any{"name": "cluster-b", "uid": ""},
			}}
		}),
		Entry("invalid application definition reference name", func(application *unstructured.Unstructured) {
			application.Object["spec"].(map[string]any)["applicationDefinitionRef"].(map[string]any)[apiTestNameKey] = "Not_A_Name"
		}),
		Entry("invalid cluster reference name", func(application *unstructured.Unstructured) {
			application.Object["spec"].(map[string]any)["clusterRef"].(map[string]any)[apiTestNameKey] = "Not_A_Name"
		}),
		Entry("non-object values", func(application *unstructured.Unstructured) {
			application.Object["spec"].(map[string]any)["values"] = "not-an-object"
		}),
		Entry("metadata name longer than 53 characters", func(application *unstructured.Unstructured) {
			application.SetName("application-name-longer-than-fifty-three-characters-12")
		}),
		Entry("dotted metadata name", func(application *unstructured.Unstructured) {
			application.SetName("web.frontend")
		}),
	)

	It("rejects changing applicationDefinitionRef.name after creation", func() {
		application := validApplication(managementNamespace)
		Expect(k8sClient.Create(ctx, application)).To(Succeed())

		application.Object["spec"].(map[string]any)["applicationDefinitionRef"].(map[string]any)[apiTestNameKey] = "another-definition"
		err := k8sClient.Update(ctx, application)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected immutability validation error, got %v", err)
	})

	It("rejects changing clusterRef.name after creation", func() {
		application := validApplication(managementNamespace)
		Expect(k8sClient.Create(ctx, application)).To(Succeed())

		application.Object["spec"].(map[string]any)["clusterRef"].(map[string]any)[apiTestNameKey] = "another-cluster"
		err := k8sClient.Update(ctx, application)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected immutability validation error, got %v", err)
	})

	DescribeTable("rejects environment selector changes after creation", func(mutate func(*unstructured.Unstructured)) {
		application := validEnvironmentApplication(managementNamespace)
		Expect(k8sClient.Create(ctx, application)).To(Succeed())

		mutate(application)
		err := k8sClient.Update(ctx, application)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected selector immutability validation error, got %v", err)
	},
		Entry("environment to legacy selector", func(application *unstructured.Unstructured) {
			spec := application.Object["spec"].(map[string]any)
			delete(spec, "environment")
			spec["clusterRef"] = map[string]any{apiTestNameKey: "production"}
		}),
		Entry("add environment target", func(application *unstructured.Unstructured) {
			targets := application.Object["spec"].(map[string]any)["environment"].(map[string]any)["targets"].([]any)
			targets = append(targets, map[string]any{"name": "cluster-c", "uid": "cluster-uid-c"})
			application.Object["spec"].(map[string]any)["environment"].(map[string]any)["targets"] = targets
		}),
		Entry("remove environment target", func(application *unstructured.Unstructured) {
			application.Object["spec"].(map[string]any)["environment"].(map[string]any)["targets"] = validEnvironmentTargets()[:1]
		}),
		Entry("replace environment target UID", func(application *unstructured.Unstructured) {
			application.Object["spec"].(map[string]any)["environment"].(map[string]any)["targets"].([]any)[0].(map[string]any)["uid"] = "replacement-uid"
		}),
		Entry("change environment name", func(application *unstructured.Unstructured) {
			application.Object["spec"].(map[string]any)["environment"].(map[string]any)["name"] = "staging"
		}),
	)

	It("accepts a target-order-only environment update but rejects membership changes", func() {
		application := validEnvironmentApplication(managementNamespace)
		Expect(k8sClient.Create(ctx, application)).To(Succeed())

		spec := application.Object["spec"].(map[string]any)
		environment := spec["environment"].(map[string]any)
		targets := environment["targets"].([]any)
		environment["targets"] = []any{targets[1], targets[0]}
		spec["values"] = map[string]any{applicationDefinitionReplicasPath: int64(3)}
		Expect(k8sClient.Update(ctx, application)).To(Succeed())

		readBack := &unstructured.Unstructured{}
		readBack.SetGroupVersionKind(applicationGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: managementNamespace, Name: application.GetName()}, readBack)).To(Succeed())
		readEnvironment := readBack.Object["spec"].(map[string]any)["environment"].(map[string]any)
		Expect(readEnvironment["targets"]).To(HaveLen(2))
		observed := readEnvironment["targets"].([]any)
		members := map[string]string{}
		for _, item := range observed {
			target := item.(map[string]any)
			members[target["name"].(string)] = target["uid"].(string)
		}
		Expect(members).To(Equal(map[string]string{"cluster-a": "cluster-uid-a", "cluster-b": "cluster-uid-b"}))

		readEnvironment["targets"].([]any)[0].(map[string]any)["uid"] = "replacement-uid"
		err := k8sClient.Update(ctx, readBack)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected target UID immutability validation error, got %v", err)

		readBack = &unstructured.Unstructured{}
		readBack.SetGroupVersionKind(applicationGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: managementNamespace, Name: application.GetName()}, readBack)).To(Succeed())
		readTargets := readBack.Object["spec"].(map[string]any)["environment"].(map[string]any)["targets"].([]any)
		readBack.Object["spec"].(map[string]any)["environment"].(map[string]any)["targets"] = append(readTargets, map[string]any{"name": "cluster-c", "uid": "cluster-uid-c"})
		err = k8sClient.Update(ctx, readBack)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected target membership immutability validation error, got %v", err)
	})

	It("rejects changing a legacy selector to an environment selector", func() {
		application := validApplication(managementNamespace)
		Expect(k8sClient.Create(ctx, application)).To(Succeed())

		spec := application.Object["spec"].(map[string]any)
		delete(spec, "clusterRef")
		spec["environment"] = validEnvironmentSelection()
		err := k8sClient.Update(ctx, application)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected selector immutability validation error, got %v", err)
	})

	It("makes releaseNamespace immutable across presence and value transitions", func() {
		application := validApplication(managementNamespace)
		Expect(k8sClient.Create(ctx, application)).To(Succeed())

		application.Object["spec"].(map[string]any)["releaseNamespace"] = apiTestApplicationsName
		err := k8sClient.Update(ctx, application)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected absent-to-present immutability rejection, got %v", err)

		application = validApplication(managementNamespace)
		application.SetName("web-frontend-with-namespace")
		application.Object["spec"].(map[string]any)["releaseNamespace"] = apiTestApplicationsName
		Expect(k8sClient.Create(ctx, application)).To(Succeed())

		application.Object["spec"].(map[string]any)["releaseNamespace"] = "team-workloads"
		err = k8sClient.Update(ctx, application)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected changed value immutability rejection, got %v", err)

		application.Object["spec"].(map[string]any)["releaseNamespace"] = nil
		err = k8sClient.Update(ctx, application)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected present-to-absent immutability rejection, got %v", err)
	})

	It("allows values updates", func() {
		application := validApplication(managementNamespace)
		Expect(k8sClient.Create(ctx, application)).To(Succeed())

		application.Object["spec"].(map[string]any)["values"] = map[string]any{
			applicationDefinitionReplicasPath: int64(4),
			apiTestResourcesKey:               map[string]any{apiTestLimitsKey: map[string]any{apiTestCPUKey: "750m"}},
		}
		Expect(k8sClient.Update(ctx, application)).To(Succeed())
	})

	It("persists binding, immutable chart pins, attempted inputs, and last applied identity through status", func() {
		application := validApplication(managementNamespace)
		Expect(k8sClient.Create(ctx, application)).To(Succeed())

		chartA := map[string]any{
			apiTestRepositoryKey: apiTestChartRepository,
			apiTestNameKey:       testApplicationDefinitionName,
			apiTestVersionKey:    "1.4.0",
			"digest":             "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}
		chartB := map[string]any{
			apiTestRepositoryKey: apiTestChartRepository,
			apiTestNameKey:       testApplicationDefinitionName,
			apiTestVersionKey:    "1.5.0",
			"digest":             "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		}
		persistStatus := func() {
			Expect(k8sClient.Status().Update(ctx, application)).To(Succeed())
			readBack := &unstructured.Unstructured{}
			readBack.SetGroupVersionKind(applicationGVK)
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: managementNamespace, Name: application.GetName()}, readBack)).To(Succeed())
			application = readBack
		}

		application.Object["status"] = map[string]any{
			apiTestObservedGenerationKey: application.GetGeneration(),
			"binding": map[string]any{
				"applicationDefinitionUID": "definition-uid",
				"clusterUID":               "cluster-uid",
				"targetAPIServer":          "https://cluster.example.test",
				"targetTrustHash":          "trust-hash",
				"releaseNamespace":         apiTestApplicationsName,
				"releaseName":              application.GetName(),
			},
		}
		persistStatus()
		Expect(application.Object["status"].(map[string]any)).NotTo(HaveKey("applied"))
		Expect(application.Object["status"].(map[string]any)["binding"].(map[string]any)).NotTo(HaveKey("remoteClaimUID"))

		application.Object["status"].(map[string]any)["binding"].(map[string]any)["remoteClaimUID"] = "claim-uid"
		persistStatus()
		Expect(application.Object["status"].(map[string]any)).NotTo(HaveKey("applied"))

		status := application.Object["status"].(map[string]any)
		status["chartPins"] = []any{chartA}
		persistStatus()
		Expect(application.Object["status"].(map[string]any)).NotTo(HaveKey("applied"))

		status = application.Object["status"].(map[string]any)
		status["applied"] = map[string]any{
			"applicationDefinitionGeneration": int64(2),
			"applicationGeneration":           application.GetGeneration(),
			"chart":                           chartA,
			"desiredInputIdentity":            "input-identity-a",
			"helmRevision":                    int64(1),
		}
		persistStatus()

		status = application.Object["status"].(map[string]any)
		status["chartPins"] = []any{chartA, chartB}
		persistStatus()
		Expect(application.Object["status"].(map[string]any)["chartPins"]).To(HaveLen(2))

		status = application.Object["status"].(map[string]any)
		status["lastAttemptedIdentity"] = "input-identity-b"
		status["conditions"] = []any{map[string]any{
			applicationDefinitionTypeField: applicationReadyConditionType,
			applicationStatusField:         applicationWorkloadFalse,
			apiTestObservedGenerationKey:   application.GetGeneration(),
			"lastTransitionTime":           time.Now().UTC().Format(time.RFC3339),
			apiTestReasonKey:               "ApplyFailed",
			"message":                      "Chart B action failed",
		}}
		persistStatus()

		finalStatus := application.Object["status"].(map[string]any)
		Expect(finalStatus["chartPins"]).To(Equal([]any{chartA, chartB}))
		Expect(finalStatus["lastAttemptedIdentity"]).To(Equal("input-identity-b"))
		Expect(finalStatus["applied"].(map[string]any)["chart"]).To(Equal(chartA))
	})

	It("persists environment binding and independent per-target status entries", func() {
		application := validEnvironmentApplication(managementNamespace)
		Expect(k8sClient.Create(ctx, application)).To(Succeed())

		application.Object["status"] = map[string]any{
			"environmentBinding": map[string]any{
				"applicationDefinitionUID": "definition-uid",
				"releaseNamespace":         apiTestApplicationsName,
			},
			"instances": []any{
				map[string]any{
					"clusterName":        "cluster-a",
					"clusterUID":         "cluster-uid-a",
					"observedGeneration": int64(1),
					"conditions": []any{map[string]any{
						"type":               "Ready",
						"status":             "True",
						"observedGeneration": int64(1),
						"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
						"reason":             "Applied",
						"message":            "Cluster A is ready",
					}},
					"resources": []any{map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": "web", "namespace": "workloads", "uid": "deployment-uid-a"}},
				},
				map[string]any{
					"clusterName":        "cluster-b",
					"clusterUID":         "cluster-uid-b",
					"observedGeneration": int64(1),
					"conditions": []any{map[string]any{
						"type":               "Ready",
						"status":             "False",
						"observedGeneration": int64(1),
						"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
						"reason":             "ApplyFailed",
						"message":            "Cluster B apply failed",
					}},
					"resources": []any{map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": "web", "namespace": "workloads", "uid": "deployment-uid-b"}},
				},
			},
		}
		Expect(k8sClient.Status().Update(ctx, application)).To(Succeed())

		readBack := &unstructured.Unstructured{}
		readBack.SetGroupVersionKind(applicationGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: managementNamespace, Name: application.GetName()}, readBack)).To(Succeed())
		status := readBack.Object["status"].(map[string]any)
		Expect(status["environmentBinding"].(map[string]any)["applicationDefinitionUID"]).To(Equal("definition-uid"))
		instances := status["instances"].([]any)
		Expect(instances).To(HaveLen(2))
		Expect(instances[0].(map[string]any)["clusterUID"]).To(Equal("cluster-uid-a"))
		Expect(instances[0].(map[string]any)["resources"]).To(HaveLen(1))
		Expect(instances[0].(map[string]any)["conditions"]).To(HaveLen(1))
		Expect(instances[1].(map[string]any)["clusterUID"]).To(Equal("cluster-uid-b"))
		Expect(instances[1].(map[string]any)["resources"]).To(HaveLen(1))
		Expect(instances[1].(map[string]any)["conditions"]).To(HaveLen(1))
	})
})

var applicationGVK = schema.GroupVersionKind{Group: "platform.releaseyard.io", Version: "v1alpha1", Kind: "Application"}

func validApplication(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": applicationGVK.Group + "/" + applicationGVK.Version,
		"kind":       applicationGVK.Kind,
		"metadata": map[string]any{
			apiTestNameKey:      "web-frontend",
			apiTestNamespaceKey: namespace,
		},
		apiTestSpecKey: map[string]any{
			"applicationDefinitionRef": map[string]any{apiTestNameKey: testApplicationDefinitionName},
			"clusterRef":               map[string]any{apiTestNameKey: "production"},
			"values": map[string]any{
				applicationDefinitionReplicasPath: int64(2),
				apiTestResourcesKey:               map[string]any{apiTestLimitsKey: map[string]any{apiTestCPUKey: "500m", "memory": "512Mi"}},
			},
		},
	}}
}

func validEnvironmentApplication(namespace string) *unstructured.Unstructured {
	application := validApplication(namespace)
	spec := application.Object["spec"].(map[string]any)
	delete(spec, "clusterRef")
	spec["environment"] = validEnvironmentSelection()
	return application
}

func validEnvironmentSelection() map[string]any {
	return map[string]any{"name": "production", "targets": validEnvironmentTargets()}
}

func validEnvironmentTargets() []any {
	return []any{
		map[string]any{"name": "cluster-a", "uid": "cluster-uid-a"},
		map[string]any{"name": "cluster-b", "uid": "cluster-uid-b"},
	}
}
