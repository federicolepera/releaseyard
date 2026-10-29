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

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
)

const (
	projectAccessNamePrefix    = "releaseyard-project-access-"
	projectAccessLabel         = "platform.releaseyard.io/project-access"
	projectAccessManagedBy     = "dashboard"
	projectAccessProjectLabel  = "platform.releaseyard.io/project"
	projectAccessRoleLabel     = "platform.releaseyard.io/project-role"
	projectAccessSubjectKind   = "platform.releaseyard.io/subject-kind"
	projectAccessSubjectName   = "platform.releaseyard.io/subject"
	projectAccessManagedByLbl  = "app.kubernetes.io/managed-by"
	projectAccessDefaultPrefix = "releaseyard-project-"
	projectAccessSubjectMaxLen = 253
)

type projectAccessProject struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type projectAccessRole struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Name  string `json:"name"`
}

type projectAccessRule struct {
	Project     string `json:"project"`
	Namespace   string `json:"namespace"`
	Role        string `json:"role"`
	SubjectKind string `json:"subjectKind"`
	Subject     string `json:"subject"`
	Binding     string `json:"binding"`
}

type projectAccessResponse struct {
	Projects []projectAccessProject `json:"projects"`
	Roles    []projectAccessRole    `json:"roles"`
	Rules    []projectAccessRule    `json:"rules"`
}

type projectAccessRequest struct {
	Project     string `json:"project"`
	Role        string `json:"role"`
	SubjectKind string `json:"subjectKind"`
	Subject     string `json:"subject"`
}

func (a *authHandler) projectRolePrefixValue() string {
	if a.projectRolePrefix == "" {
		return projectAccessDefaultPrefix
	}
	return a.projectRolePrefix
}

func (a *authHandler) projectRoles() []projectAccessRole {
	prefix := a.projectRolePrefixValue()
	return []projectAccessRole{
		{Key: "viewer", Label: "Viewer", Name: prefix + "viewer"},
		{Key: "developer", Label: "Developer", Name: prefix + "developer"},
		{Key: "requester", Label: "Requester", Name: prefix + "requester"},
		{Key: "project-admin", Label: "Project admin", Name: prefix + "admin"},
	}
}

func (a *authHandler) projectRoleName(key string) (string, bool) {
	for _, role := range a.projectRoles() {
		if role.Key == key {
			return role.Name, true
		}
	}
	return "", false
}

func (a *authHandler) projectRoleKey(name string) (string, bool) {
	for _, role := range a.projectRoles() {
		if role.Name == name {
			return role.Key, true
		}
	}
	return "", false
}

func projectAccessName(project, role, kind, subject string) string {
	sum := sha256.Sum256([]byte(project + "\x00" + role + "\x00" + kind + "\x00" + subject))
	return projectAccessNamePrefix + hex.EncodeToString(sum[:])[:10]
}

func (a *authHandler) handleProjectAccessAdmin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.getProjectAccess(w, r)
	case http.MethodPut:
		a.putProjectAccess(w, r)
	case http.MethodDelete:
		a.deleteProjectAccess(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (a *authHandler) getProjectAccess(w http.ResponseWriter, r *http.Request) {
	projects, err := a.manageableProjects(r.Context(), r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load Project access")
		return
	}
	rules, err := a.listProjectAccess(r.Context(), r, projects)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load Project access")
		return
	}
	writeJSON(w, http.StatusOK, projectAccessResponse{Projects: projects, Roles: a.projectRoles(), Rules: rules})
}

func (a *authHandler) putProjectAccess(w http.ResponseWriter, r *http.Request) {
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
	var input projectAccessRequest
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
	project, roleName, kind, subject, status, message := a.validateProjectAccessRequest(r.Context(), r, input)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	if err := a.saveProjectAccess(r.Context(), r, project, roleName, input.Role, kind, subject); err != nil {
		if apierrors.IsForbidden(err) {
			writeError(w, http.StatusForbidden, "You do not have permission to assign Project roles")
			return
		}
		writeError(w, http.StatusInternalServerError, "Could not save Project access")
		return
	}
	a.getProjectAccess(w, r)
}

func (a *authHandler) deleteProjectAccess(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "Request origin is not allowed")
		return
	}
	projectName := strings.TrimSpace(r.URL.Query().Get("project"))
	bindingName := strings.TrimSpace(r.URL.Query().Get("binding"))
	if projectName == "" || bindingName == "" {
		writeError(w, http.StatusBadRequest, "Project and binding are required")
		return
	}
	project := &platformv1alpha1.Project{}
	if err := a.k8s.Get(r.Context(), types.NamespacedName{Name: projectName}, project); err != nil {
		writeError(w, http.StatusNotFound, "Project does not exist")
		return
	}
	if project.Spec.Namespace == "" || !a.canManageProjectAccess(r, project.Spec.Namespace) {
		writeError(w, http.StatusForbidden, "You do not have permission to manage this Project")
		return
	}
	binding := &rbacv1.RoleBinding{}
	c := a.writeClientForAccess(r)
	if err := c.Get(r.Context(), types.NamespacedName{Namespace: project.Spec.Namespace, Name: bindingName}, binding); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "Project access rule does not exist")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not load Project access rule")
		}
		return
	}
	if binding.Labels[projectAccessLabel] != projectAccessManagedBy || binding.Labels[projectAccessProjectLabel] != project.Name {
		writeError(w, http.StatusBadRequest, "Role binding is not managed by Project access")
		return
	}
	if _, ok := a.projectRoleKey(binding.RoleRef.Name); !ok {
		writeError(w, http.StatusBadRequest, "Role binding does not use a Project role")
		return
	}
	if err := c.Delete(r.Context(), binding); err != nil {
		if apierrors.IsForbidden(err) {
			writeError(w, http.StatusForbidden, "You do not have permission to remove Project roles")
			return
		}
		writeError(w, http.StatusInternalServerError, "Could not remove Project access rule")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *authHandler) validateProjectAccessRequest(ctx context.Context, r *http.Request, input projectAccessRequest) (*platformv1alpha1.Project, string, string, string, int, string) {
	project := &platformv1alpha1.Project{}
	if err := a.k8s.Get(ctx, types.NamespacedName{Name: strings.TrimSpace(input.Project)}, project); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", "", "", http.StatusNotFound, "Project does not exist"
		}
		return nil, "", "", "", http.StatusInternalServerError, projectVerifyError
	}
	if project.Spec.Namespace == "" {
		return nil, "", "", "", http.StatusUnprocessableEntity, "Project namespace is not configured"
	}
	if !a.canManageProjectAccess(r, project.Spec.Namespace) {
		return nil, "", "", "", http.StatusForbidden, "You do not have permission to manage this Project"
	}
	roleName, ok := a.projectRoleName(strings.TrimSpace(input.Role))
	if !ok {
		return nil, "", "", "", http.StatusBadRequest, "Select a valid Project role"
	}
	kind := strings.TrimSpace(input.SubjectKind)
	if kind != rbacv1.GroupKind && kind != rbacv1.UserKind {
		return nil, "", "", "", http.StatusBadRequest, "Subject kind must be Group or User"
	}
	subject := strings.TrimSpace(input.Subject)
	if subject == "" || len(subject) > projectAccessSubjectMaxLen {
		return nil, "", "", "", http.StatusBadRequest, "A subject name is required"
	}
	if kind == rbacv1.GroupKind && len(validation.IsDNS1123Subdomain(subject)) != 0 {
		return nil, "", "", "", http.StatusBadRequest, "Group name is not valid"
	}
	return project, roleName, kind, subject, 0, ""
}

func (a *authHandler) saveProjectAccess(ctx context.Context, r *http.Request, project *platformv1alpha1.Project, roleName, roleKey, kind, subject string) error {
	labels := map[string]string{
		projectAccessLabel:        projectAccessManagedBy,
		projectAccessProjectLabel: project.Name,
		projectAccessRoleLabel:    roleKey,
		projectAccessManagedByLbl: "releaseyard",
	}
	annotations := map[string]string{
		projectAccessSubjectKind: kind,
		projectAccessSubjectName: subject,
	}
	desired := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:        projectAccessName(project.Name, roleKey, kind, subject),
			Namespace:   project.Spec.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: rbacClusterRole, Name: roleName},
		Subjects: []rbacv1.Subject{{Kind: kind, APIGroup: rbacv1.GroupName, Name: subject}},
	}
	c := a.writeClientForAccess(r)
	existing := &rbacv1.RoleBinding{}
	err := c.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		return c.Create(ctx, desired)
	case err != nil:
		return err
	default:
		existing.Labels, existing.Annotations, existing.RoleRef, existing.Subjects = desired.Labels, desired.Annotations, desired.RoleRef, desired.Subjects
		return c.Update(ctx, existing)
	}
}

func (a *authHandler) manageableProjects(ctx context.Context, r *http.Request) ([]projectAccessProject, error) {
	projects := &platformv1alpha1.ProjectList{}
	if err := a.k8s.List(ctx, projects); err != nil {
		return nil, err
	}
	result := make([]projectAccessProject, 0, len(projects.Items))
	for i := range projects.Items {
		project := &projects.Items[i]
		if project.Spec.Namespace == "" || !project.DeletionTimestamp.IsZero() {
			continue
		}
		if !a.canManageProjectAccess(r, project.Spec.Namespace) {
			continue
		}
		result = append(result, projectAccessProject{Name: project.Name, Namespace: project.Spec.Namespace})
	}
	slices.SortFunc(result, func(a, b projectAccessProject) int { return strings.Compare(a.Name, b.Name) })
	return result, nil
}

func (a *authHandler) listProjectAccess(ctx context.Context, r *http.Request, projects []projectAccessProject) ([]projectAccessRule, error) {
	c := a.writeClientForAccess(r)
	rules := make([]projectAccessRule, 0)
	for _, project := range projects {
		bindings := &rbacv1.RoleBindingList{}
		if err := c.List(ctx, bindings, client.InNamespace(project.Namespace), client.MatchingLabels{projectAccessLabel: projectAccessManagedBy}); err != nil {
			if apierrors.IsForbidden(err) {
				continue
			}
			return nil, err
		}
		for i := range bindings.Items {
			binding := &bindings.Items[i]
			roleKey, ok := a.projectRoleKey(binding.RoleRef.Name)
			if !ok || binding.Labels[projectAccessProjectLabel] != project.Name {
				continue
			}
			kind := binding.Annotations[projectAccessSubjectKind]
			subject := binding.Annotations[projectAccessSubjectName]
			if kind == "" || subject == "" {
				for _, item := range binding.Subjects {
					kind, subject = item.Kind, item.Name
					break
				}
			}
			rules = append(rules, projectAccessRule{
				Project: project.Name, Namespace: project.Namespace, Role: roleKey,
				SubjectKind: kind, Subject: subject, Binding: binding.Name,
			})
		}
	}
	slices.SortFunc(rules, func(a, b projectAccessRule) int {
		if diff := strings.Compare(a.Project, b.Project); diff != 0 {
			return diff
		}
		if diff := strings.Compare(a.Role, b.Role); diff != 0 {
			return diff
		}
		return strings.Compare(a.Subject, b.Subject)
	})
	return rules, nil
}

func (a *authHandler) writeClientForAccess(r *http.Request) client.Client {
	if c, ok := writeClientFromRequest(r); ok {
		return c
	}
	return a.k8s
}

func (a *authHandler) canManageProjectAccess(r *http.Request, namespace string) bool {
	c, ok := writeClientFromRequest(r)
	if !ok {
		return true
	}
	attributes := authorizationv1.ResourceAttributes{
		Namespace: namespace,
		Group:     rbacv1.GroupName,
		Resource:  "rolebindings",
		Verb:      verbCreate,
	}
	return selfSubjectAccessAllowed(r.Context(), c, attributes)
}
