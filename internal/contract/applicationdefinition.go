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

// Package contract validates ApplicationDefinition's defaults against its
// declared configurable-value contract. It does not inspect chart contents.
package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
)

const (
	stringType  = "string"
	integerType = "integer"
	booleanType = "boolean"
)

// Validate checks that an ApplicationDefinition is internally consistent and
// that any defaults at declared configurable paths conform to their contract.
// Required indicates whether a future Application must supply a value; it does
// not require a default to be present here.
func Validate(def *platformv1alpha1.ApplicationDefinition) error {
	if def == nil {
		return fmt.Errorf("application definition is nil")
	}

	if err := validateConfigurableValues(def.Spec.ConfigurableValues); err != nil {
		return err
	}
	if def.Spec.Defaults == nil {
		return nil
	}
	defaults, err := decodeDefaults(def.Spec.Defaults.Raw)
	if err != nil {
		return err
	}
	return validateDefaults(def.Spec.ConfigurableValues, defaults)
}

func validateConfigurableValues(values []platformv1alpha1.ApplicationDefinitionValue) error {
	paths := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validPath(value.Path) {
			return fmt.Errorf("configurable path %q is invalid", value.Path)
		}
		if _, ok := paths[value.Path]; ok {
			return fmt.Errorf("configurable path %q is duplicated", value.Path)
		}
		paths[value.Path] = struct{}{}

		if !validType(value.Type) {
			return fmt.Errorf("configurable path %q has unsupported type %q", value.Path, value.Type)
		}
		if value.Minimum != nil || value.Maximum != nil {
			if value.Type != integerType {
				return fmt.Errorf("configurable path %q has numeric bounds for non-integer type", value.Path)
			}
		}
		if value.Minimum != nil && value.Maximum != nil && *value.Minimum > *value.Maximum {
			return fmt.Errorf("configurable path %q has minimum greater than maximum", value.Path)
		}
	}

	for path := range paths {
		segments := strings.Split(path, ".")
		for i := 1; i < len(segments); i++ {
			if _, exists := paths[strings.Join(segments[:i], ".")]; exists {
				return fmt.Errorf("configurable paths overlap at %q", strings.Join(segments[:i], "."))
			}
		}
	}

	return nil
}

func validateDefaults(values []platformv1alpha1.ApplicationDefinitionValue, defaults map[string]any) error {
	for _, value := range values {
		defaultValue, exists, err := valueAtPath(defaults, value.Path)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if defaultValue == nil {
			return fmt.Errorf("default at configurable path %q must not be null", value.Path)
		}

		switch value.Type {
		case stringType:
			if _, ok := defaultValue.(string); !ok {
				return fmt.Errorf("default at configurable path %q must have type string", value.Path)
			}
		case booleanType:
			if _, ok := defaultValue.(bool); !ok {
				return fmt.Errorf("default at configurable path %q must have type boolean", value.Path)
			}
		case integerType:
			number, ok := defaultValue.(json.Number)
			if !ok {
				return fmt.Errorf("default at configurable path %q must have type integer", value.Path)
			}
			integer, err := number.Int64()
			if err != nil {
				return fmt.Errorf("default at configurable path %q must be an int64 integer", value.Path)
			}
			if value.Minimum != nil && integer < *value.Minimum {
				return fmt.Errorf("default at configurable path %q is below its minimum", value.Path)
			}
			if value.Maximum != nil && integer > *value.Maximum {
				return fmt.Errorf("default at configurable path %q exceeds its maximum", value.Path)
			}
		}
	}

	return nil
}

func validPath(path string) bool {
	if len(path) == 0 || len(path) > 253 {
		return false
	}
	for segment := range strings.SplitSeq(path, ".") {
		if segment == "" || !isASCIIAlpha(segment[0]) {
			return false
		}
		for i := 1; i < len(segment); i++ {
			c := segment[i]
			if !isASCIIAlpha(c) && (c < '0' || c > '9') && c != '_' && c != '-' {
				return false
			}
		}
	}
	return true
}

func isASCIIAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func validType(valueType string) bool {
	return valueType == stringType || valueType == integerType || valueType == booleanType
}

func decodeDefaults(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("defaults must be a valid JSON object")
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, fmt.Errorf("defaults must be a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("defaults must contain exactly one JSON object")
	}
	return value.(map[string]any), nil
}

func valueAtPath(root map[string]any, path string) (any, bool, error) {
	var current any = root
	segments := strings.Split(path, ".")
	for i, segment := range segments {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("default path %q has a non-object parent", strings.Join(segments[:i], "."))
		}
		current, ok = object[segment]
		if !ok {
			return nil, false, nil
		}
	}
	return current, true, nil
}
