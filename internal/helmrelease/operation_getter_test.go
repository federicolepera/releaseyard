package helmrelease

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type operationTestGetter struct {
	config    *rest.Config
	namespace string
	loader    clientcmd.ClientConfig
}

func (g *operationTestGetter) ToRESTConfig() (*rest.Config, error) {
	return rest.CopyConfig(g.config), nil
}
func (g *operationTestGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	return nil, nil
}
func (g *operationTestGetter) ToRESTMapper() (meta.RESTMapper, error) { return nil, nil }
func (g *operationTestGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return g.loader
}
func (g *operationTestGetter) BoundNamespace() string { return g.namespace }

func newOperationTestGetter(config *rest.Config) *operationTestGetter {
	loader := clientcmd.NewDefaultClientConfig(clientcmdapi.Config{}, &clientcmd.ConfigOverrides{})
	return &operationTestGetter{config: config, namespace: "bound", loader: loader}
}

func TestOperationGetterCancelsInFlightAndRejectsLaterRequests(t *testing.T) {
	started := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	operationCtx, cancelOperation := context.WithCancel(context.Background())
	defer cancelOperation()
	base := newOperationTestGetter(&rest.Config{Host: server.URL})
	getter, err := newOperationGetter(base, operationCtx)
	if err != nil {
		t.Fatal(err)
	}
	config, err := getter.ToRESTConfig()
	if err != nil {
		t.Fatal(err)
	}
	transport, err := rest.TransportFor(config)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transport}

	result := make(chan error, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		_, requestErr := client.Do(request)
		result <- requestErr
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach blocked handler")
	}
	cancelOperation()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("expected in-flight request cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request was not canceled")
	}

	request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	if _, err := client.Do(request); err == nil {
		t.Fatal("expected request after operation cancellation to be rejected")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected only the original request to reach the server, got %d", got)
	}
}

func TestOperationResponseBodyRemainsReadableUntilComplete(t *testing.T) {
	continueBody := make(chan struct{})
	var resumeOnce sync.Once
	resumeBody := func() { resumeOnce.Do(func() { close(continueBody) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("first-"))
		w.(http.Flusher).Flush()
		<-continueBody
		_, _ = w.Write([]byte("second"))
	}))
	defer server.Close()
	defer resumeBody()

	operationCtx := t.Context()
	client := operationHTTPClient(t, server.URL, operationCtx)
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if operationCtx.Err() != nil {
		t.Fatal("operation context was canceled when response headers arrived")
	}
	resumeBody()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading response body failed: %v", err)
	}
	if got, want := string(body), "first-second"; got != want {
		t.Fatalf("unexpected response body %q, want %q", got, want)
	}
	if operationCtx.Err() != nil {
		t.Fatal("reading response body canceled the operation context")
	}
	_ = response.Body.Close()
}

func TestOperationResponseBodyCancellationInterruptsRead(t *testing.T) {
	started := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	operationCtx, cancelOperation := context.WithCancel(context.Background())
	defer cancelOperation()
	client := operationHTTPClient(t, server.URL, operationCtx)
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	<-started

	readResult := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(response.Body)
		readResult <- err
	}()
	cancelOperation()
	select {
	case err := <-readResult:
		if err == nil {
			t.Fatal("expected cancellation to interrupt response body read")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("response body read remained blocked after operation cancellation")
	}
	_ = response.Body.Close()

	request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	if _, err := client.Do(request); err == nil {
		t.Fatal("expected request after operation cancellation to be rejected")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected only the original request to reach the server, got %d", got)
	}
}

func operationHTTPClient(t *testing.T, endpoint string, operationCtx context.Context) *http.Client {
	t.Helper()
	getter, err := newOperationGetter(newOperationTestGetter(&rest.Config{Host: endpoint}), operationCtx)
	if err != nil {
		t.Fatal(err)
	}
	config, err := getter.ToRESTConfig()
	if err != nil {
		t.Fatal(err)
	}
	transport, err := rest.TransportFor(config)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: transport}
}

func TestOperationGetterRestrictsDirectAndRawLoaderConfigs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	base := newOperationTestGetter(&rest.Config{Host: "http://127.0.0.1"})
	getter, err := newOperationGetter(base, ctx)
	if err != nil {
		t.Fatal(err)
	}

	configs := make([]*rest.Config, 0, 2)
	directConfig, err := getter.ToRESTConfig()
	if err != nil {
		t.Fatal(err)
	}
	configs = append(configs, directConfig)
	loaderConfig, err := getter.ToRawKubeConfigLoader().ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	configs = append(configs, loaderConfig)
	cancel()

	for _, config := range configs {
		transport, err := rest.TransportFor(config)
		if err != nil {
			t.Fatal(err)
		}
		request, _ := http.NewRequest(http.MethodPut, config.Host, nil)
		if _, err := transport.RoundTrip(request); err == nil {
			t.Fatal("expected canceled operation to reject config request")
		}
	}
	if getter.BoundNamespace() != base.namespace {
		t.Fatalf("namespace was not delegated: got %q", getter.BoundNamespace())
	}
}

func TestOperationGetterComposesBaseTransportAndPreservesConnectionSettings(t *testing.T) {
	var wrapped atomic.Int32
	var proxyCalled atomic.Int32
	proxy := func(*http.Request) (*url.URL, error) {
		proxyCalled.Add(1)
		return nil, nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, nil, "/elsewhere", http.StatusFound)
	}))
	defer server.Close()
	baseConfig := &rest.Config{
		Host:    server.URL,
		Proxy:   proxy,
		Timeout: 17 * time.Second,
		WrapTransport: func(next http.RoundTripper) http.RoundTripper {
			return operationTestRoundTripper{next: next, called: &wrapped, rejectRedirect: true}
		},
	}
	ctx := t.Context()
	getter, err := newOperationGetter(newOperationTestGetter(baseConfig), ctx)
	if err != nil {
		t.Fatal(err)
	}
	config, err := getter.ToRESTConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.Proxy == nil || config.Timeout != baseConfig.Timeout {
		t.Fatal("proxy or timeout configuration was not preserved")
	}
	transport, err := rest.TransportFor(config)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	_, requestErr := (&http.Client{Transport: transport}).Do(request)
	if wrapped.Load() != 1 {
		t.Fatalf("base transport wrapper was not composed (called %d times)", wrapped.Load())
	}
	if proxyCalled.Load() == 0 {
		t.Fatal("base no-proxy function was not preserved")
	}
	if requestErr == nil {
		t.Fatal("base reject-redirect transport policy was not preserved")
	}
}

type operationTestRoundTripper struct {
	next           http.RoundTripper
	called         *atomic.Int32
	rejectRedirect bool
}

func (rt operationTestRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	rt.called.Add(1)
	response, err := rt.next.RoundTrip(request)
	if err == nil && rt.rejectRedirect && response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
		_ = response.Body.Close()
		return nil, errors.New("redirect rejected by base transport policy")
	}
	return response, err
}
