package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	loginBindingCookie = "dashboard-login-binding"
	sessionCookie      = "dashboard-session"

	loginTransactionTTL = 5 * time.Minute
	sessionIdleTTL      = 30 * time.Minute
	sessionAbsoluteTTL  = 8 * time.Hour

	// Startup discovery of the stored identity provider can race with the
	// loopback sidecar or the identity provider itself. Retry briefly so a
	// transient failure does not force the administrator to save it again.
	identityProviderRetryInterval = 5 * time.Second
	identityProviderRetryWindow   = 2 * time.Minute
)

// OIDCConfig contains the required OpenID Connect settings for the dashboard.
type OIDCConfig struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	// TeamNamespacePrefix derives per-team namespaces from identity-provider
	// groups for multi-team Application placement.
	TeamNamespacePrefix string
	// ApplicationEditorRole is the ClusterRole bound to a team group inside its
	// namespace by the "Access applications" administration panel.
	ApplicationEditorRole string
	// SessionStore persists sessions and login transactions. When nil an
	// in-memory store is used.
	SessionStore SessionStore
	// AuditLogger emits structured security audit events. When nil audit
	// logging is disabled.
	AuditLogger *slog.Logger
}

// LocalAuthConfig configures the dashboard authentication. HashPath points at
// the mounted local-administrator password-hash Secret key; PublicURL is the
// dashboard's canonical base URL; IdentityProviderSecretName optionally
// overrides the Secret that stores the OIDC configuration.
type LocalAuthConfig struct {
	HashPath                   string
	PublicURL                  string
	IdentityProviderSecretName string
	// TeamNamespacePrefix derives per-team namespaces from identity-provider
	// groups for multi-team Application placement.
	TeamNamespacePrefix string
	// ApplicationEditorRole is the ClusterRole bound to a team group inside its
	// namespace by the "Access applications" administration panel.
	ApplicationEditorRole string
	// ClientFactory builds a Kubernetes client that impersonates the logged-in
	// user, so that authorization is enforced by the management cluster's RBAC.
	ClientFactory ImpersonatingClientFactory
	// SessionStore persists sessions and login transactions. When nil an
	// in-memory store is used.
	SessionStore SessionStore
	// AuditLogger emits structured security audit events. When nil audit
	// logging is disabled.
	AuditLogger *slog.Logger
}

type loginTransaction struct {
	state    string
	binding  string
	nonce    string
	verifier string
	expires  time.Time
}

type authMethod string

const (
	authMethodLocal authMethod = "local"
	authMethodOIDC  authMethod = "oidc"
)

type dashboardSession struct {
	issuer       string
	subject      string
	username     string
	groups       []string
	csrfToken    string
	idToken      string
	lastActivity time.Time
	expires      time.Time
	authMethod   authMethod
}

type authHandler struct {
	localHashPath string
	publicURL     *url.URL
	secureCookie  bool
	now           func() time.Time

	k8s                    client.Client
	namespace              string
	teamNamespacePrefix    string
	applicationEditorRole  string
	identityProviderSecret string
	oidc                   atomic.Pointer[oidcRuntime]
	clientFactory          ImpersonatingClientFactory
	cloudProvisioning      bool
	store                  SessionStore
	auditLog               *slog.Logger

	clients sync.Map // identity key -> client.Client

	authzCache sync.Map           // authorization cache key -> cachedAuthorization
	authzGroup singleflight.Group // coalesces concurrent authorization checks
}

// oidcRuntime is the resolved OIDC configuration for the currently stored
// identity provider. It is replaced atomically when the configuration changes.
type oidcRuntime struct {
	provider           *oidc.Provider
	verifier           *oidc.IDTokenVerifier
	oauth              oauth2.Config
	issuer             string
	redirect           *url.URL
	endSessionEndpoint string
}

// NewOIDCHandler constructs the dashboard with a fixed OIDC provider. It fails
// if configuration is incomplete, unsafe for its URL scheme, or if issuer
// discovery cannot be completed. It is used by tests and by callers that want
// to pin the provider instead of loading it from a Secret.
func NewOIDCHandler(k8s client.Client, namespace string, config OIDCConfig) (http.Handler, error) {
	issuerURL, redirectURL, err := validateOIDCConfig(config)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rt, err := discoverOIDCRuntime(ctx, issuerURL, redirectURL, config.ClientID, config.ClientSecret)
	if err != nil {
		return nil, err
	}
	publicURL := *redirectURL
	publicURL.Path = ""
	publicURL.RawPath = ""
	auth := newAuthHandler(k8s, namespace, &publicURL, "", "", nil, config.TeamNamespacePrefix, config.ApplicationEditorRole, config.SessionStore)
	auth.auditLog = config.AuditLogger
	auth.oidc.Store(rt)
	return securityHeaders(auth.middleware(NewHandler(k8s, namespace))), nil
}

// NewLocalHandler constructs the dashboard. The built-in local administrator is
// always available; when a stored identity provider configuration exists it is
// loaded at startup so both login methods are offered. It fails only if the
// base URL is missing or unsafe.
func NewLocalHandler(k8s client.Client, namespace string, config LocalAuthConfig) (http.Handler, error) {
	publicURL, err := validateLocalAuthConfig(config)
	if err != nil {
		return nil, err
	}
	auth := newAuthHandler(k8s, namespace, publicURL, config.HashPath, resolveIdentityProviderSecretName(config.IdentityProviderSecretName), config.ClientFactory, config.TeamNamespacePrefix, config.ApplicationEditorRole, config.SessionStore)
	auth.auditLog = config.AuditLogger
	auth.cloudProvisioning = crossplaneInstalled(context.Background(), k8s)
	auth.reloadIdentityProvider(context.Background())
	auth.retryIdentityProvider(context.Background())
	return securityHeaders(auth.middleware(NewHandler(k8s, namespace))), nil
}

func newAuthHandler(k8s client.Client, namespace string, publicURL *url.URL, localHashPath, identityProviderSecret string, clientFactory ImpersonatingClientFactory, teamNamespacePrefix, applicationEditorRole string, store SessionStore) *authHandler {
	if store == nil {
		store = newMemorySessionStore()
	}
	return &authHandler{
		localHashPath:          localHashPath,
		publicURL:              publicURL,
		secureCookie:           publicURL.Scheme == schemeHTTPS,
		k8s:                    k8s,
		namespace:              namespace,
		teamNamespacePrefix:    teamNamespacePrefix,
		applicationEditorRole:  applicationEditorRole,
		identityProviderSecret: identityProviderSecret,
		clientFactory:          clientFactory,
		now:                    time.Now,
		store:                  store,
	}
}

// discoverOIDCRuntime performs issuer discovery and builds the runtime OIDC
// configuration used by login, callback, and logout.
func discoverOIDCRuntime(ctx context.Context, issuerURL, redirectURL *url.URL, clientID, clientSecret string) (*oidcRuntime, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ctx = oidc.ClientContext(ctx, &http.Client{Timeout: 10 * time.Second})
	provider, err := oidc.NewProvider(ctx, issuerURL.String())
	if err != nil {
		return nil, fmt.Errorf("discover OIDC issuer: %w", err)
	}

	// Extract end_session_endpoint from the OIDC discovery document so that
	// logout can terminate the upstream IdP session (RP-Initiated Logout).
	var providerClaims struct {
		EndSessionEndpoint string `json:"end_session_endpoint"`
	}
	_ = provider.Claims(&providerClaims)

	return &oidcRuntime{
		provider: provider,
		verifier: provider.Verifier(&oidc.Config{ClientID: clientID}),
		oauth: oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL.String(),
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID},
		},
		issuer:             issuerURL.String(),
		redirect:           redirectURL,
		endSessionEndpoint: providerClaims.EndSessionEndpoint,
	}, nil
}

func (a *authHandler) localAvailable() bool { return a.localHashPath != "" }
func (a *authHandler) oidcAvailable() bool  { return a.oidc.Load() != nil }

// reloadIdentityProvider loads the stored identity provider configuration and
// rebuilds the runtime provider. Errors leave OIDC disabled (local only).
func (a *authHandler) reloadIdentityProvider(ctx context.Context) {
	a.loadAndBuildIdentityProvider(ctx)
}

func validateLocalAuthConfig(config LocalAuthConfig) (*url.URL, error) {
	if strings.TrimSpace(config.HashPath) == "" {
		return nil, errors.New("local administrator hash path is required")
	}
	if strings.TrimSpace(config.PublicURL) == "" {
		return nil, errors.New("dashboard public URL is required")
	}
	publicURL, err := parseAbsoluteURL(config.PublicURL)
	if err != nil || publicURL.Path != "" {
		return nil, errors.New("dashboard public URL must be an absolute HTTP(S) URL without path, user info, query, or fragment")
	}
	if publicURL.Scheme == schemeHTTP && !isDevelopmentHost(publicURL.Hostname()) {
		return nil, errors.New("HTTP dashboard public URL is allowed only for localhost development hosts")
	}
	return publicURL, nil
}

func validateOIDCConfig(config OIDCConfig) (*url.URL, *url.URL, error) {
	if strings.TrimSpace(config.IssuerURL) == "" || strings.TrimSpace(config.ClientID) == "" || strings.TrimSpace(config.ClientSecret) == "" || strings.TrimSpace(config.RedirectURL) == "" {
		return nil, nil, errors.New("OIDC issuer, client ID, client secret, and redirect URL are required")
	}
	issuer, err := parseAbsoluteURL(config.IssuerURL)
	if err != nil {
		return nil, nil, errors.New("OIDC issuer URL must be an absolute HTTP(S) URL without user info, query, or fragment")
	}
	redirect, err := parseAbsoluteURL(config.RedirectURL)
	if err != nil || redirect.Path != authCallbackPath {
		return nil, nil, errors.New("OIDC redirect URL must be an absolute HTTP(S) URL ending in /auth/callback")
	}
	if issuer.Scheme == schemeHTTP && !isDevelopmentHost(issuer.Hostname()) {
		return nil, nil, errors.New("HTTP OIDC issuer is allowed only for localhost development hosts")
	}
	if redirect.Scheme == schemeHTTP && (!isDevelopmentHost(redirect.Hostname()) || issuer.Scheme != schemeHTTP || !isDevelopmentHost(issuer.Hostname())) {
		return nil, nil, errors.New("HTTP OIDC redirect is allowed only with localhost development issuer and redirect hosts")
	}
	return issuer, redirect, nil
}

func parseAbsoluteURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS) {
		return nil, errors.New("invalid absolute URL")
	}
	return u, nil
}

func isDevelopmentHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

//nolint:gocyclo // orchestration handler; splitting would obscure the sequential flow.
func (a *authHandler) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		var (
			auditWriter *auditResponseWriter
			session     dashboardSession
			hasSession  bool
		)
		if a.auditLog != nil {
			requestID := newAuditRequestID()
			r = r.WithContext(withAuditRequestID(r.Context(), requestID))
			if requestID != "" {
				w.Header().Set("X-Request-ID", requestID)
			}
			auditWriter = &auditResponseWriter{ResponseWriter: w, status: http.StatusOK}
			w = auditWriter
		}
		defer func() {
			if auditWriter != nil {
				a.auditHTTP(r, session, hasSession, auditWriter.status)
			}
		}()

		// Public discovery endpoint used by the login page.
		if r.Method == http.MethodGet && r.URL.Path == "/api/auth-methods" {
			if !a.canonicalHost(r) {
				writeError(w, http.StatusBadRequest, "Request host is not allowed")
				return
			}
			a.writeAuthMethods(w)
			return
		}

		// The login page and its assets are public. When only OIDC is
		// available the login page is skipped entirely.
		if r.Method == http.MethodGet && (r.URL.Path == loginPath || isLocalPublicAsset(r.URL.Path)) {
			if !a.canonicalHost(r) {
				writeError(w, http.StatusBadRequest, "Request host is not allowed")
				return
			}
			if !a.localAvailable() {
				http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
				return
			}
			if r.URL.Path == loginPath {
				http.Redirect(w, r, "/login.html", http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if r.Method == http.MethodGet && r.URL.Path == "/auth/login" {
			if !a.canonicalHost(r) {
				writeError(w, http.StatusBadRequest, "Request host is not allowed")
				return
			}
			if !a.oidcAvailable() {
				if a.localAvailable() {
					http.Redirect(w, r, loginPath, http.StatusSeeOther)
					return
				}
				writeError(w, http.StatusNotFound, "Authentication is not configured")
				return
			}
			a.login(w, r)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == authCallbackPath {
			if !a.canonicalHost(r) {
				writeError(w, http.StatusBadRequest, "Request host is not allowed")
				return
			}
			if !a.oidcAvailable() {
				http.Redirect(w, r, loginPath, http.StatusSeeOther)
				return
			}
			a.callback(w, r)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/auth/local" {
			if !a.canonicalHost(r) {
				writeError(w, http.StatusBadRequest, "Request host is not allowed")
				return
			}
			if !a.localAvailable() {
				writeError(w, http.StatusNotFound, "Local authentication is not available")
				return
			}
			a.localLogin(w, r)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/auth/logout" {
			a.logout(w, r)
			return
		}

		session, ok, err := a.sessionFromRequest(w, r)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "Session store is unavailable")
			return
		}
		if !ok {
			a.unauthorized(w, r)
			return
		}
		hasSession = true
		if !a.canonicalHost(r) {
			writeError(w, http.StatusBadRequest, "Request host is not allowed")
			return
		}
		if isStateChangingMethod(r.Method) && !a.validCSRF(r, session) {
			writeError(w, http.StatusForbidden, "CSRF validation failed")
			return
		}

		// Authorization is the management cluster's RBAC. The dashboard
		// impersonates the logged-in user for writes; reads (the catalog) are
		// allowed to any authenticated user and use the dashboard's own client.
		//
		// Resolving teams and capabilities requires several SelfSubjectAccessReview
		// calls. Do that only for API requests and the administration pages, and
		// cache the result: static assets and ordinary page HTML do not need it.
		needsAuthorization := isAPIPath(r.URL.Path) || isAdministrationPage(r.URL.Path)
		var impersonated client.Client
		if needsAuthorization {
			var err error
			impersonated, err = a.impersonatingClient(session)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "Could not assume user identity")
				return
			}
		}
		ctx := withSession(r.Context(), session)
		if impersonated != nil {
			ctx = withImpersonatingClient(ctx, impersonated)
		}
		var teams []string
		var capabilities dashboardCapabilities
		if needsAuthorization {
			// The administration area is authorized fresh; regular API requests
			// reuse the short-lived authorization cache.
			refresh := isAdministrationPage(r.URL.Path) || isAdministrationAPIPath(r.URL.Path)
			teams, capabilities = a.authorization(ctx, session, impersonated, refresh)
			ctx = withTeams(ctx, teams)
		}
		r = r.WithContext(ctx)

		if r.Method == http.MethodGet && r.URL.Path == "/api/session" {
			teamOptions := make([]map[string]string, 0, len(teams))
			for _, namespace := range teams {
				name := strings.TrimPrefix(namespace, a.teamNamespacePrefix)
				if name == "" {
					name = namespace
				}
				teamOptions = append(teamOptions, map[string]string{"name": name, "namespace": namespace})
			}
			writeJSON(w, http.StatusOK, map[string]any{"csrfToken": session.csrfToken, "username": session.username, "isAdmin": capabilities.isAdmin, "isLocalAdmin": session.authMethod == authMethodLocal, "canAdminister": capabilities.canAdminister, "canCreateApps": capabilities.canCreateApps, "canDeleteApps": capabilities.canDeleteApps, "canUpdateApps": capabilities.canUpdateApps, "canCreateResources": capabilities.canCreateResources, "canDeleteResources": capabilities.canDeleteResources, "cloudProvisioning": a.cloudProvisioning, "teams": teamOptions})
			return
		}
		if isAdministrationPage(r.URL.Path) || isAdministrationAPIPath(r.URL.Path) {
			// The administration area is available to the initial local
			// administrator and to OIDC users whose management-cluster RBAC
			// already allows the actions the panel performs.
			if !capabilities.canAdminister {
				writeError(w, http.StatusForbidden, "Administrator access is required")
				return
			}
		}
		if r.URL.Path == "/api/administration/identity-provider" {
			a.handleIdentityProviderAdmin(w, r)
			return
		}
		if r.URL.Path == "/api/administration/definition-access" {
			a.handleDefinitionAccessAdmin(w, r)
			return
		}
		if r.URL.Path == "/api/administration/cluster-access" {
			a.handleClusterAccessAdmin(w, r)
			return
		}
		if r.URL.Path == "/api/administration/application-access" {
			a.handleApplicationAccessAdmin(w, r)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// isLocalPublicAsset lists the static files the local login page needs before a
// session exists. app.js is deliberately excluded because it treats a missing
// session as a redirect to the login page.
func isLocalPublicAsset(path string) bool {
	switch path {
	case "/login.html", "/login.js", "/style.css", "/theme-init.js":
		return true
	}
	return false
}

func (a *authHandler) canonicalHost(r *http.Request) bool {
	return r.Host != "" && strings.EqualFold(r.Host, a.publicURL.Host)
}

func (a *authHandler) unauthorized(w http.ResponseWriter, r *http.Request) {
	if isAPIPath(r.URL.Path) || strings.HasPrefix(r.URL.Path, "/auth/") {
		writeError(w, http.StatusUnauthorized, "Authentication required")
		return
	}
	if a.localAvailable() {
		http.Redirect(w, r, loginPath, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
}

func isAPIPath(path string) bool {
	return path == "/api" || strings.HasPrefix(path, "/api/")
}

func isAdministrationAPIPath(path string) bool {
	return strings.HasPrefix(path, "/api/administration/")
}

func isAdministrationPage(path string) bool {
	switch path {
	case "/identity-provider.html", "/access-definitions.html", "/cluster-access.html", "/access-applications.html":
		return true
	}
	return false
}

func isApplicationItemPath(path string) bool {
	const prefix = "/api/applications/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	name := strings.TrimPrefix(path, prefix)
	return name != "" && !strings.Contains(name, "/")
}

func isClusterEnvironmentPath(path string) bool {
	parts := strings.Split(path, "/")
	return len(parts) == 5 && parts[1] == "api" && parts[2] == resourceClusters && parts[3] != "" && parts[4] == "environment"
}

func isStateChangingMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func (a *authHandler) login(w http.ResponseWriter, r *http.Request) {
	runtime := a.oidc.Load()
	if runtime == nil {
		a.audit(r, auditEvent{Event: auditEventOIDCLogin, Outcome: auditOutcomeFailure, AuthMethod: authMethodOIDC, Reason: "identity_provider_unavailable", Status: http.StatusServiceUnavailable})
		writeError(w, http.StatusServiceUnavailable, "Identity provider is not available")
		return
	}
	binding, err := randomOpaque()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not start authentication")
		return
	}
	state, err := randomOpaque()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not start authentication")
		return
	}
	nonce, err := randomOpaque()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not start authentication")
		return
	}
	verifier := oauth2.GenerateVerifier()
	transaction := loginTransaction{state: state, binding: binding, nonce: nonce, verifier: verifier, expires: a.now().Add(loginTransactionTTL)}
	if err := a.store.PutTransaction(r.Context(), state, transaction, loginTransactionTTL); err != nil {
		reason := "login_transaction_store_unavailable"
		if errors.Is(err, errSessionCapacity) {
			reason = "login_transaction_capacity_reached"
			writeError(w, http.StatusServiceUnavailable, "Authentication is busy")
		} else {
			writeError(w, http.StatusServiceUnavailable, "Authentication store is unavailable")
		}
		a.audit(r, auditEvent{Event: auditEventOIDCLogin, Outcome: auditOutcomeFailure, AuthMethod: authMethodOIDC, Reason: reason, Status: http.StatusServiceUnavailable})
		return
	}
	a.setCookie(w, loginBindingCookie, binding, int(loginTransactionTTL.Seconds()))
	authURL := runtime.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce))
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

func (a *authHandler) callback(w http.ResponseWriter, r *http.Request) {
	runtime := a.oidc.Load()
	if runtime == nil {
		a.audit(r, auditEvent{Event: auditEventOIDCLogin, Outcome: auditOutcomeFailure, AuthMethod: authMethodOIDC, Reason: "identity_provider_unavailable", Status: http.StatusServiceUnavailable})
		writeError(w, http.StatusServiceUnavailable, "Identity provider is not available")
		return
	}
	state := r.URL.Query().Get("state")
	bindingCookie, err := r.Cookie(loginBindingCookie)
	binding := ""
	if err == nil {
		binding = bindingCookie.Value
	}
	transaction, ok, err := a.consumeTransaction(r.Context(), state, binding)
	a.clearCookie(w, loginBindingCookie)
	if err != nil {
		a.audit(r, auditEvent{Event: auditEventOIDCLogin, Outcome: auditOutcomeFailure, AuthMethod: authMethodOIDC, Reason: auditReasonSessionStoreUnavailable, Status: http.StatusServiceUnavailable})
		writeError(w, http.StatusServiceUnavailable, "Authentication store is unavailable")
		return
	}
	if !ok || r.URL.Query().Get("code") == "" {
		a.audit(r, auditEvent{Event: auditEventOIDCLogin, Outcome: auditOutcomeDenied, AuthMethod: authMethodOIDC, Reason: "invalid_transaction", Status: http.StatusBadRequest})
		writeError(w, http.StatusBadRequest, "Invalid or expired authentication transaction")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	token, err := runtime.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(transaction.verifier))
	if err != nil {
		a.audit(r, auditEvent{Event: auditEventOIDCLogin, Outcome: auditOutcomeDenied, AuthMethod: authMethodOIDC, Reason: "token_exchange_failed", Status: http.StatusUnauthorized})
		writeError(w, http.StatusUnauthorized, "Authentication failed")
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		a.audit(r, auditEvent{Event: auditEventOIDCLogin, Outcome: auditOutcomeDenied, AuthMethod: authMethodOIDC, Reason: "missing_id_token", Status: http.StatusUnauthorized})
		writeError(w, http.StatusUnauthorized, "Authentication failed")
		return
	}
	idToken, err := runtime.verifier.Verify(ctx, rawIDToken)
	if err != nil || subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(transaction.nonce)) != 1 {
		a.audit(r, auditEvent{Event: auditEventOIDCLogin, Outcome: auditOutcomeDenied, AuthMethod: authMethodOIDC, Reason: "id_token_invalid", Status: http.StatusUnauthorized})
		writeError(w, http.StatusUnauthorized, "Authentication failed")
		return
	}
	var claims struct {
		Groups            []string `json:"groups"`
		Email             string   `json:"email"`
		PreferredUsername string   `json:"preferred_username"`
	}
	if err := idToken.Claims(&claims); err != nil {
		a.audit(r, auditEvent{Event: auditEventOIDCLogin, Outcome: auditOutcomeDenied, AuthMethod: authMethodOIDC, Reason: "claims_invalid", Status: http.StatusUnauthorized})
		writeError(w, http.StatusUnauthorized, "Authentication failed")
		return
	}
	// Any authenticated user may sign in; authorization is delegated to the
	// management cluster's RBAC. The username and groups are kept so the
	// dashboard can impersonate the user for writes.
	username := claims.Email
	if username == "" {
		username = claims.PreferredUsername
	}
	if username == "" {
		username = idToken.Subject
	}
	if username == "" {
		a.audit(r, auditEvent{Event: auditEventOIDCLogin, Outcome: auditOutcomeDenied, AuthMethod: authMethodOIDC, Reason: "username_missing", Status: http.StatusUnauthorized})
		writeError(w, http.StatusUnauthorized, "Authentication failed")
		return
	}
	if !idToken.Expiry.After(a.now()) {
		a.audit(r, auditEvent{Event: auditEventOIDCLogin, Outcome: auditOutcomeDenied, Actor: username, Groups: claims.Groups, AuthMethod: authMethodOIDC, Reason: "id_token_expired", Status: http.StatusUnauthorized})
		writeError(w, http.StatusUnauthorized, "Authentication failed")
		return
	}
	csrfToken, err := randomOpaque()
	if err != nil {
		a.audit(r, auditEvent{Event: auditEventOIDCLogin, Outcome: auditOutcomeFailure, Actor: username, Groups: claims.Groups, AuthMethod: authMethodOIDC, Reason: "csrf_generation_failed", Status: http.StatusInternalServerError})
		writeError(w, http.StatusInternalServerError, "Could not create session")
		return
	}
	sessionID, err := randomOpaque()
	if err != nil {
		a.audit(r, auditEvent{Event: auditEventOIDCLogin, Outcome: auditOutcomeFailure, Actor: username, Groups: claims.Groups, AuthMethod: authMethodOIDC, Reason: "session_id_generation_failed", Status: http.StatusInternalServerError})
		writeError(w, http.StatusInternalServerError, "Could not create session")
		return
	}
	// The dashboard session has its own sliding idle TTL and absolute TTL. It
	// is not capped at the ID token expiry: Keycloak issues short-lived ID
	// tokens (5 minutes by default), and the dashboard does not re-verify the
	// token per request, so tying the session to it would log users out almost
	// immediately. The raw ID token is only kept as logout hint.
	now := a.now()
	expires := now.Add(sessionAbsoluteTTL)
	session := dashboardSession{
		issuer:       idToken.Issuer,
		subject:      idToken.Subject,
		username:     username,
		groups:       claims.Groups,
		csrfToken:    csrfToken,
		idToken:      rawIDToken,
		lastActivity: now,
		expires:      expires,
		authMethod:   authMethodOIDC,
	}
	a.authzCache.Delete(authorizationCacheKey(session))
	if err := a.store.PutSession(r.Context(), sessionID, session, sessionTTL(session, now)); err != nil {
		reason := auditReasonSessionStoreUnavailable
		if errors.Is(err, errSessionCapacity) {
			reason = "session_capacity_reached"
			writeError(w, http.StatusServiceUnavailable, "Session capacity reached")
		} else {
			writeError(w, http.StatusServiceUnavailable, "Session store is unavailable")
		}
		a.audit(r, auditEvent{Event: auditEventOIDCLogin, Outcome: auditOutcomeFailure, Actor: username, Groups: claims.Groups, AuthMethod: authMethodOIDC, Reason: reason, Status: http.StatusServiceUnavailable})
		return
	}
	a.audit(r, auditEvent{Event: auditEventOIDCLogin, Outcome: auditOutcomeSuccess, Actor: username, Groups: claims.Groups, AuthMethod: authMethodOIDC, Status: http.StatusSeeOther})
	a.setSessionCookie(w, sessionID, session)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *authHandler) consumeTransaction(ctx context.Context, state, binding string) (loginTransaction, bool, error) {
	transaction, ok, err := a.store.TakeTransaction(ctx, state)
	if err != nil {
		return loginTransaction{}, false, err
	}
	if !ok {
		return loginTransaction{}, false, nil
	}
	if subtle.ConstantTimeCompare([]byte(state), []byte(transaction.state)) != 1 || subtle.ConstantTimeCompare([]byte(binding), []byte(transaction.binding)) != 1 || !a.now().Before(transaction.expires) {
		return loginTransaction{}, false, nil
	}
	return transaction, true, nil
}

func (a *authHandler) sessionFromRequest(w http.ResponseWriter, r *http.Request) (dashboardSession, bool, error) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return dashboardSession{}, false, nil
	}
	session, ok, err := a.store.GetSession(r.Context(), cookie.Value)
	if err != nil {
		return dashboardSession{}, false, err
	}
	now := a.now()
	if !ok || !now.Before(session.expires) || !now.Before(session.lastActivity.Add(sessionIdleTTL)) {
		_ = a.store.DeleteSession(r.Context(), cookie.Value)
		a.clearCookie(w, sessionCookie)
		return dashboardSession{}, false, nil
	}
	session.lastActivity = now
	if err := a.store.PutSession(r.Context(), cookie.Value, session, sessionTTL(session, now)); err != nil {
		return dashboardSession{}, false, err
	}
	remaining := sessionTTL(session, now)
	a.setCookie(w, sessionCookie, cookie.Value, int(remaining.Seconds()))
	return session, true, nil
}

func (a *authHandler) logout(w http.ResponseWriter, r *http.Request) {
	session, ok, err := a.sessionFromRequest(w, r)
	if err != nil {
		a.audit(r, auditEvent{Event: auditEventLogout, Outcome: auditOutcomeFailure, Reason: auditReasonSessionStoreUnavailable, Status: http.StatusServiceUnavailable})
		writeError(w, http.StatusServiceUnavailable, "Session store is unavailable")
		return
	}
	if !ok {
		a.audit(r, auditEvent{Event: auditEventLogout, Outcome: auditOutcomeDenied, Reason: "not_authenticated", Status: http.StatusUnauthorized})
		writeError(w, http.StatusUnauthorized, "Authentication required")
		return
	}
	if !a.canonicalHost(r) || !a.validCSRF(r, session) {
		a.audit(r, auditEvent{Event: auditEventLogout, Outcome: auditOutcomeDenied, Actor: session.username, Groups: session.groups, AuthMethod: session.authMethod, Impersonated: session.authMethod == authMethodOIDC, Reason: "csrf_or_host_rejected", Status: http.StatusForbidden})
		writeError(w, http.StatusForbidden, "CSRF validation failed")
		return
	}

	// 1. Delete the local dashboard session and clear its cookie.
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if err := a.store.DeleteSession(r.Context(), cookie.Value); err != nil {
			a.audit(r, auditEvent{Event: auditEventLogout, Outcome: auditOutcomeFailure, Actor: session.username, Groups: session.groups, AuthMethod: session.authMethod, Impersonated: session.authMethod == authMethodOIDC, Reason: auditReasonSessionStoreUnavailable, Status: http.StatusServiceUnavailable})
			writeError(w, http.StatusServiceUnavailable, "Session store is unavailable")
			return
		}
	}
	a.audit(r, auditEvent{Event: auditEventLogout, Outcome: auditOutcomeSuccess, Actor: session.username, Groups: session.groups, AuthMethod: session.authMethod, Impersonated: session.authMethod == authMethodOIDC, Status: http.StatusOK})
	a.clearCookie(w, sessionCookie)

	// Local sessions have no upstream IdP session to terminate.
	if session.authMethod != authMethodOIDC {
		writeJSON(w, http.StatusOK, map[string]string{logoutURLField: loginPath})
		return
	}
	runtime := a.oidc.Load()
	if runtime == nil {
		writeJSON(w, http.StatusOK, map[string]string{logoutURLField: loginPath})
		return
	}

	// 2. Build the RP-Initiated Logout URL so the browser can terminate the
	//    upstream IdP session. The frontend must navigate to it.
	postLogout := a.publicURL.Scheme + "://" + a.publicURL.Host + "/"
	if runtime.endSessionEndpoint == "" {
		// The IdP did not advertise an end_session_endpoint. Return the login
		// page so the browser at least leaves the dashboard.
		writeJSON(w, http.StatusOK, map[string]string{logoutURLField: loginPath})
		return
	}
	logoutURL, err := url.Parse(runtime.endSessionEndpoint)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not build logout URL")
		return
	}
	q := logoutURL.Query()
	q.Set("client_id", runtime.oauth.ClientID)
	q.Set("post_logout_redirect_uri", postLogout)
	if session.idToken != "" {
		q.Set("id_token_hint", session.idToken)
	}
	logoutURL.RawQuery = q.Encode()

	writeJSON(w, http.StatusOK, map[string]string{logoutURLField: logoutURL.String()})
}

func (a *authHandler) validCSRF(r *http.Request, session dashboardSession) bool {
	if !sameOrigin(r) {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" && !a.originMatchesRedirect(origin) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(session.csrfToken)) == 1
}

func (a *authHandler) originMatchesRedirect(raw string) bool {
	origin, err := url.Parse(raw)
	return err == nil && origin.IsAbs() && origin.User == nil && origin.Path == "" && origin.RawQuery == "" && origin.Fragment == "" && strings.EqualFold(origin.Scheme, a.publicURL.Scheme) && strings.EqualFold(origin.Host, a.publicURL.Host)
}

func sessionTTL(session dashboardSession, now time.Time) time.Duration {
	remaining := session.expires.Sub(now)
	if remaining > sessionIdleTTL {
		return sessionIdleTTL
	}
	return remaining
}

func randomOpaque() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (a *authHandler) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: a.secureCookie, SameSite: http.SameSiteLaxMode, MaxAge: maxAge, Expires: a.now().Add(time.Duration(maxAge) * time.Second)})
}

func (a *authHandler) setSessionCookie(w http.ResponseWriter, value string, session dashboardSession) {
	remaining := min(session.expires.Sub(a.now()), sessionIdleTTL)
	a.setCookie(w, sessionCookie, value, int(remaining.Seconds()))
}

func (a *authHandler) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: true, Secure: a.secureCookie, SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0)})
}
