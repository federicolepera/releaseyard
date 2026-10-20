package controller

import (
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
)

const (
	managedByLabel = "app.kubernetes.io/managed-by"
	resourceLabel  = "platform.releaseyard.io/resource"
)

// buildManagedResource renders one managed resource from a Resource, its
// CloudTarget and a ResourceDefinition template. It is pure and does not talk
// to the API server.
func buildManagedResource(
	resource *platformv1alpha1.Resource,
	target *platformv1alpha1.CloudTarget,
	template platformv1alpha1.ResourceTemplate,
	scheme *runtime.Scheme,
) (*unstructured.Unstructured, error) {
	object := map[string]any{}
	if template.Base != nil && len(template.Base.Raw) > 0 {
		if err := json.Unmarshal(template.Base.Raw, &object); err != nil {
			return nil, fmt.Errorf("decode base for template %q: %w", template.Name, err)
		}
	}
	object["apiVersion"] = template.APIVersion
	object["kind"] = template.Kind

	metadata, _ := object["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["name"] = managedResourceName(resource.Name, template.Name)
	metadata["namespace"] = resource.Namespace
	labels, _ := metadata["labels"].(map[string]any)
	if labels == nil {
		labels = map[string]any{}
	}
	labels[managedByLabel] = "releaseyard"
	labels[resourceLabel] = resource.Name
	metadata["labels"] = labels
	object["metadata"] = metadata

	spec, _ := object["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
	}
	spec["providerConfigRef"] = map[string]any{
		"name": target.Spec.ProviderConfigRef.Name,
		"kind": "ProviderConfig",
	}
	object["spec"] = spec

	managed := &unstructured.Unstructured{Object: object}
	if err := controllerutil.SetControllerReference(resource, managed, scheme); err != nil {
		return nil, fmt.Errorf("set owner reference for template %q: %w", template.Name, err)
	}

	values := map[string]any{}
	if resource.Spec.Values != nil && len(resource.Spec.Values.Raw) > 0 {
		if err := json.Unmarshal(resource.Spec.Values.Raw, &values); err != nil {
			return nil, fmt.Errorf("decode resource values: %w", err)
		}
	}
	targetValues := map[string]any{
		"provider": target.Spec.Provider,
		"region":   target.Spec.Region,
		"providerConfigRef": map[string]any{
			"name":      target.Spec.ProviderConfigRef.Name,
			"namespace": target.Spec.ProviderConfigRef.Namespace,
		},
	}
	for _, mapping := range template.Mappings {
		value, found, err := mappingValue(mapping.From, values, targetValues)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		segments, err := parseFieldPath(mapping.To)
		if err != nil {
			return nil, fmt.Errorf("invalid target path %q: %w", mapping.To, err)
		}
		if err := setNestedField(managed.Object, segments, value); err != nil {
			return nil, fmt.Errorf("set %q: %w", mapping.To, err)
		}
	}
	return managed, nil
}

func managedResourceName(resource, template string) string {
	name := resource + "-" + template
	if len(name) <= 253 {
		return name
	}
	return strings.TrimRight(name[:253], "-.")
}

func mappingValue(from string, values, targetValues map[string]any) (any, bool, error) {
	switch {
	case strings.HasPrefix(from, "values."):
		return nestedValue(values, strings.TrimPrefix(from, "values."))
	case strings.HasPrefix(from, "target."):
		return nestedValue(targetValues, strings.TrimPrefix(from, "target."))
	default:
		return nil, false, fmt.Errorf("unsupported mapping source %q: want values.* or target.*", from)
	}
}

func nestedValue(root map[string]any, path string) (any, bool, error) {
	if path == "" {
		return nil, false, fmt.Errorf("empty source path")
	}
	var current any = root
	for _, segment := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		current, ok = object[segment]
		if !ok {
			return nil, false, nil
		}
	}
	return current, true, nil
}

func parseFieldPath(path string) ([]string, error) {
	var segments []string
	for len(path) > 0 {
		switch path[0] {
		case '.':
			path = path[1:]
		case '[':
			end := strings.IndexByte(path, ']')
			if end < 0 {
				return nil, fmt.Errorf("unclosed '['")
			}
			if end == 1 {
				return nil, fmt.Errorf("empty key")
			}
			segments = append(segments, path[1:end])
			path = path[end+1:]
		default:
			next := strings.IndexAny(path, ".[")
			if next < 0 {
				next = len(path)
			}
			segments = append(segments, path[:next])
			path = path[next:]
		}
	}
	if len(segments) == 0 {
		return nil, fmt.Errorf("empty path")
	}
	return segments, nil
}

func setNestedField(object map[string]any, segments []string, value any) error {
	current := object
	for i, segment := range segments {
		if i == len(segments)-1 {
			current[segment] = value
			return nil
		}
		next, ok := current[segment]
		if !ok {
			child := map[string]any{}
			current[segment] = child
			current = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("segment %q is not an object", segment)
		}
		current = child
	}
	return nil
}
