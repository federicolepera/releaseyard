package helmrelease

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// operationGetter limits every client created from a REST getter to one operation's
// context. The supplied getter remains the source of all connection configuration.
type operationGetter struct {
	base         namespaceBoundGetter
	operationCtx context.Context
}

// newOperationGetter returns a REST getter whose requests are canceled with
// operationCtx. It rejects a nil getter or context instead of falling back to
// ambient client configuration.
func newOperationGetter(base namespaceBoundGetter, operationCtx context.Context) (namespaceBoundGetter, error) {
	if base == nil || (reflect.ValueOf(base).Kind() == reflect.Pointer && reflect.ValueOf(base).IsNil()) {
		return nil, errors.New("nil base REST getter")
	}
	if operationCtx == nil {
		return nil, errors.New("nil operation context")
	}
	return &operationGetter{base: base, operationCtx: operationCtx}, nil
}

func (g *operationGetter) BoundNamespace() string { return g.base.BoundNamespace() }

func (g *operationGetter) ToRESTConfig() (*rest.Config, error) {
	baseConfig, err := g.base.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	if baseConfig == nil {
		return nil, errors.New("base REST getter returned a nil config")
	}
	config := rest.CopyConfig(baseConfig)
	baseWrap := config.WrapTransport
	config.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		if baseWrap != nil {
			rt = baseWrap(rt)
		}
		return operationRoundTripper{next: rt, operationCtx: g.operationCtx}
	}
	return config, nil
}

func (g *operationGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	config, err := g.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	client, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(client), nil
}

func (g *operationGetter) ToRESTMapper() (meta.RESTMapper, error) {
	discoveryClient, err := g.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient), nil
}

func (g *operationGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return operationClientConfig{base: g.base.ToRawKubeConfigLoader(), getter: g}
}

// operationClientConfig preserves non-connection loader information while
// ensuring consumers that ask the loader for a REST config still receive the
// operation-bound configuration.
type operationClientConfig struct {
	base   clientcmd.ClientConfig
	getter *operationGetter
}

func (c operationClientConfig) RawConfig() (clientcmdapi.Config, error) {
	return c.base.RawConfig()
}

func (c operationClientConfig) ClientConfig() (*rest.Config, error) {
	return c.getter.ToRESTConfig()
}

func (c operationClientConfig) Namespace() (string, bool, error) {
	return c.base.Namespace()
}

func (c operationClientConfig) ConfigAccess() clientcmd.ConfigAccess {
	return c.base.ConfigAccess()
}

type operationRoundTripper struct {
	next         http.RoundTripper
	operationCtx context.Context
}

func (rt operationRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := rt.operationCtx.Err(); err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithCancel(request.Context())
	stop := context.AfterFunc(rt.operationCtx, cancel)
	if err := rt.operationCtx.Err(); err != nil {
		stop()
		cancel()
		return nil, err
	}
	response, err := rt.next.RoundTrip(request.WithContext(requestCtx))
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	if response == nil {
		stop()
		cancel()
		return nil, errors.New("transport returned a nil response")
	}
	if response.Body == nil {
		stop()
		cancel()
		return response, nil
	}
	response.Body = &operationResponseBody{
		ReadCloser: response.Body,
		stop:       stop,
		cancel:     cancel,
	}
	return response, nil
}

type operationResponseBody struct {
	io.ReadCloser
	stop   func() bool
	cancel context.CancelFunc
	once   sync.Once
}

func (b *operationResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.cleanup()
	}
	return n, err
}

func (b *operationResponseBody) Close() error {
	var err error
	b.once.Do(func() {
		b.stop()
		b.cancel()
		err = b.ReadCloser.Close()
	})
	return err
}

func (b *operationResponseBody) cleanup() {
	b.once.Do(func() {
		b.stop()
		b.cancel()
		_ = b.ReadCloser.Close()
	})
}
