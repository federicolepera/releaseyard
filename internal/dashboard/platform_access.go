package dashboard

import (
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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	platformAccessNamePrefix    = "releaseyard-platform-access-"
	platformAccessLabel         = "platform.releaseyard.io/platform-access"
	platformAccessManagedBy     = "dashboard"
	platformAccessManagedByLbl  = "app.kubernetes.io/managed-by"
	platformAccessSubjectKind   = "platform.releaseyard.io/subject-kind"
	platformAccessSubjectName   = "platform.releaseyard.io/subject"
	platformAccessDefaultRole   = "releaseyard-platform-admin"
	platformAccessSubjectMaxLen = 253
)

type platformAdminRule struct {
	SubjectKind string      `json:"subjectKind"`
	Subject     string      `json:"subject"`
	Binding     string      `json:"binding"`
	Managed     bool        `json:"managed"`
	CreatedAt   metav1.Time `json:"createdAt,omitempty"`
}

type platformAdminResponse struct {
	Role  string              `json:"role"`
	Rules []platformAdminRule `json:"rules"`
}

type platformAdminRequest struct {
	SubjectKind string `json:"subjectKind"`
	Subject     string `json:"subject"`
}

func (a *authHandler) platformAdminRoleValue() string {
	if a.platformAdminRole == "" {
		return platformAccessDefaultRole
	}
	return a.platformAdminRole
}

func platformAccessName(kind, subject string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + subject))
	return platformAccessNamePrefix + hex.EncodeToString(sum[:])[:10]
}

func (a *authHandler) handlePlatformAccessAdmin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.getPlatformAccess(w, r)
	case http.MethodPut:
		a.putPlatformAccess(w, r)
	case http.MethodDelete:
		a.deletePlatformAccess(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (a *authHandler) getPlatformAccess(w http.ResponseWriter, r *http.Request) {
	c := a.writeClientForAccess(r)
	bindings := &rbacv1.ClusterRoleBindingList{}
	if err := c.List(r.Context(), bindings); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load platform administrators")
		return
	}
	role := a.platformAdminRoleValue()
	rules := make([]platformAdminRule, 0)
	for i := range bindings.Items {
		binding := &bindings.Items[i]
		if binding.RoleRef.Name != role {
			continue
		}
		kind, subject := "", ""
		for _, item := range binding.Subjects {
			if item.Kind == rbacv1.GroupKind || item.Kind == rbacv1.UserKind {
				kind, subject = item.Kind, item.Name
				break
			}
		}
		if subject == "" {
			continue
		}
		rules = append(rules, platformAdminRule{
			SubjectKind: kind,
			Subject:     subject,
			Binding:     binding.Name,
			Managed:     binding.Labels[platformAccessLabel] == platformAccessManagedBy,
			CreatedAt:   binding.CreationTimestamp,
		})
	}
	slices.SortFunc(rules, func(a, b platformAdminRule) int {
		if diff := strings.Compare(a.SubjectKind, b.SubjectKind); diff != 0 {
			return diff
		}
		return strings.Compare(a.Subject, b.Subject)
	})
	writeJSON(w, http.StatusOK, platformAdminResponse{Role: role, Rules: rules})
}

func (a *authHandler) putPlatformAccess(w http.ResponseWriter, r *http.Request) {
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
	var input platformAdminRequest
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
	kind := strings.TrimSpace(input.SubjectKind)
	if kind != rbacv1.GroupKind && kind != rbacv1.UserKind {
		writeError(w, http.StatusBadRequest, "Subject kind must be Group or User")
		return
	}
	subject := strings.TrimSpace(input.Subject)
	if subject == "" || len(subject) > platformAccessSubjectMaxLen {
		writeError(w, http.StatusBadRequest, "A subject name is required")
		return
	}
	if kind == rbacv1.GroupKind && len(validation.IsDNS1123Subdomain(subject)) != 0 {
		writeError(w, http.StatusBadRequest, "Group name is not valid")
		return
	}
	role := a.platformAdminRoleValue()
	desired := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: platformAccessName(kind, subject),
			Labels: map[string]string{
				platformAccessLabel:        platformAccessManagedBy,
				platformAccessManagedByLbl: "releaseyard",
			},
			Annotations: map[string]string{
				platformAccessSubjectKind: kind,
				platformAccessSubjectName: subject,
			},
		},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: rbacClusterRole, Name: role},
		Subjects: []rbacv1.Subject{{Kind: kind, APIGroup: rbacv1.GroupName, Name: subject}},
	}
	c := a.writeClientForAccess(r)
	existing := &rbacv1.ClusterRoleBinding{}
	err = c.Get(r.Context(), types.NamespacedName{Name: desired.Name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := c.Create(r.Context(), desired); err != nil {
			if apierrors.IsForbidden(err) {
				writeError(w, http.StatusForbidden, "You do not have permission to assign platform admin")
				return
			}
			writeError(w, http.StatusInternalServerError, "Could not assign platform admin")
			return
		}
	case err != nil:
		writeError(w, http.StatusInternalServerError, "Could not assign platform admin")
		return
	default:
		existing.Labels, existing.Annotations, existing.RoleRef, existing.Subjects = desired.Labels, desired.Annotations, desired.RoleRef, desired.Subjects
		if err := c.Update(r.Context(), existing); err != nil {
			writeError(w, http.StatusInternalServerError, "Could not update platform admin")
			return
		}
	}
	a.getPlatformAccess(w, r)
}

func (a *authHandler) deletePlatformAccess(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "Request origin is not allowed")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("binding"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "Binding is required")
		return
	}
	c := a.writeClientForAccess(r)
	binding := &rbacv1.ClusterRoleBinding{}
	if err := c.Get(r.Context(), types.NamespacedName{Name: name}, binding); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "Platform admin binding does not exist")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not load platform admin binding")
		}
		return
	}
	if binding.RoleRef.Name != a.platformAdminRoleValue() || binding.Labels[platformAccessLabel] != platformAccessManagedBy {
		writeError(w, http.StatusBadRequest, "Binding is not managed by Platform admins")
		return
	}
	if err := c.Delete(r.Context(), binding); err != nil {
		if apierrors.IsForbidden(err) {
			writeError(w, http.StatusForbidden, "You do not have permission to remove platform admin")
			return
		}
		writeError(w, http.StatusInternalServerError, "Could not remove platform admin")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
