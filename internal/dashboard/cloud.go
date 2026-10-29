package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
)

func (s *server) listCloudTargets(w http.ResponseWriter, r *http.Request) {
	items := &platformv1alpha1.CloudTargetList{}
	if err := s.client.List(r.Context(), items); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load cloud targets")
		return
	}
	project, status, message := s.projectFromQuery(r)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	if project != nil {
		kept := items.Items[:0]
		for i := range items.Items {
			if project.Spec.Catalog.AllowsCloudTarget(items.Items[i].Name) {
				kept = append(kept, items.Items[i])
			}
		}
		items.Items = kept
	} else if _, ok := writeClientFromRequest(r); ok {
		allowed := s.allowedCatalogNames(r)
		kept := items.Items[:0]
		for i := range items.Items {
			if allowed.cloudTargets[items.Items[i].Name] {
				kept = append(kept, items.Items[i])
			}
		}
		items.Items = kept
	}
	if items.Items == nil {
		items.Items = []platformv1alpha1.CloudTarget{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *server) listResourceDefinitions(w http.ResponseWriter, r *http.Request) {
	items := &platformv1alpha1.ResourceDefinitionList{}
	if err := s.client.List(r.Context(), items); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load resource definitions")
		return
	}
	project, status, message := s.projectFromQuery(r)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	if project != nil {
		kept := items.Items[:0]
		for i := range items.Items {
			if project.Spec.Catalog.AllowsResourceDefinition(items.Items[i].Name) {
				kept = append(kept, items.Items[i])
			}
		}
		items.Items = kept
	} else if _, ok := writeClientFromRequest(r); ok {
		allowed := s.allowedCatalogNames(r)
		kept := items.Items[:0]
		for i := range items.Items {
			if allowed.resourceDefinitions[items.Items[i].Name] {
				kept = append(kept, items.Items[i])
			}
		}
		items.Items = kept
	}
	if items.Items == nil {
		items.Items = []platformv1alpha1.ResourceDefinition{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *server) listResources(w http.ResponseWriter, r *http.Request) {
	c := s.writeClient(r)
	items := &platformv1alpha1.ResourceList{}
	for _, namespace := range s.appNamespaces(r) {
		partial := &platformv1alpha1.ResourceList{}
		if err := c.List(r.Context(), partial, client.InNamespace(namespace)); err != nil {
			if apierrors.IsForbidden(err) {
				writeError(w, http.StatusForbidden, "You do not have permission to list cloud resources in this team")
			} else {
				writeError(w, http.StatusInternalServerError, "Could not load cloud resources")
			}
			return
		}
		items.Items = append(items.Items, partial.Items...)
	}
	if items.Items == nil {
		items.Items = []platformv1alpha1.Resource{}
	}
	writeJSON(w, http.StatusOK, items)
}

type createResourceRequest struct {
	Name           string          `json:"name"`
	DefinitionName string          `json:"definitionName"`
	TargetName     string          `json:"targetName"`
	Namespace      string          `json:"namespace"`
	Values         json.RawMessage `json:"values"`
}

func (s *server) createResource(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "Request origin is not allowed")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != mediaTypeJSON {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type application/json is required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input createResourceRequest
	if err := decoder.Decode(&input); err != nil {
		status := http.StatusBadRequest
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, "Request body is invalid")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "Request body must contain exactly one JSON object")
		return
	}
	if len(input.Name) == 0 || len(input.Name) > 53 || !applicationNamePattern.MatchString(input.Name) {
		writeError(w, http.StatusBadRequest, "Resource name must be a valid DNS label of at most 53 characters")
		return
	}
	if input.DefinitionName == "" || input.TargetName == "" {
		writeError(w, http.StatusBadRequest, "A resource definition and a cloud target are required")
		return
	}
	values, err := decodeResourceValues(input.Values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	definition := &platformv1alpha1.ResourceDefinition{}
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: input.DefinitionName}, definition); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "Selected resource definition does not exist")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not verify resource definition")
		}
		return
	}
	if err := validateResourceValues(definition, values); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	target := &platformv1alpha1.CloudTarget{}
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: input.TargetName}, target); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "Selected cloud target does not exist")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not verify cloud target")
		}
		return
	}

	namespace, ok := s.cloudNamespace(r, input.Namespace, target.Spec.ProviderConfigRef.Namespace)
	if !ok {
		writeError(w, http.StatusForbidden, "Select a team you belong to")
		return
	}

	resource := &platformv1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{Name: input.Name, Namespace: namespace},
		Spec: platformv1alpha1.ResourceSpec{
			DefinitionRef: platformv1alpha1.LocalReference{Name: definition.Name},
			TargetRef:     platformv1alpha1.LocalReference{Name: target.Name},
		},
	}
	if len(values) > 0 {
		raw, err := json.Marshal(values)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Could not encode resource values")
			return
		}
		resource.Spec.Values = &apiextensionsv1.JSON{Raw: raw}
	}

	if err := s.writeClient(r).Create(r.Context(), resource); err != nil {
		switch {
		case apierrors.IsAlreadyExists(err):
			writeError(w, http.StatusConflict, "A resource with that name already exists")
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, "You do not have permission to create this resource")
		default:
			writeError(w, http.StatusInternalServerError, "Could not create resource")
		}
		return
	}
	writeJSON(w, http.StatusCreated, resource)
}

func (s *server) deleteResource(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "Request origin is not allowed")
		return
	}
	name := r.PathValue("name")
	if len(name) == 0 || len(name) > 53 || !applicationNamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "Resource name must be a valid DNS label of at most 53 characters")
		return
	}
	namespace, found, err := s.resolveResourceNamespace(r, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load resource")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "Resource does not exist")
		return
	}
	resource := &platformv1alpha1.Resource{}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := s.writeClient(r).Get(r.Context(), key, resource); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "Resource does not exist")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not load resource")
		}
		return
	}
	if err := s.writeClient(r).Delete(r.Context(), resource); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "Resource does not exist")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not delete resource")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// cloudNamespace chooses the namespace for a new Resource. A team session uses
// one of its team namespaces; a platform administrator without a team may use
// the CloudTarget's ProviderConfig namespace when their RBAC allows it there.
func (s *server) cloudNamespace(r *http.Request, requested, targetNamespace string) (string, bool) {
	if requested != "" {
		return s.createNamespace(r, requested)
	}
	if namespace, ok := s.createNamespace(r, ""); ok {
		return namespace, true
	}
	if targetNamespace == "" {
		return "", false
	}
	impersonated, ok := writeClientFromRequest(r)
	if !ok {
		return "", false
	}
	attributes := authorizationv1.ResourceAttributes{
		Namespace: targetNamespace, Group: platformAPIGroup, Resource: resourceResources, Verb: verbCreate,
	}
	if selfSubjectAccessAllowed(r.Context(), impersonated, attributes) {
		return targetNamespace, true
	}
	return "", false
}

// resolveResourceNamespace finds the namespace of a Resource among the
// namespaces the caller may use.
func (s *server) resolveResourceNamespace(r *http.Request, name string) (string, bool, error) {
	for _, namespace := range s.appNamespaces(r) {
		resource := &platformv1alpha1.Resource{}
		err := s.writeClient(r).Get(r.Context(), types.NamespacedName{Namespace: namespace, Name: name}, resource)
		switch {
		case err == nil:
			return namespace, true, nil
		case apierrors.IsNotFound(err):
			continue
		default:
			return "", false, err
		}
	}
	return "", false, nil
}

func decodeResourceValues(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 || isJSONNull(raw) {
		return map[string]json.RawMessage{}, nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, errors.New("values must be a JSON object")
	}
	return values, nil
}

// validateResourceValues enforces the definition's explicit value surface: only
// declared paths are accepted, required paths must be present, and values must
// match their declared type and range.
func validateResourceValues(definition *platformv1alpha1.ResourceDefinition, values map[string]json.RawMessage) error {
	declared := make(map[string]platformv1alpha1.ResourceDefinitionValue, len(definition.Spec.ConfigurableValues))
	for _, value := range definition.Spec.ConfigurableValues {
		declared[value.Path] = value
	}
	leaves := make(map[string]struct{})
	for key, raw := range values {
		if err := collectValuePaths(key, raw, leaves); err != nil {
			return fmt.Errorf("value %q is not valid JSON", key)
		}
	}
	for path := range leaves {
		if _, ok := declared[path]; !ok {
			return fmt.Errorf("value %q is not declared by the resource definition", path)
		}
	}
	for _, value := range definition.Spec.ConfigurableValues {
		raw, present := resourceValueAt(values, value.Path)
		if !present || isJSONNull(raw) {
			if value.Required {
				return fmt.Errorf("value %q is required", value.Path)
			}
			continue
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return fmt.Errorf("value %q is not valid JSON", value.Path)
		}
		switch value.Type {
		case "string":
			if _, ok := decoded.(string); !ok {
				return fmt.Errorf("value %q must be a string", value.Path)
			}
		case "boolean":
			if _, ok := decoded.(bool); !ok {
				return fmt.Errorf("value %q must be a boolean", value.Path)
			}
		case "integer":
			number, ok := decoded.(float64)
			if !ok || math.Trunc(number) != number {
				return fmt.Errorf("value %q must be an integer", value.Path)
			}
			if value.Minimum != nil && int64(number) < *value.Minimum {
				return fmt.Errorf("value %q must be at least %d", value.Path, *value.Minimum)
			}
			if value.Maximum != nil && int64(number) > *value.Maximum {
				return fmt.Errorf("value %q must be at most %d", value.Path, *value.Maximum)
			}
		default:
			return fmt.Errorf("value %q has an unsupported type %q", value.Path, value.Type)
		}
	}
	return nil
}

// resourceValueAt resolves a dot-separated definition path inside the nested
// values object. Definitions expose leaves (string, integer, boolean), so the
// value is encoded back to JSON for type validation.
func resourceValueAt(values map[string]json.RawMessage, path string) (json.RawMessage, bool) {
	parts := strings.Split(path, ".")
	raw, ok := values[parts[0]]
	if !ok {
		return nil, false
	}
	if len(parts) == 1 {
		return raw, true
	}
	var current any
	if err := json.Unmarshal(raw, &current); err != nil {
		return nil, false
	}
	for _, part := range parts[1:] {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[part]
		if !ok {
			return nil, false
		}
	}
	encoded, err := json.Marshal(current)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

func collectValuePaths(prefix string, raw json.RawMessage, out map[string]struct{}) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	return collectValueLeaves(prefix, value, out)
}

func collectValueLeaves(prefix string, value any, out map[string]struct{}) error {
	object, ok := value.(map[string]any)
	if !ok || len(object) == 0 {
		out[prefix] = struct{}{}
		return nil
	}
	for key, child := range object {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if err := collectValueLeaves(path, child, out); err != nil {
			return err
		}
	}
	return nil
}

func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

type allowedCatalogNames struct {
	applicationDefinitions map[string]bool
	clusters               map[string]bool
	cloudTargets           map[string]bool
	resourceDefinitions    map[string]bool
}

func (s *server) allowedCatalogNames(r *http.Request) allowedCatalogNames {
	allowed := allowedCatalogNames{
		applicationDefinitions: map[string]bool{},
		clusters:               map[string]bool{},
		cloudTargets:           map[string]bool{},
		resourceDefinitions:    map[string]bool{},
	}
	namespaces := map[string]bool{}
	for _, namespace := range s.appNamespaces(r) {
		namespaces[namespace] = true
	}
	projects := &platformv1alpha1.ProjectList{}
	if err := s.client.List(r.Context(), projects); err != nil {
		return allowed
	}
	for i := range projects.Items {
		project := &projects.Items[i]
		if !namespaces[project.Spec.Namespace] {
			continue
		}
		for _, name := range project.Spec.Catalog.ApplicationDefinitions {
			allowed.applicationDefinitions[name] = true
		}
		for _, name := range project.Spec.Catalog.Clusters {
			allowed.clusters[name] = true
		}
		for _, name := range project.Spec.Catalog.CloudTargets {
			allowed.cloudTargets[name] = true
		}
		for _, name := range project.Spec.Catalog.ResourceDefinitions {
			allowed.resourceDefinitions[name] = true
		}
	}
	return allowed
}
