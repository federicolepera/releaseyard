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
	clusterAccessNamePrefix  = "releaseyard-cluster-access-"
	clusterAccessLabel       = "platform.releaseyard.io/cluster-access"
	clusterAccessManagedBy   = "dashboard"
	clusterAccessGroupAnno   = "platform.releaseyard.io/group"
	clusterAccessGroupMaxLen = 253
)

// clusterAccessRule grants one Kubernetes group read access to a set of target
// Clusters. It is materialized as a ClusterRole with resourceNames plus a
// ClusterRoleBinding, so a team can only target the clusters the administrator
// allowed, staying within pure Kubernetes RBAC.
type clusterAccessRule struct {
	Group    string   `json:"group"`
	Clusters []string `json:"clusters"`
}

type clusterAccessRequest struct {
	Group    string   `json:"group"`
	Clusters []string `json:"clusters"`
}

type clusterAccessResponse struct {
	Rules []clusterAccessRule `json:"rules"`
}

func clusterAccessName(group string) string {
	sum := sha256.Sum256([]byte(group))
	return clusterAccessNamePrefix + hex.EncodeToString(sum[:])[:10]
}

func (a *authHandler) handleClusterAccessAdmin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.getClusterAccess(w, r)
	case http.MethodPut:
		a.putClusterAccess(w, r)
	case http.MethodDelete:
		a.deleteClusterAccess(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (a *authHandler) getClusterAccess(w http.ResponseWriter, r *http.Request) {
	rules, err := listClusterAccess(r.Context(), a.k8s)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load cluster access rules")
		return
	}
	writeJSON(w, http.StatusOK, clusterAccessResponse{Rules: rules})
}

func (a *authHandler) putClusterAccess(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "Request origin is not allowed")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type application/json is required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input clusterAccessRequest
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
	if group == "" || len(group) > clusterAccessGroupMaxLen {
		writeError(w, http.StatusBadRequest, "A group name is required")
		return
	}
	clusters, err := normalizeClusterNames(input.Clusters)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := saveClusterAccess(r.Context(), a.k8s, group, clusters); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not save the cluster access rule")
		return
	}
	rules, err := listClusterAccess(r.Context(), a.k8s)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load cluster access rules")
		return
	}
	writeJSON(w, http.StatusOK, clusterAccessResponse{Rules: rules})
}

func (a *authHandler) deleteClusterAccess(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "Request origin is not allowed")
		return
	}
	group := strings.TrimSpace(r.URL.Query().Get("group"))
	if group == "" || len(group) > clusterAccessGroupMaxLen {
		writeError(w, http.StatusBadRequest, "A group name is required")
		return
	}
	if err := deleteClusterAccess(r.Context(), a.k8s, group); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not remove the cluster access rule")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func normalizeClusterNames(clusters []string) ([]string, error) {
	if len(clusters) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(clusters))
	result := make([]string, 0, len(clusters))
	for _, cluster := range clusters {
		name := strings.TrimSpace(cluster)
		if len(validation.IsDNS1123Subdomain(name)) != 0 {
			return nil, errors.New("cluster names must be valid DNS subdomains")
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

func saveClusterAccess(ctx context.Context, k8s client.Client, group string, clusters []string) error {
	if len(clusters) == 0 {
		return deleteClusterAccess(ctx, k8s, group)
	}
	name := clusterAccessName(group)
	labels := map[string]string{clusterAccessLabel: clusterAccessManagedBy}
	annotations := map[string]string{clusterAccessGroupAnno: group}

	role := &rbacv1.ClusterRole{}
	roleErr := k8s.Get(ctx, client.ObjectKey{Name: name}, role)
	if apierrors.IsNotFound(roleErr) {
		role = &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, Annotations: annotations},
			Rules:      clusterAccessRules(clusters),
		}
		if err := k8s.Create(ctx, role); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	} else if roleErr != nil {
		return roleErr
	} else {
		role.Labels, role.Annotations, role.Rules = labels, annotations, clusterAccessRules(clusters)
		if err := k8s.Update(ctx, role); err != nil {
			return err
		}
	}

	binding := &rbacv1.ClusterRoleBinding{}
	bindErr := k8s.Get(ctx, client.ObjectKey{Name: name}, binding)
	desired := rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, Annotations: annotations},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: "Group", APIGroup: rbacv1.GroupName, Name: group}},
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

func clusterAccessRules(clusters []string) []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{{
		APIGroups:     []string{"platform.releaseyard.io"},
		Resources:     []string{"clusters"},
		ResourceNames: clusters,
		Verbs:         []string{"get"},
	}}
}

func deleteClusterAccess(ctx context.Context, k8s client.Client, group string) error {
	name := clusterAccessName(group)
	if err := k8s.Delete(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err := k8s.Delete(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func listClusterAccess(ctx context.Context, k8s client.Client) ([]clusterAccessRule, error) {
	roles := &rbacv1.ClusterRoleList{}
	if err := k8s.List(ctx, roles, client.MatchingLabels{clusterAccessLabel: clusterAccessManagedBy}); err != nil {
		return nil, err
	}
	rules := make([]clusterAccessRule, 0, len(roles.Items))
	for i := range roles.Items {
		role := &roles.Items[i]
		rule := clusterAccessRule{Group: role.Annotations[clusterAccessGroupAnno], Clusters: []string{}}
		if len(role.Rules) > 0 {
			rule.Clusters = append(rule.Clusters, role.Rules[0].ResourceNames...)
		}
		rules = append(rules, rule)
	}
	slices.SortFunc(rules, func(a, b clusterAccessRule) int { return strings.Compare(a.Group, b.Group) })
	return rules, nil
}
