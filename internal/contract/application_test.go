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

package contract

import (
	"reflect"
	"strings"
	"testing"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	applicationsNamespace      = "applications"
	applicationTestNamespace   = "apps"
	contractTestDefinitionName = "definition"
	notConfigurableError       = "not configurable"
	replicaCountTwoJSON        = `{"replicaCount":2}`
)

func TestValidateApplication(t *testing.T) {
	validDefinition := definition(`{"replicaCount":1,"image":{"repository":"nginx","tag":"stable"},"other":{"kept":true}}`,
		valueWithRequired("replicaCount", integerType, true),
		value("image.repository", stringType, nil, nil),
		value("image.tag", stringType, nil, nil),
		value("enabled", booleanType, nil, nil),
	)
	validDefinition.Spec.Release.Namespace = applicationsNamespace
	for i := range validDefinition.Spec.ConfigurableValues {
		validDefinition.Spec.ConfigurableValues[i].Minimum = nil
		validDefinition.Spec.ConfigurableValues[i].Maximum = nil
	}
	validDefinition.Spec.ConfigurableValues[0].Minimum = new(int64(1))
	validDefinition.Spec.ConfigurableValues[0].Maximum = new(int64(5))
	validDefinition.Namespace = applicationsNamespace

	tests := []struct {
		name      string
		values    string
		nameValue string
		namespace string
		wantErr   string
	}{
		{name: "valid nested values", values: `{"replicaCount":3,"image":{"tag":"1.27-alpine"},"enabled":true}`},
		{name: "required must be supplied despite definition default", values: `{"image":{"tag":"1.27-alpine"}}`, wantErr: "must be supplied"},
		{name: "unknown value path", values: `{"unknown":"value","replicaCount":2}`, wantErr: notConfigurableError},
		{name: "unknown nested value path", values: `{"replicaCount":2,"image":{"extra":"x"}}`, wantErr: notConfigurableError},
		{name: "dotted alias rejected", values: `{"replicaCount":2,"image.tag":"v2"}`, wantErr: "nested objects"},
		{name: "object at configurable leaf rejected", values: `{"replicaCount":2,"image":{"tag":{"nested":"v2"}}}`, wantErr: "must be a scalar"},
		{name: "array rejected", values: `{"replicaCount":2,"image":{"tag":["v2"]}}`, wantErr: "must not be an array"},
		{name: "null rejected", values: `{"replicaCount":2,"image":{"tag":null}}`, wantErr: "must not be null"},
		{name: "wrong string type", values: `{"replicaCount":2,"image":{"tag":7}}`, wantErr: "type string"},
		{name: "wrong boolean type", values: `{"replicaCount":2,"enabled":"true"}`, wantErr: "type boolean"},
		{name: "wrong integer type", values: `{"replicaCount":"2"}`, wantErr: int64IntegerError},
		{name: "integer decimal rejected", values: `{"replicaCount":2.0}`, wantErr: int64IntegerError},
		{name: "integer exponent rejected", values: `{"replicaCount":2e0}`, wantErr: int64IntegerError},
		{name: "integer overflow rejected", values: `{"replicaCount":9223372036854775808}`, wantErr: int64IntegerError},
		{name: "integer below minimum", values: `{"replicaCount":0}`, wantErr: "below its minimum"},
		{name: "integer above maximum", values: `{"replicaCount":6}`, wantErr: "exceeds its maximum"},
		{name: "scalar parent rejected", values: `{"replicaCount":2,"image":"nginx"}`, wantErr: notConfigurableError},
		{name: "empty nested object rejected", values: `{"replicaCount":2,"image":{}}`, wantErr: "does not identify"},
		{name: "malformed JSON rejected", values: `{"replicaCount":2`, wantErr: jsonObjectError},
		{name: "non-object JSON rejected", values: `[]`, wantErr: jsonObjectError},
		{name: "release name too long", values: replicaCountTwoJSON, nameValue: strings.Repeat("a", 54), wantErr: "at most 53 characters"},
		{name: "invalid release name", values: replicaCountTwoJSON, nameValue: "has.dot", wantErr: "Helm release name"},
		{name: "maximum release name length accepted", values: replicaCountTwoJSON, nameValue: strings.Repeat("a", 53)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			application := applicationWithValues(test.values)
			if test.nameValue != "" {
				application.Name = test.nameValue
			}
			if test.namespace != "" {
				application.Namespace = test.namespace
			}
			if test.wantErr != "" {
				if err := ValidateApplication(validDefinition, application); err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("ValidateApplication() error = %v, want substring %q", err, test.wantErr)
				}
				return
			}
			if err := ValidateApplication(validDefinition, application); err != nil {
				t.Fatalf("ValidateApplication() error = %v", err)
			}
		})
	}
}

func TestValidateApplicationEnvironmentSelection(t *testing.T) {
	definition := definition(`{"replicaCount":1}`, value("replicaCount", integerType, nil, nil))
	definition.Namespace = applicationsNamespace
	definition.Spec.Release.Namespace = applicationsNamespace
	valid := []platformv1alpha1.ApplicationEnvironmentTarget{{Name: "cluster-a", UID: "uid-a"}, {Name: "cluster-b", UID: "uid-b"}}
	tests := []struct {
		name    string
		mutate  func(*platformv1alpha1.Application)
		wantErr string
	}{
		{name: "valid snapshot", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.ClusterRef = nil
			app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "Production_1", Targets: append([]platformv1alpha1.ApplicationEnvironmentTarget(nil), valid...)}
		}},
		{name: "no selector", mutate: func(app *platformv1alpha1.Application) { app.Spec.ClusterRef = nil }, wantErr: "exactly one"},
		{name: "both selectors", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "prod", Targets: append([]platformv1alpha1.ApplicationEnvironmentTarget(nil), valid...)}
		}, wantErr: "exactly one"},
		{name: "empty environment", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.ClusterRef = nil
			app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "", Targets: append([]platformv1alpha1.ApplicationEnvironmentTarget(nil), valid...)}
		}, wantErr: "environment name"},
		{name: "invalid environment label value", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.ClusterRef = nil
			app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "prod/blue", Targets: append([]platformv1alpha1.ApplicationEnvironmentTarget(nil), valid...)}
		}, wantErr: "environment name"},
		{name: "empty targets are dynamic", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.ClusterRef = nil
			app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "prod"}
		}},
		{name: "duplicate name", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.ClusterRef = nil
			app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "prod", Targets: []platformv1alpha1.ApplicationEnvironmentTarget{{Name: "cluster-a", UID: "uid-a"}, {Name: "cluster-a", UID: "uid-b"}}}
		}, wantErr: "duplicated"},
		{name: "duplicate UID", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.ClusterRef = nil
			app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "prod", Targets: []platformv1alpha1.ApplicationEnvironmentTarget{{Name: "cluster-a", UID: "uid-a"}, {Name: "cluster-b", UID: "uid-a"}}}
		}, wantErr: "UID"},
		{name: "unsorted names accepted", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.ClusterRef = nil
			app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "prod", Targets: []platformv1alpha1.ApplicationEnvironmentTarget{{Name: "cluster-b", UID: "uid-b"}, {Name: "cluster-a", UID: "uid-a"}}}
		}},
		{name: "duplicate name regardless of position", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.ClusterRef = nil
			app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "prod", Targets: []platformv1alpha1.ApplicationEnvironmentTarget{{Name: "cluster-b", UID: "uid-b"}, {Name: "cluster-a", UID: "uid-a"}, {Name: "cluster-b", UID: "uid-c"}}}
		}, wantErr: "duplicated"},
		{name: "invalid target DNS name", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.ClusterRef = nil
			app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "prod", Targets: []platformv1alpha1.ApplicationEnvironmentTarget{{Name: "Bad_name", UID: "uid"}}}
		}, wantErr: "target 0 name"},
		{name: "target DNS label too long", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.ClusterRef = nil
			app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "prod", Targets: []platformv1alpha1.ApplicationEnvironmentTarget{{Name: strings.Repeat("a", 64), UID: "uid"}}}
		}, wantErr: "target 0 name"},
		{name: "empty target UID", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.ClusterRef = nil
			app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "prod", Targets: []platformv1alpha1.ApplicationEnvironmentTarget{{Name: "cluster-a"}}}
		}, wantErr: "UID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := applicationWithValues(replicaCountTwoJSON)
			tt.mutate(app)
			err := ValidateApplication(definition, app)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateApplication() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateApplication() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateApplicationClusterSelection(t *testing.T) {
	definition := definition(`{"replicaCount":1}`, value("replicaCount", integerType, nil, nil))
	definition.Namespace = applicationsNamespace
	definition.Spec.Release.Namespace = applicationsNamespace
	valid := []platformv1alpha1.ApplicationClusterTarget{{Name: "cluster-a", UID: "uid-a"}, {Name: "cluster-b", UID: "uid-b"}}
	tests := []struct {
		name    string
		mutate  func(*platformv1alpha1.Application)
		wantErr string
	}{
		{name: "valid selection", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.ClusterRef = nil
			app.Spec.Clusters = append([]platformv1alpha1.ApplicationClusterTarget(nil), valid...)
		}},
		{name: "clusterRef and clusters", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.Clusters = append([]platformv1alpha1.ApplicationClusterTarget(nil), valid...)
		}, wantErr: "exactly one"},
		{name: "environment and clusters", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.ClusterRef = nil
			app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: "prod"}
			app.Spec.Clusters = append([]platformv1alpha1.ApplicationClusterTarget(nil), valid...)
		}, wantErr: "exactly one"},
		{name: "empty selection", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.ClusterRef = nil
			app.Spec.Clusters = []platformv1alpha1.ApplicationClusterTarget{}
		}, wantErr: "exactly one"},
		{name: "duplicate name", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.ClusterRef = nil
			app.Spec.Clusters = []platformv1alpha1.ApplicationClusterTarget{{Name: "cluster-a", UID: "uid-a"}, {Name: "cluster-a", UID: "uid-b"}}
		}, wantErr: "duplicated"},
		{name: "duplicate UID", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.ClusterRef = nil
			app.Spec.Clusters = []platformv1alpha1.ApplicationClusterTarget{{Name: "cluster-a", UID: "uid-a"}, {Name: "cluster-b", UID: "uid-a"}}
		}, wantErr: "UID"},
		{name: "invalid target DNS name", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.ClusterRef = nil
			app.Spec.Clusters = []platformv1alpha1.ApplicationClusterTarget{{Name: "Bad_name", UID: "uid"}}
		}, wantErr: "target 0 name"},
		{name: "empty target UID", mutate: func(app *platformv1alpha1.Application) {
			app.Spec.ClusterRef = nil
			app.Spec.Clusters = []platformv1alpha1.ApplicationClusterTarget{{Name: "cluster-a"}}
		}, wantErr: "UID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := applicationWithValues(replicaCountTwoJSON)
			tt.mutate(app)
			err := ValidateApplication(definition, app)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateApplication() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateApplication() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateApplicationOptionalValuesAndReferences(t *testing.T) {
	definition := definition(`{"optional":"default"}`, value("optional", stringType, nil, nil))
	definition.Namespace = applicationTestNamespace
	definition.Spec.Release.Namespace = applicationsNamespace
	application := applicationWithValues("")
	application.Namespace = applicationTestNamespace
	application.Spec.Values = nil
	if err := ValidateApplication(definition, application); err != nil {
		t.Fatalf("ValidateApplication() with optional omitted value error = %v", err)
	}

	application.Spec.ApplicationDefinitionRef.Name = ""
	if err := ValidateApplication(definition, application); err == nil || !strings.Contains(err.Error(), "applicationDefinitionRef name") {
		t.Fatalf("ValidateApplication() empty definition ref error = %v", err)
	}
	application.Spec.ApplicationDefinitionRef.Name = contractTestDefinitionName
	application.Spec.ClusterRef.Name = "Bad_Name"
	if err := ValidateApplication(definition, application); err == nil || !strings.Contains(err.Error(), "clusterRef name") {
		t.Fatalf("ValidateApplication() invalid cluster ref error = %v", err)
	}
}

func TestEffectiveReleaseNamespace(t *testing.T) {
	definition := definition(`{}`)
	definition.Spec.Release.Namespace = applicationsNamespace
	application := applicationWithValues("")

	got, err := EffectiveReleaseNamespace(definition, application)
	if err != nil || got != applicationsNamespace {
		t.Fatalf("EffectiveReleaseNamespace() = %q, %v; want %q, nil", got, err, applicationsNamespace)
	}

	selected := applicationsNamespace
	application.Spec.ReleaseNamespace = &selected
	if _, err := EffectiveReleaseNamespace(definition, application); err == nil || !strings.Contains(err.Error(), "does not allow") {
		t.Fatalf("same-name explicit override error = %v, want override rejection", err)
	}

	definition.Spec.Release.AllowNamespaceOverride = true
	selected = "team-workloads"
	got, err = EffectiveReleaseNamespace(definition, application)
	if err != nil || got != selected {
		t.Fatalf("allowed EffectiveReleaseNamespace() = %q, %v; want %q, nil", got, err, selected)
	}

	for _, invalid := range []string{"", "Team", "team.workloads", strings.Repeat("a", 64), "-team"} {
		application.Spec.ReleaseNamespace = &invalid
		if _, err := EffectiveReleaseNamespace(definition, application); err == nil || !strings.Contains(err.Error(), "valid DNS label") {
			t.Errorf("EffectiveReleaseNamespace(%q) error = %v, want invalid namespace error", invalid, err)
		}
	}
}

func TestEffectiveReleaseName(t *testing.T) {
	const prefix = "releaseyard-team-"
	cases := []struct {
		name      string
		namespace string
		app       string
		want      string
	}{
		{name: "team prefix stripped", namespace: "releaseyard-team-platform", app: "payments", want: "platform-payments"},
		{name: "no prefix uses namespace", namespace: "apps", app: "payments", want: "apps-payments"},
		{name: "invalid characters sanitized", namespace: "releaseyard-team-Team_A", app: "My App", want: "team-a-my-app"},
		{name: "empty namespace prefix empty", namespace: prefix, app: "payments", want: "payments"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveReleaseName(tc.namespace, tc.app, prefix); got != tc.want {
				t.Fatalf("EffectiveReleaseName() = %q, want %q", got, tc.want)
			}
		})
	}

	longNamespace := "releaseyard-team-" + strings.Repeat("a", 40)
	longName := strings.Repeat("b", 40)
	first := EffectiveReleaseName(longNamespace, longName, prefix)
	if len(first) > releaseNameMaxLength {
		t.Fatalf("truncated name length = %d, want <= %d", len(first), releaseNameMaxLength)
	}
	if second := EffectiveReleaseName(longNamespace, longName, prefix); second != first {
		t.Fatalf("EffectiveReleaseName not deterministic: %q != %q", first, second)
	}
	if other := EffectiveReleaseName(longNamespace, longName+"c", prefix); other == first {
		t.Fatalf("different inputs produced the same release name %q", first)
	}
}

func TestValidateApplicationReleaseNamespacePolicyDoesNotChangeRequiredValues(t *testing.T) {
	definition := definition(`{"required":"default"}`, valueWithRequired("required", stringType, true))
	definition.Namespace = applicationsNamespace
	definition.Spec.Release.Namespace = applicationsNamespace
	application := applicationWithValues("")
	application.Spec.Values = nil
	if err := ValidateApplication(definition, application); err == nil || !strings.Contains(err.Error(), "must be supplied") {
		t.Fatalf("ValidateApplication() missing required value error = %v, want required-value error", err)
	}
}

func TestApplicationDefinitionReferenceIdentity(t *testing.T) {
	definition := definition(`{}`, value("setting", stringType, nil, nil))
	definition.Namespace = applicationTestNamespace
	definition.Spec.Release.Namespace = applicationsNamespace
	application := applicationWithValues(`{"setting":"value"}`)
	application.Namespace = applicationTestNamespace

	for _, test := range []struct {
		name   string
		change func(*platformv1alpha1.Application)
	}{
		{
			name: "wrong definition name",
			change: func(application *platformv1alpha1.Application) {
				application.Spec.ApplicationDefinitionRef.Name = "another-definition"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := application.DeepCopy()
			test.change(candidate)
			if err := ValidateApplication(definition, candidate); err == nil {
				t.Fatal("ValidateApplication() error = nil, want reference identity rejection")
			}
			if _, err := MergeValues(definition, candidate); err == nil {
				t.Fatal("MergeValues() error = nil, want reference identity rejection")
			}
		})
	}
}

func TestMergeValuesDeepCopiesAndOverlaysDefaultsDeterministically(t *testing.T) {
	definition := definition(`{"replicaCount":1,"image":{"repository":"nginx","tag":"stable"},"unconfigured":{"array":[1,2],"flag":true}}`,
		value("replicaCount", integerType, nil, nil),
		value("image.tag", stringType, nil, nil),
	)
	definition.Namespace = applicationTestNamespace
	definition.Spec.Release.Namespace = applicationsNamespace
	application := applicationWithValues(`{"replicaCount":3,"image":{"tag":"1.27-alpine"}}`)
	application.Namespace = applicationTestNamespace
	definitionRaw := string(definition.Spec.Defaults.Raw)
	applicationRaw := string(application.Spec.Values.Raw)

	first, err := MergeValues(definition, application)
	if err != nil {
		t.Fatalf("MergeValues() error = %v", err)
	}
	second, err := MergeValues(definition, application)
	if err != nil {
		t.Fatalf("second MergeValues() error = %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("MergeValues() differs between calls: first=%#v second=%#v", first, second)
	}
	if got := first["replicaCount"]; got != int64(3) {
		t.Errorf("replicaCount = %#v, want int64(3)", got)
	}
	image := first["image"].(map[string]any)
	if image["repository"] != "nginx" || image["tag"] != "1.27-alpine" {
		t.Errorf("merged image = %#v", image)
	}
	if first["unconfigured"].(map[string]any)["flag"] != true {
		t.Errorf("unconfigured defaults were not preserved: %#v", first["unconfigured"])
	}
	if string(definition.Spec.Defaults.Raw) != definitionRaw || string(application.Spec.Values.Raw) != applicationRaw {
		t.Fatal("MergeValues() mutated one of its input objects")
	}
	first["unconfigured"].(map[string]any)["flag"] = false
	third, err := MergeValues(definition, application)
	if err != nil {
		t.Fatalf("third MergeValues() error = %v", err)
	}
	if third["unconfigured"].(map[string]any)["flag"] != true {
		t.Fatal("MergeValues() result aliases definition defaults")
	}
}

func TestMergeValuesWithoutDefaults(t *testing.T) {
	definition := &platformv1alpha1.ApplicationDefinition{ObjectMeta: metav1.ObjectMeta{Name: contractTestDefinitionName}}
	definition.Namespace = applicationsNamespace
	definition.Spec.Release.Namespace = applicationsNamespace
	definition.Spec.ConfigurableValues = []platformv1alpha1.ApplicationDefinitionValue{value("enabled", booleanType, nil, nil)}
	application := applicationWithValues(`{"enabled":true}`)
	merged, err := MergeValues(definition, application)
	if err != nil {
		t.Fatalf("MergeValues() error = %v", err)
	}
	if !reflect.DeepEqual(merged, map[string]any{"enabled": true}) {
		t.Fatalf("MergeValues() = %#v, want enabled=true", merged)
	}
}

func TestMergeValuesPreservesExactAndFractionalDefaultNumbers(t *testing.T) {
	definition := definition(`{"large":9007199254740993,"minimum":-9223372036854775808,"maximum":9223372036854775807,"fraction":1.25,"exponent":1e3,"nested":[{"exact":9007199254740993}]}`)
	definition.Namespace = applicationsNamespace
	definition.Spec.Release.Namespace = applicationsNamespace
	application := applicationWithValues("")
	definitionRaw := string(definition.Spec.Defaults.Raw)
	merged, err := MergeValues(definition, application)
	if err != nil {
		t.Fatalf("MergeValues() error = %v", err)
	}
	if merged["large"] != int64(9007199254740993) || merged["minimum"] != int64(-9223372036854775808) || merged["maximum"] != int64(9223372036854775807) {
		t.Fatalf("integer defaults were not preserved exactly: %#v", merged)
	}
	if merged["fraction"] != float64(1.25) || merged["exponent"] != float64(1000) {
		t.Fatalf("fractional defaults were not converted: %#v", merged)
	}
	nested := merged["nested"].([]any)[0].(map[string]any)
	if nested["exact"] != int64(9007199254740993) {
		t.Fatalf("nested integer default was not preserved exactly: %#v", nested)
	}
	if string(definition.Spec.Defaults.Raw) != definitionRaw {
		t.Fatal("MergeValues() mutated definition defaults")
	}
}

func TestMergeValuesRejectsUnrepresentableDefaultNumbers(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "positive integer overflow", raw: `{"value":9223372036854775808}`},
		{name: "negative integer overflow", raw: `{"nested":{"value":-9223372036854775809}}`},
		{name: "integer overflow in nested array", raw: `{"nested":[{"values":[1,9223372036854775808]}]}`},
		{name: "exponent overflow", raw: `{"nested":[1e400]}`},
		{name: "negative exponent overflow", raw: `{"nested":{"value":-1e400}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition := definition(test.raw)
			definition.Namespace = applicationsNamespace
			definition.Spec.Release.Namespace = applicationsNamespace
			application := applicationWithValues("")
			original := string(definition.Spec.Defaults.Raw)
			if _, err := MergeValues(definition, application); err == nil {
				t.Fatal("MergeValues() error = nil, want numeric conversion failure")
			}
			if string(definition.Spec.Defaults.Raw) != original {
				t.Fatal("MergeValues() mutated definition defaults on failure")
			}
		})
	}
}

func applicationWithValues(raw string) *platformv1alpha1.Application {
	application := &platformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: applicationsNamespace},
		Spec: platformv1alpha1.ApplicationSpec{
			ApplicationDefinitionRef: platformv1alpha1.ApplicationReference{Name: contractTestDefinitionName},
			ClusterRef:               &platformv1alpha1.ApplicationReference{Name: "workload-a"},
		},
	}
	if raw != "" {
		application.Spec.Values = &apiextensionsv1.JSON{Raw: []byte(raw)}
	}
	return application
}
