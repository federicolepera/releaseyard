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
	"strings"
	"testing"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	int64IntegerError = "int64 integer"
	jsonObjectError   = "valid JSON object"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name       string
		definition *platformv1alpha1.ApplicationDefinition
		wantErr    string
	}{
		{
			name: "valid nested values and inclusive integer boundaries",
			definition: definition(`{"service":{"name":"api","replicas":3,"enabled":true}}`,
				value("service.name", stringType, nil, nil),
				value("service.replicas", integerType, new(int64(2)), new(int64(3))),
				value("service.enabled", booleanType, nil, nil),
			),
		},
		{
			name: "missing required and optional defaults are allowed",
			definition: definition(`{"other":"value"}`,
				valueWithRequired("required.path", stringType, true),
				valueWithRequired("optional.path", booleanType, false),
			),
		},
		{
			name:    "nil definition",
			wantErr: "application definition is nil",
		},
		{
			name:       "duplicate paths",
			definition: definition(`{}`, value("foo", stringType, nil, nil), value("foo", booleanType, nil, nil)),
			wantErr:    "duplicated",
		},
		{
			name:       "prefix-overlapping paths",
			definition: definition(`{"foo":{"bar":"value"}}`, value("foo", stringType, nil, nil), value("foo.bar", stringType, nil, nil)),
			wantErr:    "overlap",
		},
		{
			name:       "prefix comparison is segment aware",
			definition: definition(`{"foo":"one","foobar":"two"}`, value("foo", stringType, nil, nil), value("foobar", stringType, nil, nil)),
		},
		{
			name:       "malformed path",
			definition: definition(`{}`, value("foo..bar", stringType, nil, nil)),
			wantErr:    "path",
		},
		{
			name:       "unsupported type",
			definition: definition(`{}`, value("foo", "number", nil, nil)),
			wantErr:    "unsupported type",
		},
		{
			name:       "scalar intermediate parent",
			definition: definition(`{"foo":"secret-default"}`, value("foo.bar", stringType, nil, nil)),
			wantErr:    "non-object parent",
		},
		{
			name:       "string type mismatch",
			definition: definition(`{"foo":12}`, value("foo", stringType, nil, nil)),
			wantErr:    "must have type string",
		},
		{
			name:       "boolean type mismatch",
			definition: definition(`{"foo":"true"}`, value("foo", booleanType, nil, nil)),
			wantErr:    "must have type boolean",
		},
		{
			name:       "integer type mismatch",
			definition: definition(`{"foo":true}`, value("foo", integerType, nil, nil)),
			wantErr:    "must have type integer",
		},
		{
			name:       "null value rejected",
			definition: definition(`{"foo":null}`, value("foo", stringType, nil, nil)),
			wantErr:    "must not be null",
		},
		{
			name:       "integer decimal rejected",
			definition: definition(`{"foo":1.0}`, value("foo", integerType, nil, nil)),
			wantErr:    int64IntegerError,
		},
		{
			name:       "integer exponent rejected",
			definition: definition(`{"foo":1e2}`, value("foo", integerType, nil, nil)),
			wantErr:    int64IntegerError,
		},
		{
			name:       "integer overflow rejected",
			definition: definition(`{"foo":9223372036854775808}`, value("foo", integerType, nil, nil)),
			wantErr:    int64IntegerError,
		},
		{
			name:       "minimum inclusive boundary accepted",
			definition: definition(`{"foo":2}`, value("foo", integerType, new(int64(2)), new(int64(4)))),
		},
		{
			name:       "maximum inclusive boundary accepted",
			definition: definition(`{"foo":4}`, value("foo", integerType, new(int64(2)), new(int64(4)))),
		},
		{
			name:       "below minimum rejected",
			definition: definition(`{"foo":1}`, value("foo", integerType, new(int64(2)), new(int64(4)))),
			wantErr:    "below its minimum",
		},
		{
			name:       "above maximum rejected",
			definition: definition(`{"foo":5}`, value("foo", integerType, new(int64(2)), new(int64(4)))),
			wantErr:    "exceeds its maximum",
		},
		{
			name:       "malformed defaults rejected",
			definition: definition(`{"foo":"credential-secret"`, value("foo", stringType, nil, nil)),
			wantErr:    jsonObjectError,
		},
		{
			name:       "non-object defaults rejected",
			definition: definition(`["secret"]`, value("foo", stringType, nil, nil)),
			wantErr:    "JSON object",
		},
		{
			name:       "multiple JSON values rejected",
			definition: definition(`{} {}`, value("foo", stringType, nil, nil)),
			wantErr:    "exactly one JSON object",
		},
		{
			name:       "minimum on non-integer rejected",
			definition: definition(`{}`, value("foo", stringType, new(int64(1)), nil)),
			wantErr:    "numeric bounds for non-integer",
		},
		{
			name:       "maximum on non-integer rejected",
			definition: definition(`{}`, value("foo", booleanType, nil, new(int64(1)))),
			wantErr:    "numeric bounds for non-integer",
		},
		{
			name:       "minimum greater than maximum rejected",
			definition: definition(`{}`, value("foo", integerType, new(int64(5)), new(int64(2)))),
			wantErr:    "minimum greater than maximum",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := Validate(test.definition)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.wantErr)
			}
			if strings.Contains(err.Error(), "secret-default") || strings.Contains(err.Error(), "credential-secret") {
				t.Fatalf("Validate() error leaked a default value: %v", err)
			}
		})
	}
}

func definition(defaults string, values ...platformv1alpha1.ApplicationDefinitionValue) *platformv1alpha1.ApplicationDefinition {
	return &platformv1alpha1.ApplicationDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: contractTestDefinitionName},
		Spec: platformv1alpha1.ApplicationDefinitionSpec{
			Defaults:           &apiextensionsv1.JSON{Raw: []byte(defaults)},
			ConfigurableValues: values,
		},
	}
}

func value(path, valueType string, minimum, maximum *int64) platformv1alpha1.ApplicationDefinitionValue {
	return platformv1alpha1.ApplicationDefinitionValue{
		Path:    path,
		Type:    valueType,
		Minimum: minimum,
		Maximum: maximum,
	}
}

func valueWithRequired(path, valueType string, required bool) platformv1alpha1.ApplicationDefinitionValue {
	return platformv1alpha1.ApplicationDefinitionValue{Path: path, Type: valueType, Required: required}
}
