package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	applicationAccessLabel     = "platform.releaseyard.io/application-access"
	applicationAccessManagedBy = "dashboard"
	applicationAccessGroupAnno = "platform.releaseyard.io/group"
	teamNamespaceLabel         = "platform.releaseyard.io/team"
	groupNameMaxLen            = 253
)

// applicationAccessRule grants one Kubernetes group edit access to a team
// namespace. It is materialized as a namespace (named from the group via the
// configured prefix) plus a RoleBinding to the Application editor ClusterRole,
// keeping authorization in pure Kubernetes RBAC.
type applicationAccessRule struct {
	Group     string `json:"group"`
	Namespace string `json:"namespace"`
}

type applicationAccessRequest struct {
	Group string `json:"group"`
}

type applicationAccessResponse struct {
	Rules []applicationAccessRule `json:"rules"`
}

// applicationBindingName mirrors the chart's naming convention: the editor
// binding is the editor ClusterRole name without its "-role" suffix.
func applicationBindingName(editorRole string) string {
	return strings.TrimSuffix(editorRole, "-role")
}

func (a *authHandler) handleApplicationAccessAdmin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.getApplicationAccess(w, r)
	case http.MethodPut:
		a.putApplicationAccess(w, r)
	case http.MethodDelete:
		a.deleteApplicationAccess(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (a *authHandler) getApplicationAccess(w http.ResponseWriter, r *http.Request) {
	rules, err := a.listApplicationAccess(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load application access rules")
		return
	}
	writeJSON(w, http.StatusOK, applicationAccessResponse{Rules: rules})
}

func (a *authHandler) putApplicationAccess(w http.ResponseWriter, r *http.Request) {
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
	var input applicationAccessRequest
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
	if group == "" || len(group) > groupNameMaxLen {
		writeError(w, http.StatusBadRequest, "A group name is required")
		return
	}
	if err := a.saveApplicationAccess(r.Context(), group); err != nil {
		if errors.Is(err, errInvalidTeamGroup) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "Could not save the application access rule")
		return
	}
	rules, err := a.listApplicationAccess(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load application access rules")
		return
	}
	writeJSON(w, http.StatusOK, applicationAccessResponse{Rules: rules})
}

func (a *authHandler) deleteApplicationAccess(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "Request origin is not allowed")
		return
	}
	group := strings.TrimSpace(r.URL.Query().Get("group"))
	if group == "" || len(group) > groupNameMaxLen {
		writeError(w, http.StatusBadRequest, "A group name is required")
		return
	}
	namespace := namespaceForGroup(a.teamNamespacePrefix, group)
	if namespace == "" {
		writeError(w, http.StatusBadRequest, errInvalidTeamGroup.Error())
		return
	}
	if err := a.deleteApplicationAccessRule(r.Context(), namespace); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not remove the application access rule")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var errInvalidTeamGroup = errors.New("group name cannot be mapped to a valid namespace")

// saveApplicationAccess ensures the team namespace exists and binds the group to
// the Application editor ClusterRole inside it. The namespace is never deleted
// when access is revoked, so existing Applications survive.
func (a *authHandler) saveApplicationAccess(ctx context.Context, group string) error {
	namespace := namespaceForGroup(a.teamNamespacePrefix, group)
	if namespace == "" {
		return errInvalidTeamGroup
	}
	if errs := validation.IsDNS1123Label(namespace); len(errs) != 0 {
		return errInvalidTeamGroup
	}

	ns := &corev1.Namespace{}
	labels := map[string]string{teamNamespaceLabel: "true"}
	getErr := a.k8s.Get(ctx, client.ObjectKey{Name: namespace}, ns)
	if apierrors.IsNotFound(getErr) {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: labels}}
		if err := a.k8s.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	} else if getErr != nil {
		return getErr
	}

	name := applicationBindingName(a.applicationEditorRole)
	binding := &rbacv1.RoleBinding{}
	desired := rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      map[string]string{applicationAccessLabel: applicationAccessManagedBy},
			Annotations: map[string]string{applicationAccessGroupAnno: group},
		},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: a.applicationEditorRole},
		Subjects: []rbacv1.Subject{{Kind: "Group", APIGroup: rbacv1.GroupName, Name: group}},
	}
	bindErr := a.k8s.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, binding)
	switch {
	case apierrors.IsNotFound(bindErr):
		if err := a.k8s.Create(ctx, &desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	case bindErr != nil:
		return bindErr
	default:
		binding.Labels, binding.Annotations, binding.RoleRef, binding.Subjects = desired.Labels, desired.Annotations, desired.RoleRef, desired.Subjects
		if err := a.k8s.Update(ctx, binding); err != nil {
			return err
		}
	}
	return nil
}

func (a *authHandler) deleteApplicationAccessRule(ctx context.Context, namespace string) error {
	name := applicationBindingName(a.applicationEditorRole)
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := a.k8s.Delete(ctx, binding); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// listApplicationAccess reports the team namespaces whose editor binding points
// at the configured Application editor ClusterRole.
func (a *authHandler) listApplicationAccess(ctx context.Context) ([]applicationAccessRule, error) {
	bindings := &rbacv1.RoleBindingList{}
	if err := a.k8s.List(ctx, bindings); err != nil {
		return nil, err
	}
	rules := make([]applicationAccessRule, 0)
	for i := range bindings.Items {
		binding := &bindings.Items[i]
		if binding.RoleRef.Name != a.applicationEditorRole || !strings.HasPrefix(binding.Namespace, a.teamNamespacePrefix) {
			continue
		}
		group := ""
		for _, subject := range binding.Subjects {
			if subject.Kind == "Group" {
				group = subject.Name
				break
			}
		}
		if group == "" {
			continue
		}
		rules = append(rules, applicationAccessRule{Group: group, Namespace: binding.Namespace})
	}
	slices.SortFunc(rules, func(a, b applicationAccessRule) int {
		if diff := strings.Compare(a.Group, b.Group); diff != 0 {
			return diff
		}
		return strings.Compare(a.Namespace, b.Namespace)
	})
	return rules, nil
}
