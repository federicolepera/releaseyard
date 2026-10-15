package dashboard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	"github.com/federicolepera/idp/internal/contract"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed web/*
var webFiles embed.FS

const maxRequestBody = 64 << 10

const (
	clusterEnvironmentLabel = platformv1alpha1.ClusterEnvironmentLabelKey
	maxEnvironmentTargets   = 32
)

var applicationNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

type server struct {
	client    client.Client
	namespace string
	static    http.Handler

	// targets caches read-only clients to managed clusters, keyed by cluster name.
	targets sync.Map
}

// NewHandler returns the dashboard HTTP handler. Resources managed by this
// handler are constrained to namespace, except for cluster-scoped Clusters.
func NewHandler(k8s client.Client, namespace string) http.Handler {
	staticFS, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic("dashboard embedded files are unavailable: " + err.Error())
	}
	s := &server{client: k8s, namespace: namespace, static: http.FileServer(http.FS(staticFS))}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/applicationdefinitions", s.listDefinitions)
	mux.HandleFunc("GET /api/applications", s.listApplications)
	mux.HandleFunc("GET /api/activity", s.listActivity)
	mux.HandleFunc("GET /api/clusters", s.listClusters)
	mux.HandleFunc("PATCH /api/clusters/{name}/environment", s.updateClusterEnvironment)
	mux.HandleFunc("GET /api/environments/{name}/targets", s.previewEnvironmentTargets)
	mux.HandleFunc("POST /api/applications", s.createApplication)
	mux.HandleFunc("PATCH /api/applications/{name}", s.updateApplicationValues)
	mux.HandleFunc("DELETE /api/applications/{name}", s.deleteApplication)
	mux.HandleFunc("GET /api/applications/{name}/resources", s.listApplicationResources)
	mux.HandleFunc("GET /api/applications/{name}/pods/{pod}/logs", s.applicationPodLogs)
	mux.HandleFunc("GET /api/applications/{name}/metrics", s.applicationPodMetrics)
	mux.HandleFunc("GET /api/applications/{name}/events", s.applicationResourceEvents)
	mux.Handle("/", s.static)
	return securityHeaders(mux)
}

// writeClient returns the client used for state-changing operations. When the
// request carries an impersonating client it is used, so the management
// cluster's RBAC authorizes the logged-in user; otherwise the dashboard's own
// ServiceAccount client is used (for example for the local administrator).
func (s *server) writeClient(r *http.Request) client.Client {
	if c, ok := writeClientFromRequest(r); ok {
		return c
	}
	return s.client
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *server) listDefinitions(w http.ResponseWriter, r *http.Request) {
	items := &platformv1alpha1.ApplicationDefinitionList{}
	if err := s.client.List(r.Context(), items); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load application definitions")
		return
	}
	items.Items = filterReadable(r, "platform.releaseyard.io", "applicationdefinitions", "", items.Items, func(item platformv1alpha1.ApplicationDefinition) string { return item.Name })
	if items.Items == nil {
		items.Items = []platformv1alpha1.ApplicationDefinition{}
	}
	writeJSON(w, http.StatusOK, items)
}

// filterReadable removes list items the viewer is not allowed to get, so the
// catalog and other reads reflect the management cluster's RBAC. A session
// without an impersonating client (the local administrator) sees everything.
func filterReadable[T any](r *http.Request, group, resource, namespace string, items []T, name func(T) string) []T {
	all, allowed := readableNames(r, group, resource, namespace)
	if all {
		return items
	}
	kept := make([]T, 0, len(items))
	for _, item := range items {
		if _, ok := allowed[name(item)]; ok {
			kept = append(kept, item)
		}
	}
	return kept
}

// readableNames reports whether the viewer may read every object of a resource
// type and, when not, the specific names they may get. It asks the management
// cluster with a SelfSubjectRulesReview as the impersonated user, so the answer
// reflects RBAC resourceNames governance and fails closed on error.
func readableNames(r *http.Request, group, resource, namespace string) (bool, map[string]struct{}) {
	impersonated, ok := writeClientFromRequest(r)
	if !ok {
		return true, nil
	}
	reviewNamespace := namespace
	if reviewNamespace == "" {
		// A SelfSubjectRulesReview must name a namespace. Cluster-scoped rules
		// are returned regardless of the namespace, so the default namespace is
		// enough to learn the viewer's cluster-wide access.
		reviewNamespace = metav1.NamespaceDefault
	}
	review := &authorizationv1.SelfSubjectRulesReview{
		Spec: authorizationv1.SelfSubjectRulesReviewSpec{Namespace: reviewNamespace},
	}
	if err := impersonated.Create(r.Context(), review); err != nil {
		return false, nil
	}
	allowed := make(map[string]struct{})
	for _, rule := range review.Status.ResourceRules {
		if !ruleIncludes(rule.Verbs, "get") || !ruleIncludes(rule.APIGroups, group) || !ruleIncludes(rule.Resources, resource) {
			continue
		}
		if len(rule.ResourceNames) == 0 {
			return true, nil
		}
		for _, name := range rule.ResourceNames {
			allowed[name] = struct{}{}
		}
	}
	return false, allowed
}

func ruleIncludes(values []string, want string) bool {
	return slices.Contains(values, want) || slices.Contains(values, "*")
}

// appNamespaces returns the namespaces the caller may use for Applications. The
// local administrator (no OIDC session) uses the fixed management namespace.
// An OIDC session uses its team namespaces, optionally narrowed to a single
// team selected through the authorized ?namespace= query parameter.
func (s *server) appNamespaces(r *http.Request) []string {
	session, ok := sessionFromContext(r.Context())
	if !ok || session.authMethod == authMethodLocal {
		return []string{s.namespace}
	}
	teams := teamsFromContext(r.Context())
	selected := strings.TrimSpace(r.URL.Query().Get("namespace"))
	if selected == "" {
		return teams
	}
	if slices.Contains(teams, selected) {
		return []string{selected}
	}
	return nil
}

// createNamespace resolves the namespace for a new Application. The local
// administrator always uses the management namespace; an OIDC session must
// select one of its team namespaces.
func (s *server) createNamespace(r *http.Request, requested string) (string, bool) {
	session, ok := sessionFromContext(r.Context())
	if !ok || session.authMethod == authMethodLocal {
		return s.namespace, true
	}
	teams := teamsFromContext(r.Context())
	requested = strings.TrimSpace(requested)
	if requested == "" {
		if len(teams) == 1 {
			return teams[0], true
		}
		return "", false
	}
	if slices.Contains(teams, requested) {
		return requested, true
	}
	return "", false
}

// resolveApplicationNamespace finds the namespace of an Application among the
// namespaces the caller may use. It returns the namespace and whether it was
// found.
func (s *server) resolveApplicationNamespace(r *http.Request, name string) (string, bool, error) {
	for _, namespace := range s.appNamespaces(r) {
		app := &platformv1alpha1.Application{}
		err := s.writeClient(r).Get(r.Context(), types.NamespacedName{Namespace: namespace, Name: name}, app)
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

func (s *server) listApplications(w http.ResponseWriter, r *http.Request) {
	c := s.writeClient(r)
	items := &platformv1alpha1.ApplicationList{}
	for _, namespace := range s.appNamespaces(r) {
		partial := &platformv1alpha1.ApplicationList{}
		if err := c.List(r.Context(), partial, client.InNamespace(namespace)); err != nil {
			writeError(w, http.StatusInternalServerError, "Could not load applications")
			return
		}
		items.Items = append(items.Items, partial.Items...)
	}
	if items.Items == nil {
		items.Items = []platformv1alpha1.Application{}
	}
	writeJSON(w, http.StatusOK, items)
}

// activityEntry is a safe projection of a Kubernetes Event for the per-object
// activity view. It never exposes raw event payloads beyond reason and message.
type activityEntry struct {
	Type      string      `json:"type"`
	Reason    string      `json:"reason"`
	Message   string      `json:"message"`
	Source    string      `json:"source,omitempty"`
	Count     int32       `json:"count"`
	FirstSeen metav1.Time `json:"firstSeen"`
	LastSeen  metav1.Time `json:"lastSeen"`
}

// listActivity returns the Kubernetes Events that describe a single object's
// history. It is level 1 of the audit model: the caller may read the object
// (verified with a SelfSubjectAccessReview as the impersonated user) and the
// activity is filtered to that exact object. Events are read with the dashboard
// ServiceAccount so visibility follows the object's read permission.
func (s *server) listActivity(w http.ResponseWriter, r *http.Request) {
	if kind := r.URL.Query().Get("kind"); kind != "Application" {
		writeError(w, http.StatusBadRequest, "Unsupported activity kind")
		return
	}
	name := r.URL.Query().Get("name")
	if len(name) == 0 || len(name) > 253 {
		writeError(w, http.StatusBadRequest, "Activity name is required")
		return
	}
	// The local administrator (no impersonating client) reads activity in the
	// fixed management namespace. OIDC sessions must be allowed to read the
	// application, and the activity is scoped to the team namespace that owns it.
	namespace := s.namespace
	if impersonated, ok := writeClientFromRequest(r); ok {
		allowed, found := false, false
		for _, candidate := range s.appNamespaces(r) {
			attributes := authorizationv1.ResourceAttributes{
				Group: "platform.releaseyard.io", Resource: "applications", Verb: "get",
				Namespace: candidate, Name: name,
			}
			if !selfSubjectAccessAllowed(r.Context(), impersonated, attributes) {
				continue
			}
			allowed = true
			namespace = candidate
			found = true
			break
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "You do not have permission to view this application")
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "Application does not exist")
			return
		}
	}
	events := &corev1.EventList{}
	if err := s.client.List(r.Context(), events, client.InNamespace(namespace)); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load activity")
		return
	}
	entries := make([]activityEntry, 0, len(events.Items))
	for i := range events.Items {
		event := &events.Items[i]
		if event.InvolvedObject.Kind != "Application" || event.InvolvedObject.Name != name {
			continue
		}
		last := event.LastTimestamp
		if event.EventTime.After(last.Time) {
			last = metav1.NewTime(event.EventTime.Time)
		}
		first := event.FirstTimestamp
		if first.IsZero() {
			first = last
		}
		entries = append(entries, activityEntry{
			Type: event.Type, Reason: event.Reason, Message: event.Message,
			Source: event.Source.Component, Count: event.Count, FirstSeen: first, LastSeen: last,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].LastSeen.After(entries[j].LastSeen.Time) })
	writeJSON(w, http.StatusOK, map[string]any{"items": entries})
}

func (s *server) listClusters(w http.ResponseWriter, r *http.Request) {
	items := &platformv1alpha1.ClusterList{}
	if err := s.client.List(r.Context(), items); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load clusters")
		return
	}
	items.Items = filterReadable(r, "platform.releaseyard.io", "clusters", "", items.Items, func(item platformv1alpha1.Cluster) string { return item.Name })
	if items.Items == nil {
		items.Items = []platformv1alpha1.Cluster{}
	}
	writeJSON(w, http.StatusOK, items)
}

type updateClusterEnvironmentRequest struct {
	UID             types.UID       `json:"uid"`
	ResourceVersion string          `json:"resourceVersion"`
	Environment     json.RawMessage `json:"environment"`
}

func (s *server) updateClusterEnvironment(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "Request origin is not allowed")
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "Cluster name is required")
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
	var input updateClusterEnvironmentRequest
	if err := decoder.Decode(&input); err != nil {
		writeJSONDecodeError(w, err)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSONDecodeError(w, err)
		return
	}
	if input.UID == "" || input.ResourceVersion == "" || len(input.Environment) == 0 {
		writeError(w, http.StatusBadRequest, "UID, resourceVersion, and environment are required")
		return
	}
	remove := bytes.Equal(bytes.TrimSpace(input.Environment), []byte("null"))
	environment := ""
	if !remove {
		if err := json.Unmarshal(input.Environment, &environment); err != nil || !validLabelValue(environment) {
			writeError(w, http.StatusBadRequest, "Environment must be a nonempty Kubernetes label value or null")
			return
		}
	}

	cluster := &platformv1alpha1.Cluster{}
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "Cluster does not exist")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not load cluster")
		}
		return
	}
	if cluster.UID != input.UID || cluster.ResourceVersion != input.ResourceVersion || cluster.DeletionTimestamp != nil {
		writeError(w, http.StatusConflict, "Cluster has changed or is being deleted")
		return
	}
	original := cluster.DeepCopy()
	if remove {
		delete(cluster.Labels, clusterEnvironmentLabel)
	} else {
		if cluster.Labels == nil {
			cluster.Labels = make(map[string]string)
		}
		cluster.Labels[clusterEnvironmentLabel] = environment
	}
	if err := s.writeClient(r).Patch(r.Context(), cluster, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) {
			writeError(w, http.StatusConflict, "Cluster was modified by another request")
		} else if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "Cluster does not exist")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not update cluster environment")
		}
		return
	}
	writeJSON(w, http.StatusOK, cluster)
}

type environmentTarget struct {
	Name      string    `json:"name"`
	UID       types.UID `json:"uid"`
	Connected bool      `json:"connected"`
	Deleting  bool      `json:"deleting"`
}

type environmentTargetsResponse struct {
	Environment string              `json:"environment"`
	Targets     []environmentTarget `json:"targets"`
	Digest      string              `json:"digest"`
}

func (s *server) previewEnvironmentTargets(w http.ResponseWriter, r *http.Request) {
	environment := r.PathValue("name")
	if !validLabelValue(environment) {
		writeError(w, http.StatusBadRequest, "Environment must be a nonempty Kubernetes label value")
		return
	}
	targets, digest, err := resolveEnvironmentTargets(r.Context(), s.client, environment)
	if err != nil {
		if errors.Is(err, errTooManyEnvironmentTargets) {
			writeError(w, http.StatusUnprocessableEntity, "Environment has too many cluster targets")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not load environment targets")
		}
		return
	}
	writeJSON(w, http.StatusOK, environmentTargetsResponse{Environment: environment, Targets: targets, Digest: digest})
}

var errTooManyEnvironmentTargets = errors.New("too many environment targets")

// resolveEnvironmentTargets returns the current label membership and a stable
// digest suitable for future create-time membership revalidation.
func resolveEnvironmentTargets(ctx context.Context, k8s client.Client, environment string) ([]environmentTarget, string, error) {
	items := &platformv1alpha1.ClusterList{}
	if err := k8s.List(ctx, items, client.MatchingLabels{clusterEnvironmentLabel: environment}); err != nil {
		return nil, "", err
	}
	if len(items.Items) > maxEnvironmentTargets {
		return nil, "", errTooManyEnvironmentTargets
	}
	targets := make([]environmentTarget, 0, len(items.Items))
	for i := range items.Items {
		cluster := &items.Items[i]
		targets = append(targets, environmentTarget{
			Name: cluster.Name, UID: cluster.UID,
			Connected: clusterConnected(cluster),
			Deleting:  cluster.DeletionTimestamp != nil,
		})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	hash := sha256.New()
	writeDigestPart(hash, environment)
	for _, target := range targets {
		writeDigestPart(hash, target.Name)
		writeDigestPart(hash, string(target.UID))
	}
	return targets, hex.EncodeToString(hash.Sum(nil)), nil
}

func clusterConnected(cluster *platformv1alpha1.Cluster) bool {
	for _, condition := range cluster.Status.Conditions {
		if condition.Type == "Connected" {
			return condition.Status == metav1.ConditionTrue
		}
	}
	return false
}

func validLabelValue(value string) bool {
	return value != "" && len(validation.IsValidLabelValue(value)) == 0
}

func writeDigestPart(w io.Writer, value string) {
	_, _ = fmt.Fprintf(w, "%d:", len(value))
	_, _ = io.WriteString(w, value)
}

func writeJSONDecodeError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		status = http.StatusRequestEntityTooLarge
	}
	writeError(w, status, "Request body is invalid")
}

func (s *server) deleteApplication(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "Request origin is not allowed")
		return
	}
	name := r.PathValue("name")
	if len(name) == 0 || len(name) > 53 || !applicationNamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "Application name must be a valid DNS label of at most 53 characters")
		return
	}
	namespace, found, err := s.resolveApplicationNamespace(r, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load application")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "Application does not exist")
		return
	}
	app := &platformv1alpha1.Application{}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := s.writeClient(r).Get(r.Context(), key, app); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "Application does not exist")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not load application")
		}
		return
	}
	if err := s.writeClient(r).Delete(r.Context(), app); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "Application does not exist")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not delete application")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type updateApplicationValuesRequest struct {
	Generation int64           `json:"generation"`
	Values     json.RawMessage `json:"values"`
}

func (s *server) updateApplicationValues(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "Request origin is not allowed")
		return
	}
	name := r.PathValue("name")
	if len(name) == 0 || len(name) > 53 || !applicationNamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "Application name must be a valid DNS label of at most 53 characters")
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
	var input updateApplicationValuesRequest
	if err := decoder.Decode(&input); err != nil {
		status := http.StatusBadRequest
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, "Request body is invalid")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		status := http.StatusBadRequest
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, "Request body must contain exactly one JSON object")
		return
	}
	if input.Generation <= 0 || len(input.Values) == 0 {
		writeError(w, http.StatusBadRequest, "A positive generation and values are required")
		return
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(input.Values, &values); err != nil || values == nil {
		writeError(w, http.StatusBadRequest, "Values must be a JSON object")
		return
	}

	namespace, found, err := s.resolveApplicationNamespace(r, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load application")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "Application does not exist")
		return
	}
	app := &platformv1alpha1.Application{}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := s.writeClient(r).Get(r.Context(), key, app); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "Application does not exist")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not load application")
		}
		return
	}
	if app.Generation != input.Generation || app.DeletionTimestamp != nil {
		writeError(w, http.StatusConflict, "Application has changed or is being deleted")
		return
	}
	definition := &platformv1alpha1.ApplicationDefinition{}
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: app.Spec.ApplicationDefinitionRef.Name}, definition); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "Selected definition does not exist")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not verify definition")
		}
		return
	}
	if !isCurrentValidatedDefinition(definition) {
		writeError(w, http.StatusUnprocessableEntity, "The selected application definition is not validated for its current generation")
		return
	}
	prospective := app.DeepCopy()
	if len(values) == 0 {
		prospective.Spec.Values = nil
	} else {
		prospective.Spec.Values = &apiextensionsv1.JSON{Raw: bytes.Clone(input.Values)}
	}
	if err := contract.ValidateApplication(definition, prospective); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Values do not meet the application definition's requirements")
		return
	}
	app.Spec.Values = prospective.Spec.Values
	if err := s.writeClient(r).Update(r.Context(), app); err != nil {
		if apierrors.IsConflict(err) {
			writeError(w, http.StatusConflict, "Application was modified by another request")
		} else if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "Application does not exist")
		} else if apierrors.IsInvalid(err) {
			writeError(w, http.StatusUnprocessableEntity, "Values do not meet the application definition's requirements")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not update application")
		}
		return
	}
	writeJSON(w, http.StatusOK, app)
}

type createApplicationRequest struct {
	Name             string          `json:"name"`
	DefinitionName   string          `json:"definitionName"`
	ClusterName      string          `json:"clusterName"`
	EnvironmentName  string          `json:"environmentName"`
	Namespace        string          `json:"namespace"`
	PreviewDigest    string          `json:"previewDigest"`
	ReleaseNamespace string          `json:"releaseNamespace"`
	Values           json.RawMessage `json:"values"`
}

func (s *server) createApplication(w http.ResponseWriter, r *http.Request) {
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
	var input createApplicationRequest
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
		writeError(w, http.StatusBadRequest, "Application name must be a valid DNS label of at most 53 characters")
		return
	}
	if input.DefinitionName == "" || (input.ClusterName == "") == (input.EnvironmentName == "") {
		writeError(w, http.StatusBadRequest, "Specify a definition and exactly one target")
		return
	}
	environmentMode := input.EnvironmentName != ""
	if environmentMode && (!validLabelValue(input.EnvironmentName) || !validPreviewDigest(input.PreviewDigest)) {
		writeError(w, http.StatusBadRequest, "Environment and previewDigest are invalid")
		return
	}
	if len(input.Values) != 0 {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(input.Values, &object); err != nil || object == nil || string(input.Values) == "null" {
			writeError(w, http.StatusBadRequest, "Values must be a JSON object")
			return
		}
	}

	namespace, ok := s.createNamespace(r, input.Namespace)
	if !ok {
		writeError(w, http.StatusForbidden, "Select a team you belong to")
		return
	}
	definition := &platformv1alpha1.ApplicationDefinition{}
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: input.DefinitionName}, definition); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "Selected definition does not exist")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not verify definition")
		}
		return
	}
	if !isCurrentValidatedDefinition(definition) {
		writeError(w, http.StatusUnprocessableEntity, "The selected application definition is not validated for its current generation")
		return
	}
	app := &platformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: input.Name, Namespace: namespace},
		Spec: platformv1alpha1.ApplicationSpec{
			ApplicationDefinitionRef: platformv1alpha1.ApplicationReference{Name: definition.Name},
		},
	}
	if environmentMode {
		targets, digest, err := resolveEnvironmentTargets(r.Context(), s.client, input.EnvironmentName)
		if err != nil {
			if errors.Is(err, errTooManyEnvironmentTargets) {
				writeError(w, http.StatusUnprocessableEntity, "Environment has too many cluster targets")
			} else {
				writeError(w, http.StatusInternalServerError, "Could not load environment targets")
			}
			return
		}
		if digest != input.PreviewDigest {
			writeError(w, http.StatusConflict, "Environment targets have changed since preview")
			return
		}
		if len(targets) == 0 {
			writeError(w, http.StatusUnprocessableEntity, "Environment has no cluster targets")
			return
		}
		snapshot := make([]platformv1alpha1.ApplicationEnvironmentTarget, 0, len(targets))
		for _, target := range targets {
			if target.Deleting {
				writeError(w, http.StatusConflict, "An environment target is being deleted")
				return
			}
			if target.UID == "" {
				writeError(w, http.StatusUnprocessableEntity, "An environment target has no UID")
				return
			}
			snapshot = append(snapshot, platformv1alpha1.ApplicationEnvironmentTarget{Name: target.Name, UID: target.UID})
		}
		app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: input.EnvironmentName, Targets: snapshot}
	} else {
		cluster := &platformv1alpha1.Cluster{}
		if err := s.client.Get(r.Context(), types.NamespacedName{Name: input.ClusterName}, cluster); err != nil {
			if apierrors.IsNotFound(err) {
				writeError(w, http.StatusNotFound, "Selected cluster does not exist")
			} else {
				writeError(w, http.StatusInternalServerError, "Could not verify cluster")
			}
			return
		}
		app.Spec.ClusterRef = &platformv1alpha1.ApplicationReference{Name: cluster.Name}
	}
	if input.ReleaseNamespace != "" {
		app.Spec.ReleaseNamespace = &input.ReleaseNamespace
	}
	if len(input.Values) != 0 {
		app.Spec.Values = &apiextensionsv1.JSON{Raw: bytes.Clone(input.Values)}
	}
	if _, err := contract.EffectiveReleaseNamespace(definition, app); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Target namespace is not allowed or is invalid")
		return
	}
	if err := contract.ValidateApplication(definition, app); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Values do not meet the application definition's requirements")
		return
	}
	if err := s.writeClient(r).Create(r.Context(), app); err != nil {
		if apierrors.IsAlreadyExists(err) {
			writeError(w, http.StatusConflict, "An application with this name already exists")
		} else if apierrors.IsForbidden(err) {
			writeError(w, http.StatusForbidden, "You do not have permission to create applications in this team")
		} else if apierrors.IsInvalid(err) {
			writeError(w, http.StatusBadRequest, "Application is invalid")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not create application")
		}
		return
	}
	writeJSON(w, http.StatusCreated, app)
}

func validPreviewDigest(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}
	for _, char := range digest {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// isCurrentValidatedDefinition deliberately requires the condition itself and
// the aggregate status to describe the current object generation.
func isCurrentValidatedDefinition(definition *platformv1alpha1.ApplicationDefinition) bool {
	if definition.Status.ObservedGeneration != definition.Generation {
		return false
	}
	for _, condition := range definition.Status.Conditions {
		if condition.Type == "Validated" {
			return condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == definition.Generation
		}
	}
	return false
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.IsAbs() && u.Host != "" && strings.EqualFold(u.Host, r.Host) && (u.Scheme == "http" || u.Scheme == "https")
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
