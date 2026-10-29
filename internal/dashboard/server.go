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
	"strings"
	"sync"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	"github.com/federicolepera/idp/internal/contract"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

const (
	mediaTypeJSON        = "application/json"
	rbacClusterRole      = "ClusterRole"
	rbacSubjectGroup     = "Group"
	platformAPIGroup     = "platform.releaseyard.io"
	resourceApplications = "applications"
	resourceClusters     = "clusters"
	resourceResources    = "resources"
	verbGet              = "get"
	verbCreate           = "create"
	projectVerifyError   = "Could not verify Project"
	schemeHTTP           = "http"
	schemeHTTPS          = "https"
	authCallbackPath     = "/auth/callback"
	loginPath            = "/login"
	logoutURLField       = "logoutUrl"
	podKind              = "Pod"
	statusRunning        = "Running"
	statusPending        = "Pending"
	statusHealthy        = "Healthy"
	deliveryDirect       = "direct"
	deliveryGitOps       = "gitops"
	toneGood             = "good"
	toneMuted            = "muted"
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
	mux.HandleFunc("GET /api/projects", s.listProjects)
	mux.HandleFunc("GET /api/applications", s.listApplications)
	mux.HandleFunc("GET /api/activity", s.listActivity)
	mux.HandleFunc("GET /api/clusters", s.listClusters)
	mux.HandleFunc("PATCH /api/clusters/{name}/environment", s.updateClusterEnvironment)
	mux.HandleFunc("GET /api/cloudtargets", s.listCloudTargets)
	mux.HandleFunc("GET /api/resourcedefinitions", s.listResourceDefinitions)
	mux.HandleFunc("GET /api/resources", s.listResources)
	mux.HandleFunc("POST /api/resources", s.createResource)
	mux.HandleFunc("DELETE /api/resources/{name}", s.deleteResource)
	mux.HandleFunc("GET /api/environments/{name}/targets", s.previewEnvironmentTargets)
	mux.HandleFunc("POST /api/applications", s.createApplication)
	mux.HandleFunc("PATCH /api/applications/{name}", s.updateApplicationValues)
	mux.HandleFunc("PATCH /api/applications/{name}/placement", s.updateApplicationPlacement)
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

func (s *server) listProjects(w http.ResponseWriter, r *http.Request) {
	items := &platformv1alpha1.ProjectList{}
	if err := s.client.List(r.Context(), items); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load projects")
		return
	}
	if _, ok := writeClientFromRequest(r); ok {
		allowed := make(map[string]struct{})
		for _, namespace := range s.appNamespaces(r) {
			allowed[namespace] = struct{}{}
		}
		kept := items.Items[:0]
		for i := range items.Items {
			if _, ok := allowed[items.Items[i].Spec.Namespace]; ok {
				kept = append(kept, items.Items[i])
			}
		}
		items.Items = kept
	}
	if items.Items == nil {
		items.Items = []platformv1alpha1.Project{}
	}
	writeJSON(w, http.StatusOK, items)
}

// projectFromQuery resolves the Project catalog selected by the dashboard. It
// returns nil when no project parameter is present. Access follows the same
// Project namespace visibility as Applications.
func (s *server) projectFromQuery(r *http.Request) (*platformv1alpha1.Project, int, string) {
	name := strings.TrimSpace(r.URL.Query().Get("project"))
	if name == "" {
		return nil, 0, ""
	}
	project := &platformv1alpha1.Project{}
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name}, project); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, http.StatusNotFound, "Selected Project does not exist"
		}
		return nil, http.StatusInternalServerError, projectVerifyError
	}
	if project.Spec.Namespace == "" || !slices.Contains(s.appNamespaces(r), project.Spec.Namespace) {
		return nil, http.StatusForbidden, "You do not have access to this Project catalog"
	}
	return project, 0, ""
}

func filterCatalogDefinitions(items []platformv1alpha1.ApplicationDefinition, project *platformv1alpha1.Project) []platformv1alpha1.ApplicationDefinition {
	if project == nil {
		return items
	}
	kept := make([]platformv1alpha1.ApplicationDefinition, 0, len(items))
	for i := range items {
		if project.Spec.Catalog.AllowsApplicationDefinition(items[i].Name) {
			kept = append(kept, items[i])
		}
	}
	return kept
}

func filterCatalogClusters(items []platformv1alpha1.Cluster, project *platformv1alpha1.Project) []platformv1alpha1.Cluster {
	if project == nil {
		return items
	}
	kept := make([]platformv1alpha1.Cluster, 0, len(items))
	for i := range items {
		if project.Spec.Catalog.AllowsCluster(items[i].Name) {
			kept = append(kept, items[i])
		}
	}
	return kept
}

func (s *server) listDefinitions(w http.ResponseWriter, r *http.Request) {
	items := &platformv1alpha1.ApplicationDefinitionList{}
	if err := s.client.List(r.Context(), items); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load application definitions")
		return
	}
	project, status, message := s.projectFromQuery(r)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	items.Items = filterCatalogDefinitions(items.Items, project)
	items.Items = filterReadable(r, platformAPIGroup, "applicationdefinitions", "", items.Items, func(item platformv1alpha1.ApplicationDefinition) string { return item.Name })
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
		if !ruleIncludes(rule.Verbs, verbGet) || !ruleIncludes(rule.APIGroups, group) || !ruleIncludes(rule.Resources, resource) {
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
// local administrator (no OIDC session) can read every Project namespace, plus
// the management namespace for legacy Applications. An OIDC session uses its
// team namespaces and the Project namespaces where the management cluster lets
// it list Applications, optionally narrowed to one namespace through the
// authorized ?namespace= query parameter.
func (s *server) appNamespaces(r *http.Request) []string {
	session, ok := sessionFromContext(r.Context())
	if !ok || session.authMethod == authMethodLocal {
		return s.localAdminAppNamespaces(r.Context())
	}
	namespaces := append([]string(nil), teamsFromContext(r.Context())...)
	if impersonated, ok := writeClientFromRequest(r); ok {
		projects := &platformv1alpha1.ProjectList{}
		if err := s.client.List(r.Context(), projects); err == nil {
			for i := range projects.Items {
				namespace := projects.Items[i].Spec.Namespace
				if namespace == "" || slices.Contains(namespaces, namespace) {
					continue
				}
				attributes := authorizationv1.ResourceAttributes{
					Namespace: namespace, Group: platformAPIGroup, Resource: resourceApplications, Verb: "list",
				}
				if selfSubjectAccessAllowed(r.Context(), impersonated, attributes) {
					namespaces = append(namespaces, namespace)
				}
			}
		}
	}
	selected := strings.TrimSpace(r.URL.Query().Get("namespace"))
	if selected == "" {
		return namespaces
	}
	if slices.Contains(namespaces, selected) {
		return []string{selected}
	}
	return nil
}

func (s *server) localAdminAppNamespaces(ctx context.Context) []string {
	namespaces := []string{s.namespace}
	projects := &platformv1alpha1.ProjectList{}
	if err := s.client.List(ctx, projects); err != nil {
		return namespaces
	}
	for i := range projects.Items {
		namespace := projects.Items[i].Spec.Namespace
		if namespace == "" || slices.Contains(namespaces, namespace) {
			continue
		}
		namespaces = append(namespaces, namespace)
	}
	return namespaces
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
	// OIDC sessions must be allowed to read the application, and the activity is
	// scoped to the namespace that owns it. The local administrator is allowed to
	// read every Project namespace.
	namespace := s.namespace
	if impersonated, ok := writeClientFromRequest(r); ok {
		allowed, found := false, false
		for _, candidate := range s.appNamespaces(r) {
			attributes := authorizationv1.ResourceAttributes{
				Group: platformAPIGroup, Resource: resourceApplications, Verb: verbGet,
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
	} else {
		for _, candidate := range s.appNamespaces(r) {
			app := &platformv1alpha1.Application{}
			if err := s.client.Get(r.Context(), types.NamespacedName{Namespace: candidate, Name: name}, app); err == nil {
				namespace = candidate
				break
			}
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
	slices.SortFunc(entries, func(a, b activityEntry) int { return b.LastSeen.Compare(a.LastSeen.Time) })
	writeJSON(w, http.StatusOK, map[string]any{"items": entries})
}

func (s *server) listClusters(w http.ResponseWriter, r *http.Request) {
	items := &platformv1alpha1.ClusterList{}
	if err := s.client.List(r.Context(), items); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load clusters")
		return
	}
	project, status, message := s.projectFromQuery(r)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	items.Items = filterCatalogClusters(items.Items, project)
	items.Items = filterReadable(r, platformAPIGroup, resourceClusters, "", items.Items, func(item platformv1alpha1.Cluster) string { return item.Name })
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
	if err != nil || mediaType != mediaTypeJSON {
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
	slices.SortFunc(targets, func(a, b environmentTarget) int { return strings.Compare(a.Name, b.Name) })
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
	if err != nil || mediaType != mediaTypeJSON {
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
	session, _ := sessionFromContext(r.Context())
	recordChangeHistory(app, session.username, "Updated values")
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

type updateApplicationPlacementRequest struct {
	Generation int64                     `json:"generation"`
	Clusters   []applicationClusterInput `json:"clusters"`
}

//nolint:gocyclo // request validation and optimistic concurrency are intentionally explicit.
func (s *server) updateApplicationPlacement(w http.ResponseWriter, r *http.Request) {
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
	if err != nil || mediaType != mediaTypeJSON {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type application/json is required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input updateApplicationPlacementRequest
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
	if input.Generation <= 0 || len(input.Clusters) == 0 {
		writeError(w, http.StatusBadRequest, "A positive generation and at least one cluster are required")
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
	if app.Spec.ClusterRef != nil || app.Spec.Environment != nil || len(app.Spec.Clusters) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "Placement can only be edited for explicitly selected clusters")
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
	targets, status, message := s.resolveApplicationClusters(r.Context(), input.Clusters)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	oldUIDs := make(map[string]string, len(app.Spec.Clusters))
	for _, target := range app.Spec.Clusters {
		oldUIDs[target.Name] = target.UID
	}
	for _, target := range targets {
		if oldUID, exists := oldUIDs[target.Name]; exists && oldUID != target.UID {
			writeError(w, http.StatusUnprocessableEntity, "A selected cluster changed. Remove the old target before adding the replacement")
			return
		}
	}
	prospective := app.DeepCopy()
	prospective.Spec.Clusters = targets
	if err := contract.ValidateApplication(definition, prospective); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Selected clusters do not form a valid placement")
		return
	}
	app.Spec.Clusters = targets
	session, _ := sessionFromContext(r.Context())
	recordChangeHistory(app, session.username, "Updated placement")
	if err := s.writeClient(r).Update(r.Context(), app); err != nil {
		if apierrors.IsConflict(err) {
			writeError(w, http.StatusConflict, "Application was modified by another request")
		} else if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "Application does not exist")
		} else if apierrors.IsInvalid(err) {
			writeError(w, http.StatusUnprocessableEntity, "Selected clusters do not form a valid placement")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not update application placement")
		}
		return
	}
	writeJSON(w, http.StatusOK, app)
}

type createApplicationRequest struct {
	Name             string                    `json:"name"`
	Description      string                    `json:"description"`
	DefinitionName   string                    `json:"definitionName"`
	ClusterName      string                    `json:"clusterName"`
	Clusters         []applicationClusterInput `json:"clusters"`
	EnvironmentName  string                    `json:"environmentName"`
	Namespace        string                    `json:"namespace"`
	PreviewDigest    string                    `json:"previewDigest"`
	ReleaseNamespace string                    `json:"releaseNamespace"`
	Values           json.RawMessage           `json:"values"`
	ProjectName      string                    `json:"projectName"`
	Delivery         string                    `json:"delivery"`
}

type applicationClusterInput struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

func (s *server) resolveApplicationClusters(ctx context.Context, input []applicationClusterInput) ([]platformv1alpha1.ApplicationClusterTarget, int, string) {
	if len(input) == 0 {
		return nil, http.StatusBadRequest, "Select at least one cluster"
	}
	if len(input) > maxEnvironmentTargets {
		return nil, http.StatusBadRequest, "Select at most 32 clusters"
	}
	seenNames := make(map[string]struct{}, len(input))
	seenUIDs := make(map[types.UID]struct{}, len(input))
	targets := make([]platformv1alpha1.ApplicationClusterTarget, 0, len(input))
	for _, selected := range input {
		if len(validation.IsDNS1123Subdomain(selected.Name)) != 0 || selected.UID == "" || len(selected.UID) > 128 {
			return nil, http.StatusBadRequest, "Selected clusters are invalid"
		}
		if _, exists := seenNames[selected.Name]; exists {
			return nil, http.StatusBadRequest, "Selected clusters contain duplicates"
		}
		seenNames[selected.Name] = struct{}{}
		if _, exists := seenUIDs[selected.UID]; exists {
			return nil, http.StatusBadRequest, "Selected clusters contain duplicates"
		}
		seenUIDs[selected.UID] = struct{}{}
		cluster := &platformv1alpha1.Cluster{}
		if err := s.client.Get(ctx, types.NamespacedName{Name: selected.Name}, cluster); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, http.StatusNotFound, "A selected cluster does not exist"
			}
			return nil, http.StatusInternalServerError, "Could not verify selected clusters"
		}
		if cluster.UID != selected.UID {
			return nil, http.StatusConflict, "A selected cluster changed. Refresh the cluster list and try again"
		}
		if !cluster.DeletionTimestamp.IsZero() {
			return nil, http.StatusUnprocessableEntity, "A selected cluster is being deleted"
		}
		targets = append(targets, platformv1alpha1.ApplicationClusterTarget{Name: cluster.Name, UID: string(cluster.UID)})
	}
	return targets, 0, ""
}

//nolint:gocyclo // orchestration handler; splitting would obscure the sequential flow.
func (s *server) createApplication(w http.ResponseWriter, r *http.Request) {
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
	if len(input.Description) > 280 {
		writeError(w, http.StatusBadRequest, "Description must be at most 280 characters")
		return
	}
	targetModes := 0
	if input.ClusterName != "" {
		targetModes++
	}
	if len(input.Clusters) > 0 {
		targetModes++
	}
	if input.EnvironmentName != "" {
		targetModes++
	}
	if input.DefinitionName == "" || targetModes != 1 {
		writeError(w, http.StatusBadRequest, "Specify a definition and exactly one target")
		return
	}
	environmentMode := input.EnvironmentName != ""
	if environmentMode && !validLabelValue(input.EnvironmentName) {
		writeError(w, http.StatusBadRequest, "Environment is invalid")
		return
	}
	if len(input.Values) != 0 {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(input.Values, &object); err != nil || object == nil || string(input.Values) == "null" {
			writeError(w, http.StatusBadRequest, "Values must be a JSON object")
			return
		}
	}

	delivery, project, status, message := s.resolveDelivery(r.Context(), input.ProjectName, input.Delivery)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	if project == nil || project.Spec.Namespace == "" {
		writeError(w, http.StatusUnprocessableEntity, "Select a Project with a configured namespace")
		return
	}
	namespace := project.Spec.Namespace
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
			Description:              strings.TrimSpace(input.Description),
			ApplicationDefinitionRef: platformv1alpha1.ApplicationReference{Name: definition.Name},
		},
	}
	switch {
	case environmentMode:
		app.Spec.Environment = &platformv1alpha1.ApplicationEnvironmentSelection{Name: input.EnvironmentName}
	case len(input.Clusters) > 0:
		targets, status, message := s.resolveApplicationClusters(r.Context(), input.Clusters)
		if status != 0 {
			writeError(w, status, message)
			return
		}
		app.Spec.Clusters = targets
	default:
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
	if project != nil {
		if message := projectCatalogDenial(project, app.Spec); message != "" {
			writeError(w, http.StatusUnprocessableEntity, message)
			return
		}
	}
	session, _ := sessionFromContext(r.Context())
	if delivery == deliveryGitOps {
		request := &platformv1alpha1.ApplicationRequest{
			ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: namespace},
			Spec: platformv1alpha1.ApplicationRequestSpec{
				ProjectRef:           platformv1alpha1.ProjectRequestReference{Name: project.Name},
				ApplicationName:      app.Name,
				ApplicationNamespace: namespace,
				RequestedBy:          session.username,
				IdempotencyKey:       applicationRequestKey(app),
				Application:          app.Spec,
			},
		}
		if err := s.writeClient(r).Create(r.Context(), request); err != nil {
			if apierrors.IsAlreadyExists(err) {
				writeError(w, http.StatusConflict, "A GitOps request with this application name already exists")
			} else if apierrors.IsForbidden(err) {
				writeError(w, http.StatusForbidden, "You do not have permission to create GitOps requests")
			} else {
				writeError(w, http.StatusInternalServerError, "Could not create GitOps request")
			}
			return
		}
		writeJSON(w, http.StatusAccepted, request)
		return
	}
	recordChangeHistory(app, session.username, "Initial configuration")
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

func (s *server) resolveDelivery(ctx context.Context, projectName, requested string) (string, *platformv1alpha1.Project, int, string) {
	delivery := requested
	if delivery == "" {
		delivery = deliveryDirect
	}
	if delivery != deliveryDirect && delivery != deliveryGitOps {
		return "", nil, http.StatusBadRequest, "Delivery must be direct or gitops"
	}
	if projectName == "" {
		return "", nil, http.StatusUnprocessableEntity, "Select a Project for application delivery"
	}
	project := &platformv1alpha1.Project{}
	if err := s.client.Get(ctx, types.NamespacedName{Name: projectName}, project); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil, http.StatusNotFound, "Selected Project does not exist"
		}
		return "", nil, http.StatusInternalServerError, projectVerifyError
	}
	mode := project.Spec.Delivery.Mode
	if mode == "" {
		mode = platformv1alpha1.DeliveryModeDirect
	}
	if mode == platformv1alpha1.DeliveryModeDirect && delivery == deliveryGitOps {
		return "", nil, http.StatusUnprocessableEntity, "GitOps is disabled for this Project"
	}
	if mode == platformv1alpha1.DeliveryModeGitOps && delivery == deliveryDirect {
		return "", nil, http.StatusUnprocessableEntity, "This Project requires GitOps delivery"
	}
	if delivery == deliveryGitOps {
		if project.Spec.Namespace == "" {
			return "", nil, http.StatusUnprocessableEntity, "Project namespace is not configured"
		}
		if !meta.IsStatusConditionTrue(project.Status.Conditions, platformv1alpha1.ProjectConditionNamespaceReady) {
			return "", nil, http.StatusUnprocessableEntity, "Project namespace is not ready"
		}
		if project.Status.Repository == nil || !meta.IsStatusConditionTrue(project.Status.Conditions, platformv1alpha1.ProjectConditionRepositoryReady) {
			return "", nil, http.StatusUnprocessableEntity, "Project repository is not ready for GitOps"
		}
	}
	return delivery, project, 0, ""
}

func applicationRequestKey(app *platformv1alpha1.Application) string {
	payload, _ := json.Marshal(app.Spec)
	sum := sha256.Sum256([]byte(app.Namespace + "\x00" + app.Name + "\x00" + string(payload)))
	return hex.EncodeToString(sum[:])
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
	return err == nil && u.IsAbs() && u.Host != "" && strings.EqualFold(u.Host, r.Host) && (u.Scheme == schemeHTTP || u.Scheme == schemeHTTPS)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func projectCatalogDenial(project *platformv1alpha1.Project, application platformv1alpha1.ApplicationSpec) string {
	if !project.Spec.Catalog.AllowsApplicationDefinition(application.ApplicationDefinitionRef.Name) {
		return "Application definition is not in the Project catalog"
	}
	if application.ClusterRef != nil && !project.Spec.Catalog.AllowsCluster(application.ClusterRef.Name) {
		return "Target cluster is not in the Project catalog"
	}
	for _, target := range application.Clusters {
		if !project.Spec.Catalog.AllowsCluster(target.Name) {
			return "Target cluster is not in the Project catalog"
		}
	}
	if application.Environment != nil {
		for _, target := range application.Environment.Targets {
			if !project.Spec.Catalog.AllowsCluster(target.Name) {
				return "Environment target is not in the Project catalog"
			}
		}
	}
	return ""
}
