/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"net/url"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const remoteKubeconfigName = "remote"
const remoteOperationTimeout = 30 * time.Second

// remoteTargetIdentity identifies the trusted endpoint without retaining CA material.
type remoteTargetIdentity struct {
	Server      string
	TrustSHA256 [sha256.Size]byte
}

// parseRemoteClusterConfig constructs restricted, in-memory settings for remote API clients.
// Helm consumers require an embedded CA; ordinary probes may use system roots.
func parseRemoteClusterConfig(data []byte, namespace string) (*rest.Config, remoteTargetIdentity, error) {
	parsed, err := parseProbeConfig(data)
	if err != nil {
		return nil, remoteTargetIdentity{}, errInvalidKubeconfig
	}
	if len(parsed.caData) == 0 || namespace == "" {
		return nil, remoteTargetIdentity{}, errInvalidKubeconfig
	}
	identity := remoteTargetIdentity{Server: parsed.server, TrustSHA256: sha256.Sum256(append(bytesWithSeparator(parsed.caData), []byte(parsed.serverName)...))}
	cfg := &rest.Config{
		Host:        parsed.server,
		BearerToken: parsed.token,
		TLSClientConfig: rest.TLSClientConfig{
			CAData:     append([]byte(nil), parsed.caData...),
			CertData:   append([]byte(nil), parsed.certData...),
			KeyData:    append([]byte(nil), parsed.keyData...),
			ServerName: parsed.serverName,
			Insecure:   false,
		},
		Timeout:       remoteOperationTimeout,
		Proxy:         func(*http.Request) (*url.URL, error) { return nil, nil },
		WrapTransport: func(rt http.RoundTripper) http.RoundTripper { return rejectRedirectsTransport{next: rt} },
	}
	return cfg, identity, nil
}

func bytesWithSeparator(data []byte) []byte {
	result := make([]byte, 0, len(data)+1)
	result = append(result, data...)
	return append(result, 0)
}

type rejectRedirectsTransport struct{ next http.RoundTripper }

func (t rejectRedirectsTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < 400 {
		_ = response.Body.Close()
		return nil, errors.New("remote API redirect rejected")
	}
	return response, nil
}

// remoteRESTClientGetter provides discovery and mapping without kubeconfig or disk cache access.
type remoteRESTClientGetter struct {
	config    *rest.Config
	loader    clientcmd.ClientConfig
	namespace string
}

func newRemoteRESTClientGetter(config *rest.Config, namespace string) (genericclioptions.RESTClientGetter, error) {
	if config == nil || namespace == "" {
		return nil, errInvalidKubeconfig
	}
	cc := &clientcmdapi.Config{
		CurrentContext: remoteKubeconfigName,
		Clusters:       map[string]*clientcmdapi.Cluster{remoteKubeconfigName: {Server: config.Host, CertificateAuthorityData: append([]byte(nil), config.CAData...), TLSServerName: config.ServerName}},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{remoteKubeconfigName: {Token: config.BearerToken, ClientCertificateData: append([]byte(nil), config.CertData...), ClientKeyData: append([]byte(nil), config.KeyData...)}},
		Contexts:       map[string]*clientcmdapi.Context{remoteKubeconfigName: {Cluster: remoteKubeconfigName, AuthInfo: remoteKubeconfigName, Namespace: namespace}},
	}
	loader := clientcmd.NewNonInteractiveClientConfig(*cc, remoteKubeconfigName, &clientcmd.ConfigOverrides{}, inMemoryConfigAccess{config: cc})
	return &remoteRESTClientGetter{config: rest.CopyConfig(config), loader: restrictedClientConfig{loader: loader, config: rest.CopyConfig(config), namespace: namespace}, namespace: namespace}, nil
}

// BoundNamespace exposes the namespace that callers must use for operations through this getter.
// Consumers should compare their requested operation namespace with this value before making requests.
func (g *remoteRESTClientGetter) BoundNamespace() string { return g.namespace }

func (g *remoteRESTClientGetter) ToRESTConfig() (*rest.Config, error) {
	return rest.CopyConfig(g.config), nil
}
func (g *remoteRESTClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	cfg, err := g.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	client, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(client), nil
}
func (g *remoteRESTClientGetter) ToRESTMapper() (meta.RESTMapper, error) {
	d, err := g.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	resources, err := restmapper.GetAPIGroupResources(d)
	if err != nil {
		return nil, err
	}
	return restmapper.NewDiscoveryRESTMapper(resources), nil
}
func (g *remoteRESTClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig { return g.loader }

type inMemoryConfigAccess struct{ config *clientcmdapi.Config }

// restrictedClientConfig prevents clientcmd from rebuilding a weaker config from kubeconfig fields.
// Raw config remains available for consumers that need the selected embedded credentials.
type restrictedClientConfig struct {
	loader    clientcmd.ClientConfig
	config    *rest.Config
	namespace string
}

func (c restrictedClientConfig) RawConfig() (clientcmdapi.Config, error) { return c.loader.RawConfig() }
func (c restrictedClientConfig) ClientConfig() (*rest.Config, error) {
	return rest.CopyConfig(c.config), nil
}
func (c restrictedClientConfig) Namespace() (string, bool, error)     { return c.namespace, true, nil }
func (c restrictedClientConfig) ConfigAccess() clientcmd.ConfigAccess { return c.loader.ConfigAccess() }

func (a inMemoryConfigAccess) GetLoadingPrecedence() []string { return nil }
func (a inMemoryConfigAccess) GetStartingConfig() (*clientcmdapi.Config, error) {
	return a.config.DeepCopy(), nil
}
func (a inMemoryConfigAccess) GetDefaultFilename() string { return "" }
func (a inMemoryConfigAccess) IsExplicitFile() bool       { return false }
func (a inMemoryConfigAccess) GetExplicitFile() string    { return "" }
