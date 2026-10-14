package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestRenderedCredentialsRBACMatchesManagerIdentity(t *testing.T) {
	output, err := exec.Command("../../bin/kustomize", "build", "../../config/default").Output()
	if err != nil {
		t.Fatalf("render Kustomize output: %v", err)
	}

	var deploymentNamespace, deploymentServiceAccount string
	var roleName, roleNamespace string
	var bindingRoleName, bindingRoleKind, bindingNamespace, subjectName, subjectNamespace string
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	for {
		var object map[string]any
		if err := decoder.Decode(&object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode rendered resource: %v", err)
		}
		if len(object) == 0 {
			break
		}
		metadata := objectMap(t, object, "metadata")
		name, _ := metadata["name"].(string)
		namespace, _ := metadata["namespace"].(string)
		switch object["kind"] {
		case applicationWorkloadDeploymentKind:
			if name == "idp-controller-manager" {
				deploymentNamespace = namespace
				podSpec := objectMap(t, objectMap(t, objectMap(t, object, "spec"), "template"), "spec")
				deploymentServiceAccount, _ = podSpec["serviceAccountName"].(string)
			}
		case "Role":
			if name == "idp-manager-credentials-reader" {
				roleName, roleNamespace = name, namespace
				rules, ok := object["rules"].([]any)
				if !ok || len(rules) != 1 {
					t.Fatalf("credentials Role rules = %#v, want one rule", object["rules"])
				}
				rule := rules[0].(map[string]any)
				resources := rule["resources"].([]any)
				verbs := rule["verbs"].([]any)
				if len(resources) != 1 || resources[0] != "secrets" || len(verbs) != 1 || verbs[0] != "get" {
					t.Fatalf("credentials Role grants unexpected access: %#v", rule)
				}
			}
		case "RoleBinding":
			if name == "idp-manager-credentials-reader" {
				bindingNamespace = namespace
				roleRef := objectMap(t, object, "roleRef")
				bindingRoleName, _ = roleRef["name"].(string)
				bindingRoleKind, _ = roleRef["kind"].(string)
				subjects, ok := object["subjects"].([]any)
				if !ok || len(subjects) != 1 {
					t.Fatalf("credentials RoleBinding subjects = %#v, want exactly one subject", object["subjects"])
				}
				subject := subjects[0].(map[string]any)
				subjectName, _ = subject["name"].(string)
				subjectNamespace, _ = subject["namespace"].(string)
			}
		}
	}

	if deploymentNamespace != testManagerNamespace || deploymentServiceAccount == "" {
		t.Fatalf("rendered manager identity = namespace %q, serviceAccount %q", deploymentNamespace, deploymentServiceAccount)
	}
	if roleName == "" || roleNamespace != deploymentNamespace {
		t.Fatalf("credentials Role identity = %q in %q; manager namespace is %q", roleName, roleNamespace, deploymentNamespace)
	}
	if bindingRoleName != roleName || bindingRoleKind != "Role" || bindingNamespace != roleNamespace {
		t.Fatalf("credentials RoleBinding namespace/ref = %q/%s %q, Role = %q/%q", bindingNamespace, bindingRoleKind, bindingRoleName, roleNamespace, roleName)
	}
	if subjectName != deploymentServiceAccount || subjectNamespace != deploymentNamespace {
		t.Fatalf("credentials RoleBinding subject = %q/%q, manager service account = %q/%q", subjectName, subjectNamespace, deploymentServiceAccount, deploymentNamespace)
	}
}

func objectMap(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	if key == "" {
		return object
	}
	value, ok := object[key].(map[string]any)
	if !ok {
		encoded, _ := json.Marshal(object)
		t.Fatalf("rendered object is missing map %q: %s", key, encoded)
	}
	return value
}
