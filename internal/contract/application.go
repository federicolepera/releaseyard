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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

var (
	helmReleaseNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dnsSubdomainPattern    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

const (
	// releaseNameMaxLength is the Helm limit for a release name.
	releaseNameMaxLength = 53
	// releaseNameHashLength is the deterministic suffix added to truncated names.
	releaseNameHashLength = 6
	// releaseNameTrimLength leaves room for the hash suffix and its dash.
	releaseNameTrimLength = releaseNameMaxLength - releaseNameHashLength - 1
)

// EffectiveReleaseName derives the Helm release name used on a workload cluster
// from the Application's team namespace and name, so two teams can deploy the
// same application name into the same workload namespace without colliding.
//
// The team namespace prefix (for example "releaseyard-team-") is stripped so
// "releaseyard-team-platform" with an application "payments" becomes
// "platform-payments". The result is always a valid DNS-1123 label of at most
// 53 characters; longer results are truncated and given a deterministic hash
// suffix so the mapping stays stable and unique.
func EffectiveReleaseName(teamNamespace, name, teamNamespacePrefix string) string {
	team := sanitizeReleaseSegment(strings.TrimPrefix(teamNamespace, teamNamespacePrefix))
	segment := sanitizeReleaseSegment(name)
	combined := segment
	if team != "" {
		combined = team + "-" + segment
	}
	if len(combined) <= releaseNameMaxLength {
		return combined
	}
	sum := sha256.Sum256([]byte(combined))
	suffix := hex.EncodeToString(sum[:])[:releaseNameHashLength]
	return strings.Trim(combined[:releaseNameTrimLength], "-") + "-" + suffix
}

// sanitizeReleaseSegment lowercases a value and replaces every run of
// non-alphanumeric characters with a single dash, so the result is a valid
// DNS-1123 label fragment.
func sanitizeReleaseSegment(value string) string {
	var builder strings.Builder
	pendingDash := false
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pendingDash && builder.Len() > 0 {
				builder.WriteByte('-')
			}
			pendingDash = false
			builder.WriteRune(r)
			continue
		}
		pendingDash = true
	}
	return builder.String()
}

// ValidateApplication validates an Application's user values against the
// configurable paths in its ApplicationDefinition. Definition defaults are
// validated by Validate; chart files and chart defaults are never inspected.
func ValidateApplication(definition *platformv1alpha1.ApplicationDefinition, application *platformv1alpha1.Application) error {
	if definition == nil {
		return fmt.Errorf("application definition is nil")
	}
	if application == nil {
		return fmt.Errorf("application is nil")
	}
	if len(application.Name) == 0 || len(application.Name) > 53 || !helmReleaseNamePattern.MatchString(application.Name) {
		return fmt.Errorf("application name must be a valid Helm release name of at most 53 characters")
	}
	if application.Spec.ApplicationDefinitionRef.Name != definition.Name {
		return fmt.Errorf("applicationDefinitionRef name does not match the supplied ApplicationDefinition")
	}
	if !dnsSubdomainPattern.MatchString(application.Spec.ApplicationDefinitionRef.Name) {
		return fmt.Errorf("applicationDefinitionRef name is invalid")
	}
	if (application.Spec.ClusterRef == nil) == (application.Spec.Environment == nil) {
		return fmt.Errorf("exactly one of clusterRef or environment must be set")
	}
	if application.Spec.ClusterRef != nil {
		if !dnsSubdomainPattern.MatchString(application.Spec.ClusterRef.Name) {
			return fmt.Errorf("clusterRef name is invalid")
		}
	} else if err := validateEnvironmentSelection(application.Spec.Environment); err != nil {
		return err
	}
	if err := Validate(definition); err != nil {
		return fmt.Errorf("application definition contract is invalid: %w", err)
	}
	if _, err := EffectiveReleaseNamespace(definition, application); err != nil {
		return err
	}

	if err := validateConfigurableValues(definition.Spec.ConfigurableValues); err != nil {
		return err
	}
	allowed := make(map[string]platformv1alpha1.ApplicationDefinitionValue, len(definition.Spec.ConfigurableValues))
	for _, configured := range definition.Spec.ConfigurableValues {
		allowed[configured.Path] = configured
	}
	var overrides map[string]any
	if application.Spec.Values != nil {
		var err error
		overrides, err = decodeDefaults(application.Spec.Values.Raw)
		if err != nil {
			return fmt.Errorf("application values must be a valid JSON object")
		}
	}
	if overrides == nil {
		overrides = map[string]any{}
	}
	if err := validateOverrideObject(overrides, "", allowed); err != nil {
		return err
	}
	for _, configured := range definition.Spec.ConfigurableValues {
		if !configured.Required {
			continue
		}
		if _, exists := submittedValue(overrides, configured.Path); !exists {
			return fmt.Errorf("required configurable path %q must be supplied by the Application", configured.Path)
		}
	}
	return nil
}

func validateEnvironmentSelection(environment *platformv1alpha1.ApplicationEnvironmentSelection) error {
	if environment == nil {
		return fmt.Errorf("environment selection is required")
	}
	if len(environment.Name) == 0 || len(environment.Name) > 63 || len(validation.IsValidLabelValue(environment.Name)) != 0 {
		return fmt.Errorf("environment name must be a valid Kubernetes label value of at most 63 characters")
	}
	if len(environment.Targets) > 32 {
		return fmt.Errorf("environment targets must contain at most 32 Clusters")
	}
	seenUIDs := make(map[types.UID]struct{}, len(environment.Targets))
	seenNames := make(map[string]struct{}, len(environment.Targets))
	for i, target := range environment.Targets {
		if !validClusterDNSSubdomain(target.Name) {
			return fmt.Errorf("environment target %d name is invalid", i)
		}
		if target.UID == "" || len(target.UID) > 128 {
			return fmt.Errorf("environment target %q UID must contain between 1 and 128 characters", target.Name)
		}
		if _, exists := seenNames[target.Name]; exists {
			return fmt.Errorf("environment target name %q is duplicated", target.Name)
		}
		seenNames[target.Name] = struct{}{}
		if _, exists := seenUIDs[target.UID]; exists {
			return fmt.Errorf("environment target UID %q is duplicated", target.UID)
		}
		seenUIDs[target.UID] = struct{}{}
	}
	return nil
}

func validClusterDNSSubdomain(name string) bool {
	if len(name) == 0 || len(name) > 253 || !dnsSubdomainPattern.MatchString(name) || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if len(label) > 63 {
			return false
		}
	}
	return true
}

// EffectiveReleaseNamespace returns the target namespace selected by an Application,
// enforcing its ApplicationDefinition's namespace override policy.
func EffectiveReleaseNamespace(definition *platformv1alpha1.ApplicationDefinition, application *platformv1alpha1.Application) (string, error) {
	if definition == nil {
		return "", fmt.Errorf("application definition is nil")
	}
	if application == nil {
		return "", fmt.Errorf("application is nil")
	}
	defaultNamespace := definition.Spec.Release.Namespace
	if !helmReleaseNamePattern.MatchString(defaultNamespace) || len(defaultNamespace) > 63 {
		return "", fmt.Errorf("application definition release namespace is invalid")
	}
	if application.Spec.ReleaseNamespace == nil {
		return defaultNamespace, nil
	}
	selected := *application.Spec.ReleaseNamespace
	if len(selected) == 0 || len(selected) > 63 || !helmReleaseNamePattern.MatchString(selected) {
		return "", fmt.Errorf("application releaseNamespace must be a valid DNS label of at most 63 characters")
	}
	if !definition.Spec.Release.AllowNamespaceOverride {
		return "", fmt.Errorf("application definition does not allow a release namespace override")
	}
	return selected, nil
}

// MergeValues returns a deep-copied Helm-ready map containing definition
// defaults overlaid by validated Application values. It does not inspect
// chart defaults or mutate either input.
func MergeValues(definition *platformv1alpha1.ApplicationDefinition, application *platformv1alpha1.Application) (map[string]any, error) {
	if err := ValidateApplication(definition, application); err != nil {
		return nil, err
	}
	merged := map[string]any{}
	if definition.Spec.Defaults != nil {
		defaults, err := decodeDefaults(definition.Spec.Defaults.Raw)
		if err != nil {
			return nil, fmt.Errorf("application definition defaults are invalid")
		}
		merged, err = helmValueMap(defaults)
		if err != nil {
			return nil, fmt.Errorf("application definition defaults contain a number that cannot be represented without loss")
		}
	}
	if application.Spec.Values == nil {
		return merged, nil
	}
	overrides, err := decodeDefaults(application.Spec.Values.Raw)
	if err != nil {
		return nil, fmt.Errorf("application values must be a valid JSON object")
	}
	helmedOverrides, err := helmValueMap(overrides)
	if err != nil {
		return nil, fmt.Errorf("application values contain a number that cannot be represented without loss")
	}
	mergeValueMaps(merged, helmedOverrides)
	return merged, nil
}

func validateOverrideObject(object map[string]any, prefix string, allowed map[string]platformv1alpha1.ApplicationDefinitionValue) error {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		value := object[key]
		if key == "" || strings.Contains(key, ".") {
			return fmt.Errorf("application value path %q is invalid; use nested objects for dotted paths", joinValuePath(prefix, key))
		}
		path := joinValuePath(prefix, key)
		switch typed := value.(type) {
		case map[string]any:
			if _, isLeaf := allowed[path]; isLeaf {
				return fmt.Errorf("configurable path %q must be a scalar value", path)
			}
			if len(typed) == 0 {
				return fmt.Errorf("application value path %q does not identify a configurable value", path)
			}
			if err := validateOverrideObject(typed, path, allowed); err != nil {
				return err
			}
		case nil:
			return fmt.Errorf("application value at configurable path %q must not be null", path)
		case []any:
			return fmt.Errorf("application value at path %q must not be an array", path)
		default:
			configured, exists := allowed[path]
			if !exists {
				return fmt.Errorf("application value path %q is not configurable", path)
			}
			if err := validateApplicationScalar(path, value, configured); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateApplicationScalar(path string, actual any, configured platformv1alpha1.ApplicationDefinitionValue) error {
	switch configured.Type {
	case stringType:
		if _, ok := actual.(string); !ok {
			return fmt.Errorf("application value at configurable path %q must have type string", path)
		}
	case booleanType:
		if _, ok := actual.(bool); !ok {
			return fmt.Errorf("application value at configurable path %q must have type boolean", path)
		}
	case integerType:
		number, ok := actual.(json.Number)
		if !ok {
			return fmt.Errorf("application value at configurable path %q must be an int64 integer", path)
		}
		integer, err := number.Int64()
		if err != nil {
			return fmt.Errorf("application value at configurable path %q must be an int64 integer", path)
		}
		if configured.Minimum != nil && integer < *configured.Minimum {
			return fmt.Errorf("application value at configurable path %q is below its minimum", path)
		}
		if configured.Maximum != nil && integer > *configured.Maximum {
			return fmt.Errorf("application value at configurable path %q exceeds its maximum", path)
		}
	default:
		return fmt.Errorf("configurable path %q has unsupported type", path)
	}
	return nil
}

func joinValuePath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func submittedValue(root map[string]any, path string) (any, bool) {
	var current any = root
	for segment := range strings.SplitSeq(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func helmValueMap(value map[string]any) (map[string]any, error) {
	result := make(map[string]any, len(value))
	for key, child := range value {
		converted, err := helmValue(child)
		if err != nil {
			return nil, err
		}
		result[key] = converted
	}
	return result, nil
}

func helmValue(value any) (any, error) {
	switch typed := value.(type) {
	case map[string]any:
		return helmValueMap(typed)
	case []any:
		result := make([]any, len(typed))
		for i := range typed {
			converted, err := helmValue(typed[i])
			if err != nil {
				return nil, err
			}
			result[i] = converted
		}
		return result, nil
	case json.Number:
		if !strings.ContainsAny(typed.String(), ".eE") {
			integer, err := typed.Int64()
			if err != nil {
				return nil, fmt.Errorf("JSON integer cannot be represented as int64 without loss")
			}
			return integer, nil
		}
		number, err := typed.Float64()
		if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
			return nil, fmt.Errorf("JSON decimal cannot be represented as a finite float64")
		}
		return number, nil
	}
	return value, nil
}

func mergeValueMaps(destination, overrides map[string]any) {
	for key, override := range overrides {
		if overrideMap, ok := override.(map[string]any); ok {
			if destinationMap, exists := destination[key].(map[string]any); exists {
				mergeValueMaps(destinationMap, overrideMap)
				continue
			}
			destination[key] = overrideMap
			continue
		}
		destination[key] = override
	}
}
