package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	definitionAccessNamePrefix  = "releaseyard-definition-access-"
	definitionAccessLabel       = "platform.releaseyard.io/definition-access"
	definitionAccessManagedBy   = "dashboard"
	definitionAccessGroupAnno   = "platform.releaseyard.io/group"
	definitionAccessGroupMaxLen = 253
)

// definitionAccessRule grants one Kubernetes group read access to a set of
// ApplicationDefinitions. It is materialized as a ClusterRole with resourceNames
// plus a ClusterRoleBinding, so authorization stays pure Kubernetes RBAC.
type definitionAccessRule struct {
	Group       string   `json:"group"`
	Definitions []string `json:"definitions"`
}

type definitionAccessRequest struct {
	Group       string   `json:"group"`
	Definitions []string `json:"definitions"`
}

type definitionAccessResponse struct {
	Rules []definitionAccessRule `json:"rules"`
}

func definitionAccessName(group string) string {
	sum := sha256.Sum256([]byte(group))
	return definitionAccessNamePrefix + hex.EncodeToString(sum[:])[:10]
}

func (a *authHandler) handleDefinitionAccessAdmin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.getDefinitionAccess(w, r)
	case http.MethodPut:
		a.putDefinitionAccess(w, r)
	case http.MethodDelete:
		a.deleteDefinitionAccess(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (a *authHandler) getDefinitionAccess(w http.ResponseWriter, r *http.Request) {
	rules, err := listDefinitionAccess(r.Context(), a.k8s)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load definition access rules")
		return
	}
	writeJSON(w, http.StatusOK, definitionAccessResponse{Rules: rules})
}

func (a *authHandler) putDefinitionAccess(w http.ResponseWriter, r *http.Request) {
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
	var input definitionAccessRequest
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
	group := strings.TrimSpace(input.Group)
	if group == "" || len(group) > definitionAccessGroupMaxLen {
		writeError(w, http.StatusBadRequest, "A group name is required")
		return
	}
	definitions, err := normalizeDefinitions(input.Definitions)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := saveDefinitionAccess(r.Context(), a.k8s, group, definitions); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not save the definition access rule")
		return
	}
	rules, err := listDefinitionAccess(r.Context(), a.k8s)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load definition access rules")
		return
	}
	writeJSON(w, http.StatusOK, definitionAccessResponse{Rules: rules})
}

func (a *authHandler) deleteDefinitionAccess(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "Request origin is not allowed")
		return
	}
	group := strings.TrimSpace(r.URL.Query().Get("group"))
	if group == "" || len(group) > definitionAccessGroupMaxLen {
		writeError(w, http.StatusBadRequest, "A group name is required")
		return
	}
	if err := deleteDefinitionAccess(r.Context(), a.k8s, group); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not remove the definition access rule")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func normalizeDefinitions(definitions []string) ([]string, error) {
	if len(definitions) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(definitions))
	result := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		name := strings.TrimSpace(definition)
		if len(validation.IsDNS1123Subdomain(name)) != 0 {
			return nil, errors.New("definition names must be valid DNS subdomains")
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	slices.Sort(result)
	return result, nil
}

func saveDefinitionAccess(ctx context.Context, k8s client.Client, group string, definitions []string) error {
	if len(definitions) == 0 {
		return deleteDefinitionAccess(ctx, k8s, group)
	}
	name := definitionAccessName(group)
	labels := map[string]string{definitionAccessLabel: definitionAccessManagedBy}
	annotations := map[string]string{definitionAccessGroupAnno: group}

	role := &rbacv1.ClusterRole{}
	roleErr := k8s.Get(ctx, client.ObjectKey{Name: name}, role)
	if apierrors.IsNotFound(roleErr) {
		role = &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, Annotations: annotations},
			Rules:      definitionAccessRules(definitions),
		}
		if err := k8s.Create(ctx, role); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	} else if roleErr != nil {
		return roleErr
	} else {
		role.Labels, role.Annotations, role.Rules = labels, annotations, definitionAccessRules(definitions)
		if err := k8s.Update(ctx, role); err != nil {
			return err
		}
	}

	binding := &rbacv1.ClusterRoleBinding{}
	bindErr := k8s.Get(ctx, client.ObjectKey{Name: name}, binding)
	desired := rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, Annotations: annotations},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: rbacClusterRole, Name: name},
		Subjects:   []rbacv1.Subject{{Kind: rbacSubjectGroup, APIGroup: rbacv1.GroupName, Name: group}},
	}
	switch {
	case apierrors.IsNotFound(bindErr):
		if err := k8s.Create(ctx, &desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	case bindErr != nil:
		return bindErr
	default:
		binding.Labels, binding.Annotations, binding.RoleRef, binding.Subjects = desired.Labels, desired.Annotations, desired.RoleRef, desired.Subjects
		if err := k8s.Update(ctx, binding); err != nil {
			return err
		}
	}
	return nil
}

func definitionAccessRules(definitions []string) []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{{
		APIGroups:     []string{platformAPIGroup},
		Resources:     []string{"applicationdefinitions"},
		ResourceNames: definitions,
		Verbs:         []string{verbGet},
	}}
}

func deleteDefinitionAccess(ctx context.Context, k8s client.Client, group string) error {
	name := definitionAccessName(group)
	if err := k8s.Delete(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err := k8s.Delete(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func listDefinitionAccess(ctx context.Context, k8s client.Client) ([]definitionAccessRule, error) {
	roles := &rbacv1.ClusterRoleList{}
	if err := k8s.List(ctx, roles, client.MatchingLabels{definitionAccessLabel: definitionAccessManagedBy}); err != nil {
		return nil, err
	}
	rules := make([]definitionAccessRule, 0, len(roles.Items))
	for i := range roles.Items {
		role := &roles.Items[i]
		rule := definitionAccessRule{Group: role.Annotations[definitionAccessGroupAnno], Definitions: []string{}}
		if len(role.Rules) > 0 {
			rule.Definitions = append(rule.Definitions, role.Rules[0].ResourceNames...)
		}
		rules = append(rules, rule)
	}
	slices.SortFunc(rules, func(a, b definitionAccessRule) int { return strings.Compare(a.Group, b.Group) })
	return rules, nil
}
