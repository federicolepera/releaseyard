package controller

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"k8s.io/client-go/rest"
)

const (
	testRemoteNamespace  = "explicit-target"
	testRemoteServerName = "tls.example"
)

func TestRemoteClusterConfigAndGetter(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	configBytes := testKubeconfig(server)

	t.Run("oversized config rejected", func(t *testing.T) {
		if _, err := parseProbeConfig(make([]byte, maxProbeConfigSize+1)); err != errInvalidKubeconfig {
			t.Fatalf("parseProbeConfig error = %v, want invalid kubeconfig", err)
		}
	})

	config, identity, err := parseRemoteClusterConfig(configBytes, testRemoteNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Server != server.URL || identity.TrustSHA256 == ([32]byte{}) {
		t.Fatalf("unexpected target identity: %#v", identity)
	}
	config.ServerName = testRemoteServerName
	if config.Host != server.URL || config.Insecure || config.Timeout != remoteOperationTimeout {
		t.Fatalf("unexpected restricted REST config: %#v", config)
	}
	proxy, err := config.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "target"}})
	if err != nil || proxy != nil {
		t.Fatalf("proxy = %v, %v; want direct connection", proxy, err)
	}

	t.Setenv("KUBECONFIG", "/a/nonexistent/ambient/config")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	getter, err := newRemoteRESTClientGetter(config, testRemoteNamespace)
	if err != nil {
		t.Fatal(err)
	}
	loader := getter.ToRawKubeConfigLoader()
	ns, overridden, err := loader.Namespace()
	if err != nil || ns != testRemoteNamespace {
		t.Fatalf("namespace = %q, %v, %v", ns, overridden, err)
	}
	raw, err := loader.RawConfig()
	if err != nil {
		t.Fatal(err)
	}
	if raw.CurrentContext != "remote" || raw.Contexts["remote"].Namespace != testRemoteNamespace || raw.Clusters["remote"].Server != server.URL {
		t.Fatalf("raw loader did not use in-memory target: %#v", raw)
	}
	if raw.Clusters["remote"].TLSServerName != testRemoteServerName || len(raw.Clusters["remote"].CertificateAuthorityData) == 0 {
		t.Fatalf("raw config lost selected TLS settings: %#v", raw.Clusters["remote"])
	}
	if len(raw.AuthInfos["remote"].ClientCertificateData) != 0 || len(raw.AuthInfos["remote"].ClientKeyData) != 0 {
		t.Fatal("unexpected client certificate material in token fixture")
	}
	bound, ok := getter.(interface{ BoundNamespace() string })
	if !ok || bound.BoundNamespace() != testRemoteNamespace {
		got := "<unavailable>"
		if ok {
			got = bound.BoundNamespace()
		}
		t.Fatalf("BoundNamespace() = %q, want %s", got, testRemoteNamespace)
	}
	if requestedNamespace := "different-target"; requestedNamespace == bound.BoundNamespace() {
		t.Fatalf("mismatched operation namespace %q unexpectedly matched getter binding", requestedNamespace)
	}
	for _, entrypoint := range []struct {
		name string
		get  func() (*rest.Config, error)
	}{
		{name: "getter", get: getter.ToRESTConfig},
		{name: "raw loader", get: loader.ClientConfig},
	} {
		t.Run(entrypoint.name, func(t *testing.T) {
			assertRemoteRESTConfig(t, entrypoint.get, server.URL)
		})
	}
	_ = overridden // the namespace is supplied by the in-memory context, not a CLI override.
}

func assertRemoteRESTConfig(t *testing.T, get func() (*rest.Config, error), serverURL string) {
	t.Helper()
	restConfig, err := get()
	if err != nil {
		t.Fatal(err)
	}
	if restConfig.Timeout != remoteOperationTimeout || restConfig.ServerName != testRemoteServerName {
		t.Fatalf("timeout/server name = %v/%q", restConfig.Timeout, restConfig.ServerName)
	}
	proxy, err := restConfig.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "target"}})
	if err != nil || proxy != nil {
		t.Fatalf("proxy = %v, %v; want direct connection", proxy, err)
	}
	transport := restConfig.WrapTransport(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Body: http.NoBody}, nil
	}))
	response, err := transport.RoundTrip(httptest.NewRequest(http.MethodGet, serverURL, nil))
	if err == nil || response != nil {
		t.Fatalf("redirect returned response=%v error=%v", response, err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
