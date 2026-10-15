package dashboard

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	observabilityTimeout  = 30 * time.Second
	maxObservableResource = 200
	maxLogBytes           = 1 << 20
	maxLogTailLines       = 2000
	defaultLogTailLines   = 200
	metricsAPIGroupVer    = "metrics.k8s.io/v1beta1"
)

// targetConn holds read-only clients to a managed cluster, built from the same
// kubeconfig Secret the controller uses.
type targetConn struct {
	dynamic   dynamic.Interface
	core      kubernetes.Interface
	discovery discovery.DiscoveryInterface
	mapper    meta.RESTMapper
}

// targetFor returns a cached read-only connection to the cluster, building it on
// first use. The demo reuses the controller's kubeconfig (which is privileged);
// a production setup should mount a read-only credential instead.
func (s *server) targetFor(ctx context.Context, clusterName string) (*targetConn, error) {
	if cached, ok := s.targets.Load(clusterName); ok {
		return cached.(*targetConn), nil
	}
	cluster := &platformv1alpha1.Cluster{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: clusterName}, cluster); err != nil {
		return nil, err
	}
	ref := strings.TrimSpace(cluster.Spec.KubeconfigSecretRef.Name)
	if ref == "" {
		return nil, errors.New("cluster has no kubeconfig reference")
	}
	secret := &corev1.Secret{}
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: ref}, secret); err != nil {
		return nil, err
	}
	data := secret.Data["kubeconfig"]
	if len(data) == 0 {
		return nil, errors.New("cluster kubeconfig secret is empty")
	}
	config, err := clientcmd.RESTConfigFromKubeConfig(data)
	if err != nil {
		return nil, err
	}
	config.Timeout = observabilityTimeout
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	core, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	disc, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, err
	}
	groups, err := restmapper.GetAPIGroupResources(disc)
	if err != nil {
		return nil, err
	}
	conn := &targetConn{dynamic: dyn, core: core, discovery: disc, mapper: restmapper.NewDiscoveryRESTMapper(groups)}
	s.targets.Store(clusterName, conn)
	return conn, nil
}

// applicationScope is the target, namespace, release and observed resource
// identities for one Application.
type applicationScope struct {
	clusterName string
	namespace   string
	release     string
	resources   []platformv1alpha1.ApplicationResource
}

func applicationScopeOf(app *platformv1alpha1.Application) (applicationScope, error) {
	if app.Spec.ClusterRef != nil && app.Status.Binding != nil {
		return applicationScope{
			clusterName: app.Spec.ClusterRef.Name,
			namespace:   app.Status.Binding.ReleaseNamespace,
			release:     app.Status.Binding.ReleaseName,
			resources:   app.Status.Resources,
		}, nil
	}
	for i := range app.Status.Instances {
		instance := app.Status.Instances[i]
		if instance.Binding != nil {
			return applicationScope{
				clusterName: instance.ClusterName,
				namespace:   instance.Binding.ReleaseNamespace,
				release:     instance.Binding.ReleaseName,
				resources:   instance.Resources,
			}, nil
		}
	}
	return applicationScope{}, errors.New("application has no observed target yet")
}

func (s applicationScope) belongsTo(pod string) bool {
	for _, item := range s.resources {
		if item.Kind == "Pod" && item.Name == pod {
			return true
		}
	}
	return false
}

func (s applicationScope) podNames() []string {
	names := make([]string, 0)
	for _, item := range s.resources {
		if item.Kind == "Pod" && item.Name != "" {
			names = append(names, item.Name)
		}
	}
	sort.Strings(names)
	return names
}

// loadApplication validates the name, resolves its namespace with the
// impersonated RBAC and returns the Application. It writes the error response
// and returns false on failure.
func (s *server) loadApplication(w http.ResponseWriter, r *http.Request, name string) (*platformv1alpha1.Application, bool) {
	if len(name) == 0 || len(name) > 53 || !applicationNamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "Application name must be a valid DNS label of at most 53 characters")
		return nil, false
	}
	namespace, found, err := s.resolveApplicationNamespace(r, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load application")
		return nil, false
	}
	if !found {
		writeError(w, http.StatusNotFound, "Application does not exist")
		return nil, false
	}
	app := &platformv1alpha1.Application{}
	if err := s.writeClient(r).Get(r.Context(), types.NamespacedName{Namespace: namespace, Name: name}, app); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "Application does not exist")
		} else {
			writeError(w, http.StatusInternalServerError, "Could not load application")
		}
		return nil, false
	}
	return app, true
}

type applicationResourceView struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	YAML       string `json:"yaml,omitempty"`
	Error      string `json:"error,omitempty"`
}

type applicationResourcesResponse struct {
	ClusterName string                    `json:"clusterName"`
	Namespace   string                    `json:"namespace"`
	Release     string                    `json:"release"`
	Items       []applicationResourceView `json:"items"`
	Truncated   bool                      `json:"truncated,omitempty"`
}

// listApplicationResources returns the live YAML (including status) of every
// observed resource of the Application's Helm release. Secrets are never
// returned. The objects come from the target cluster, so their status is live.
func (s *server) listApplicationResources(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApplication(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	scope, err := applicationScopeOf(app)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "The application has no observed target yet")
		return
	}
	conn, err := s.targetFor(r.Context(), scope.clusterName)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Could not reach the target cluster")
		return
	}

	items := make([]applicationResourceView, 0, len(scope.resources))
	truncated := false
	for _, item := range scope.resources {
		if item.Kind == "Secret" {
			continue
		}
		if len(items) >= maxObservableResource {
			truncated = true
			break
		}
		view := applicationResourceView{APIVersion: item.APIVersion, Kind: item.Kind, Namespace: item.Namespace, Name: item.Name}
		gv, err := schema.ParseGroupVersion(item.APIVersion)
		if err != nil {
			view.Error = "Unsupported apiVersion"
			items = append(items, view)
			continue
		}
		mapping, err := conn.mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: item.Kind}, gv.Version)
		if err != nil {
			view.Error = "Unknown resource type on the target cluster"
			items = append(items, view)
			continue
		}
		ns := item.Namespace
		if ns == "" {
			ns = scope.namespace
		}
		object, err := conn.dynamic.Resource(mapping.Resource).Namespace(ns).Get(r.Context(), item.Name, metav1.GetOptions{})
		if err != nil {
			view.Error = "Could not read the resource from the target cluster"
			items = append(items, view)
			continue
		}
		if encoded, err := yaml.Marshal(object.Object); err == nil {
			view.YAML = string(encoded)
		}
		items = append(items, view)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].Name < items[j].Name
	})
	writeJSON(w, http.StatusOK, applicationResourcesResponse{
		ClusterName: scope.clusterName,
		Namespace:   scope.namespace,
		Release:     scope.release,
		Items:       items,
		Truncated:   truncated,
	})
}

type applicationLogsResponse struct {
	Pod       string `json:"pod"`
	Container string `json:"container,omitempty"`
	Logs      string `json:"logs"`
	Truncated bool   `json:"truncated,omitempty"`
}

// applicationPodLogs returns the log tail of a Pod that belongs to the
// Application's release.
func (s *server) applicationPodLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	pod := r.PathValue("pod")
	if len(pod) == 0 || len(pod) > 253 {
		writeError(w, http.StatusBadRequest, "Pod name is required")
		return
	}
	app, ok := s.loadApplication(w, r, name)
	if !ok {
		return
	}
	scope, err := applicationScopeOf(app)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "The application has no observed target yet")
		return
	}
	if !scope.belongsTo(pod) {
		writeError(w, http.StatusNotFound, "Pod does not belong to this application")
		return
	}
	conn, err := s.targetFor(r.Context(), scope.clusterName)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Could not reach the target cluster")
		return
	}

	tail := int64(defaultLogTailLines)
	if raw := r.URL.Query().Get("tailLines"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 {
			writeError(w, http.StatusBadRequest, "tailLines must be a positive integer")
			return
		}
		if parsed > maxLogTailLines {
			parsed = maxLogTailLines
		}
		tail = parsed
	}
	container := strings.TrimSpace(r.URL.Query().Get("container"))
	options := &corev1.PodLogOptions{TailLines: &tail}
	if container != "" {
		options.Container = container
	}
	stream, err := conn.core.CoreV1().Pods(scope.namespace).GetLogs(pod, options).Stream(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "Could not read Pod logs from the target cluster")
		return
	}
	defer func() { _ = stream.Close() }()
	body, err := io.ReadAll(io.LimitReader(stream, maxLogBytes))
	if err != nil {
		writeError(w, http.StatusBadGateway, "Could not read Pod logs from the target cluster")
		return
	}
	writeJSON(w, http.StatusOK, applicationLogsResponse{
		Pod:       pod,
		Container: container,
		Logs:      string(body),
		Truncated: int64(len(body)) >= maxLogBytes,
	})
}

type podUsage struct {
	Pod         string `json:"pod"`
	CPU         string `json:"cpu,omitempty"`
	Memory      string `json:"memory,omitempty"`
	CPUmilli    int64  `json:"cpuMilli"`
	MemoryBytes int64  `json:"memoryBytes"`
}

type applicationMetricsResponse struct {
	Available    bool       `json:"available"`
	Reason       string     `json:"reason,omitempty"`
	Pods         []podUsage `json:"pods,omitempty"`
	TotalCPUmil  int64      `json:"totalCpuMilli,omitempty"`
	TotalMemByte int64      `json:"totalMemoryBytes,omitempty"`
}

// applicationPodMetrics returns CPU/memory usage per Pod from the target
// cluster's metrics API. When metrics-server is not installed it reports that
// instead of failing.
func (s *server) applicationPodMetrics(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApplication(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	scope, err := applicationScopeOf(app)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "The application has no observed target yet")
		return
	}
	conn, err := s.targetFor(r.Context(), scope.clusterName)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Could not reach the target cluster")
		return
	}
	if _, err := conn.discovery.ServerResourcesForGroupVersion(metricsAPIGroupVer); err != nil {
		writeJSON(w, http.StatusOK, applicationMetricsResponse{Available: false, Reason: "Metrics are not available: metrics-server is not installed on the target cluster."})
		return
	}
	gvr := schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}
	list, err := conn.dynamic.Resource(gvr).Namespace(scope.namespace).List(r.Context(), metav1.ListOptions{})
	if err != nil {
		writeJSON(w, http.StatusOK, applicationMetricsResponse{Available: false, Reason: "Metrics are not available on the target cluster."})
		return
	}

	releasePods := make(map[string]struct{})
	for _, name := range scope.podNames() {
		releasePods[name] = struct{}{}
	}
	usage := make([]podUsage, 0, len(list.Items))
	var totalCPU, totalMem resource.Quantity
	for i := range list.Items {
		item := &list.Items[i]
		podName := item.GetName()
		if _, ok := releasePods[podName]; !ok {
			continue
		}
		cpu, memory := sumMetricsContainers(item)
		entry := podUsage{Pod: podName, CPUmilli: cpu.MilliValue(), MemoryBytes: memory.Value(), CPU: cpu.String(), Memory: memory.String()}
		totalCPU.Add(cpu)
		totalMem.Add(memory)
		usage = append(usage, entry)
	}
	sort.Slice(usage, func(i, j int) bool { return usage[i].Pod < usage[j].Pod })
	writeJSON(w, http.StatusOK, applicationMetricsResponse{
		Available:    true,
		Pods:         usage,
		TotalCPUmil:  totalCPU.MilliValue(),
		TotalMemByte: totalMem.Value(),
	})
}

func sumMetricsContainers(item *unstructured.Unstructured) (resource.Quantity, resource.Quantity) {
	var cpu, memory resource.Quantity
	containers, _, _ := unstructured.NestedSlice(item.Object, "containers")
	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		usage, _, _ := unstructured.NestedMap(container, "usage")
		if value, ok := usage["cpu"].(string); ok {
			if quantity, err := resource.ParseQuantity(value); err == nil {
				cpu.Add(quantity)
			}
		}
		if value, ok := usage["memory"].(string); ok {
			if quantity, err := resource.ParseQuantity(value); err == nil {
				memory.Add(quantity)
			}
		}
	}
	return cpu, memory
}
