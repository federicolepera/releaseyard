package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultIdentityProviderSecretName = "releaseyard-dashboard-identity-provider"
	identityProviderIssuerKey         = "issuer-url"
	identityProviderClientIDKey       = "client-id"
	identityProviderClientSecretKey   = "client-secret"
)

// identityProviderConfig is the OIDC configuration persisted for the dashboard.
type identityProviderConfig struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
}

type identityProviderRequest struct {
	IssuerURL    string `json:"issuerURL"`
	ClientID     string `json:"clientID"`
	ClientSecret string `json:"clientSecret"`
}

type identityProviderStatus struct {
	Enabled         bool   `json:"enabled"`
	IssuerURL       string `json:"issuerURL,omitempty"`
	ClientID        string `json:"clientID,omitempty"`
	RedirectURL     string `json:"redirectURL"`
	HasClientSecret bool   `json:"hasClientSecret"`
}

func resolveIdentityProviderSecretName(name string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	return defaultIdentityProviderSecretName
}

// loadIdentityProvider reads the stored OIDC configuration. The boolean reports
// whether a complete configuration is present.
func loadIdentityProvider(ctx context.Context, k8s client.Client, namespace, name string) (identityProviderConfig, bool, error) {
	secret := &corev1.Secret{}
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return identityProviderConfig{}, false, nil
		}
		return identityProviderConfig{}, false, err
	}
	config := identityProviderConfig{
		IssuerURL:    strings.TrimSpace(string(secret.Data[identityProviderIssuerKey])),
		ClientID:     strings.TrimSpace(string(secret.Data[identityProviderClientIDKey])),
		ClientSecret: string(secret.Data[identityProviderClientSecretKey]),
	}
	if config.IssuerURL == "" || config.ClientID == "" || config.ClientSecret == "" {
		return identityProviderConfig{}, false, nil
	}
	return config, true, nil
}

// saveIdentityProvider creates or updates the OIDC configuration Secret.
func saveIdentityProvider(ctx context.Context, k8s client.Client, namespace, name string, config identityProviderConfig) error {
	desired := map[string][]byte{
		identityProviderIssuerKey:       []byte(config.IssuerURL),
		identityProviderClientIDKey:     []byte(config.ClientID),
		identityProviderClientSecretKey: []byte(config.ClientSecret),
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	err := k8s.Get(ctx, key, secret)
	if apierrors.IsNotFound(err) {
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       desired,
		}
		if createErr := k8s.Create(ctx, secret); createErr == nil {
			return nil
		} else if !apierrors.IsAlreadyExists(createErr) {
			return createErr
		}
		if err := k8s.Get(ctx, key, secret); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Type = corev1.SecretTypeOpaque
	maps.Copy(secret.Data, desired)
	return k8s.Update(ctx, secret)
}

func deleteIdentityProvider(ctx context.Context, k8s client.Client, namespace, name string) error {
	err := k8s.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// buildOIDCRuntime validates the configuration and performs issuer discovery.
func buildOIDCRuntime(ctx context.Context, config identityProviderConfig, publicURL *url.URL) (*oidcRuntime, error) {
	if strings.TrimSpace(config.IssuerURL) == "" || strings.TrimSpace(config.ClientID) == "" || strings.TrimSpace(config.ClientSecret) == "" {
		return nil, errors.New("issuer URL, client ID, and client secret are required")
	}
	issuer, err := parseAbsoluteURL(config.IssuerURL)
	if err != nil {
		return nil, errors.New("issuer URL must be an absolute HTTP(S) URL without user info, query, or fragment")
	}
	if issuer.Scheme == "http" && !isDevelopmentHost(issuer.Hostname()) {
		return nil, errors.New("HTTP issuer is allowed only for localhost development hosts")
	}
	redirect := *publicURL
	redirect.Path = "/auth/callback"
	return discoverOIDCRuntime(ctx, issuer, &redirect, config.ClientID, config.ClientSecret)
}

// loadAndBuildIdentityProvider reads the stored configuration and rebuilds the
// OIDC runtime. It reports whether the runtime was built and whether a complete
// stored configuration was present, so callers can decide whether to retry.
func (a *authHandler) loadAndBuildIdentityProvider(ctx context.Context) (built, present bool) {
	if a.k8s == nil || a.identityProviderSecret == "" {
		return false, false
	}
	cfg, ok, err := loadIdentityProvider(ctx, a.k8s, a.namespace, a.identityProviderSecret)
	if err != nil || !ok {
		a.oidc.Store(nil)
		return false, false
	}
	rt, err := buildOIDCRuntime(ctx, cfg, a.publicURL)
	if err != nil {
		a.oidc.Store(nil)
		return false, true
	}
	a.oidc.Store(rt)
	return true, true
}

// retryIdentityProvider rebuilds the OIDC runtime in the background when the
// startup discovery failed, for example because the loopback sidecar or the
// identity provider was not yet reachable. Without it the stored configuration
// would stay inactive until an administrator saved it again.
func (a *authHandler) retryIdentityProvider(ctx context.Context) {
	if a.k8s == nil || a.identityProviderSecret == "" {
		return
	}
	go func() {
		deadline := time.Now().Add(identityProviderRetryWindow)
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(identityProviderRetryInterval):
			}
			if built, present := a.loadAndBuildIdentityProvider(ctx); built || !present {
				return
			}
		}
	}()
}

func (a *authHandler) callbackURL() string {
	if a.publicURL == nil {
		return ""
	}
	u := *a.publicURL
	u.Path = "/auth/callback"
	return u.String()
}

func (a *authHandler) writeAuthMethods(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"local":       a.localAvailable(),
		"oidc":        a.oidcAvailable(),
		"redirectURL": a.callbackURL(),
	})
}

func (a *authHandler) handleIdentityProviderAdmin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.getIdentityProvider(w, r)
	case http.MethodPut:
		a.putIdentityProvider(w, r)
	case http.MethodDelete:
		a.deleteIdentityProvider(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (a *authHandler) getIdentityProvider(w http.ResponseWriter, r *http.Request) {
	config, ok, err := loadIdentityProvider(r.Context(), a.k8s, a.namespace, a.identityProviderSecret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load identity provider configuration")
		return
	}
	status := identityProviderStatus{Enabled: ok, RedirectURL: a.callbackURL()}
	if ok {
		status.IssuerURL = config.IssuerURL
		status.ClientID = config.ClientID
		status.HasClientSecret = config.ClientSecret != ""
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *authHandler) putIdentityProvider(w http.ResponseWriter, r *http.Request) {
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
	var input identityProviderRequest
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

	existing, hasExisting, err := loadIdentityProvider(r.Context(), a.k8s, a.namespace, a.identityProviderSecret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load identity provider configuration")
		return
	}
	clientSecret := input.ClientSecret
	if clientSecret == "" {
		if !hasExisting {
			writeError(w, http.StatusBadRequest, "Client secret is required")
			return
		}
		clientSecret = existing.ClientSecret
	}
	config := identityProviderConfig{
		IssuerURL:    strings.TrimSpace(input.IssuerURL),
		ClientID:     strings.TrimSpace(input.ClientID),
		ClientSecret: clientSecret,
	}
	if config.IssuerURL == "" || config.ClientID == "" {
		writeError(w, http.StatusBadRequest, "Issuer URL and client ID are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	runtime, err := buildOIDCRuntime(ctx, config, a.publicURL)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := saveIdentityProvider(r.Context(), a.k8s, a.namespace, a.identityProviderSecret, config); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not save identity provider configuration")
		return
	}
	a.oidc.Store(runtime)
	writeJSON(w, http.StatusOK, identityProviderStatus{
		Enabled:         true,
		IssuerURL:       config.IssuerURL,
		ClientID:        config.ClientID,
		RedirectURL:     a.callbackURL(),
		HasClientSecret: true,
	})
}

func (a *authHandler) deleteIdentityProvider(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "Request origin is not allowed")
		return
	}
	if err := deleteIdentityProvider(r.Context(), a.k8s, a.namespace, a.identityProviderSecret); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not remove identity provider configuration")
		return
	}
	a.oidc.Store(nil)
	w.WriteHeader(http.StatusNoContent)
}
