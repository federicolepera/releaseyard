package dashboard

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testDashboardHost = "dashboard.localhost:18080"
	testOIDCClientID  = "releaseyard-dashboard"

	// Test-only group names used as OIDC claims. Authorization no longer depends
	// on them; they are only carried into the session for impersonation.
	loginGroup     = "/platform"
	adminGroup     = "/administrators"
	developerGroup = "/developers"
)

type fakeOIDCProvider struct {
	server        *httptest.Server
	key           *rsa.PrivateKey
	mu            sync.Mutex
	nonce         string
	nonceOverride string
	groups        any
	audience      any
	expiration    time.Time
	challenge     string
	gotVerifier   string
}

func newFakeOIDCProvider(t *testing.T, groups any) *fakeOIDCProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeOIDCProvider{key: key, groups: groups}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]any{
			"issuer":                                provider.server.URL,
			"authorization_endpoint":                provider.server.URL + "/authorize",
			"token_endpoint":                        provider.server.URL + "/token",
			"jwks_uri":                              provider.server.URL + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "test-key",
			"n": base64.RawURLEncoding.EncodeToString(provider.key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(provider.key.PublicKey.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		provider.mu.Lock()
		provider.gotVerifier = r.Form.Get("code_verifier")
		nonce := provider.nonce
		if provider.nonceOverride != "" {
			nonce = provider.nonceOverride
		}
		groupsValue := provider.groups
		audience := provider.audience
		expiration := provider.expiration
		challenge := provider.challenge
		provider.mu.Unlock()
		digest := sha256.Sum256([]byte(provider.gotVerifier))
		if provider.gotVerifier == "" || base64.RawURLEncoding.EncodeToString(digest[:]) != challenge {
			http.Error(w, "invalid PKCE verifier", http.StatusBadRequest)
			return
		}
		idToken, err := provider.signedIDToken(nonce, groupsValue, audience, expiration)
		if err != nil {
			http.Error(w, "could not sign token", http.StatusInternalServerError)
			return
		}
		writeTestJSON(w, map[string]any{"access_token": "test-access-token", "token_type": "Bearer", "expires_in": 3600, "id_token": idToken})
	})
	provider.server = httptest.NewServer(mux)
	t.Cleanup(provider.server.Close)
	return provider
}

func (p *fakeOIDCProvider) signedIDToken(nonce string, groups, audience any, expiration time.Time) (string, error) {
	now := time.Now()
	if audience == nil {
		audience = testOIDCClientID
	}
	if expiration.IsZero() {
		expiration = now.Add(time.Hour)
	}
	claims := map[string]any{
		"iss": p.server.URL, "sub": "user-123", "aud": audience,
		"iat": now.Unix(), "exp": expiration.Unix(), "nonce": nonce,
	}
	if groups != nil {
		claims["groups"] = groups
	}
	headerJSON, err := json.Marshal(map[string]string{"alg": "RS256", "kid": "test-key", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(encoded))
	signature, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return encoded + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func newOIDCTestHandler(t *testing.T, groups any, redirect string) (http.Handler, *fakeOIDCProvider) {
	t.Helper()
	provider := newFakeOIDCProvider(t, groups)
	_, k8s := testHandler(t)
	handler, err := NewOIDCHandler(k8s, testNamespace, OIDCConfig{
		IssuerURL: provider.server.URL, ClientID: testOIDCClientID,
		ClientSecret: "test-client-secret", RedirectURL: redirect,
	})
	if err != nil {
		t.Fatalf("construct OIDC handler: %v", err)
	}
	return handler, provider
}

type loginResult struct {
	state   string
	cookie  *http.Cookie
	request *url.URL
}

func beginTestLogin(t *testing.T, handler http.Handler, provider *fakeOIDCProvider) loginResult {
	return beginTestLoginAt(t, handler, provider, "http", testDashboardHost)
}

func beginTestLoginAt(t *testing.T, handler http.Handler, provider *fakeOIDCProvider, scheme, host string) loginResult {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, scheme+"://"+host+"/auth/login", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("login status=%d body=%s", response.Code, response.Body)
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse authorization redirect: %v", err)
	}
	state := location.Query().Get("state")
	nonce := location.Query().Get("nonce")
	challenge := location.Query().Get("code_challenge")
	if state == "" || nonce == "" || challenge == "" || location.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("authorization redirect omitted state, nonce, or PKCE S256")
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != loginBindingCookie {
		t.Fatalf("login binding cookie missing: %+v", cookies)
	}
	if cookies[0].Path != "/" || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode || cookies[0].Secure != (scheme == "https") {
		t.Fatalf("unexpected login binding cookie properties: %+v", cookies[0])
	}
	provider.mu.Lock()
	provider.nonce = nonce
	provider.challenge = challenge
	provider.mu.Unlock()
	return loginResult{state: state, cookie: cookies[0], request: location}
}

func finishTestLogin(t *testing.T, handler http.Handler, login loginResult) *httptest.ResponseRecorder {
	return finishTestLoginAt(t, handler, login, "http", testDashboardHost)
}

func finishTestLoginAt(t *testing.T, handler http.Handler, login loginResult, scheme, host string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, scheme+"://"+host+"/auth/callback?state="+url.QueryEscape(login.state)+"&code=test-code", nil)
	req.AddCookie(login.cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func TestOIDCConfigurationFailsClosed(t *testing.T) {
	_, k8s := testHandler(t)
	base := OIDCConfig{IssuerURL: "http://127.0.0.1:99", ClientID: testOIDCClientID, ClientSecret: "secret", RedirectURL: "http://dashboard.localhost:18080/auth/callback"}
	for name, mutate := range map[string]func(*OIDCConfig){
		"issuer":                    func(c *OIDCConfig) { c.IssuerURL = "" },
		"client ID":                 func(c *OIDCConfig) { c.ClientID = "" },
		"client secret":             func(c *OIDCConfig) { c.ClientSecret = "" },
		"redirect":                  func(c *OIDCConfig) { c.RedirectURL = "" },
		"insecure arbitrary issuer": func(c *OIDCConfig) { c.IssuerURL = "http://issuer.example/realm" },
	} {
		t.Run(name, func(t *testing.T) {
			config := base
			mutate(&config)
			if _, err := NewOIDCHandler(k8s, testNamespace, config); err == nil {
				t.Fatal("expected configuration error")
			}
		})
	}
	failedDiscovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer failedDiscovery.Close()
	base.IssuerURL = failedDiscovery.URL
	if _, err := NewOIDCHandler(k8s, testNamespace, base); err == nil {
		t.Fatal("expected discovery failure")
	}
}

func TestOIDCUnauthenticatedRoutesAndHealth(t *testing.T) {
	handler, _ := newOIDCTestHandler(t, []string{loginGroup, adminGroup}, "http://"+testDashboardHost+"/auth/callback")
	for _, test := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/api/applications", http.StatusUnauthorized},
		{http.MethodGet, "/api/unknown", http.StatusUnauthorized},
		{http.MethodGet, "/api/session", http.StatusUnauthorized},
		{http.MethodGet, "/", http.StatusSeeOther},
		{http.MethodGet, "/style.css", http.StatusSeeOther},
		{http.MethodPost, "/auth/logout", http.StatusUnauthorized},
	} {
		req := httptest.NewRequest(test.method, "http://"+testDashboardHost+test.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != test.status {
			t.Errorf("%s %s status=%d, want %d", test.method, test.path, response.Code, test.status)
		}
		if strings.HasPrefix(test.path, "/api/") && test.status == http.StatusUnauthorized && !strings.Contains(response.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s response Content-Type=%q, want JSON", test.path, response.Header().Get("Content-Type"))
		}
		if test.path == "/api/applications" && response.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("API Cache-Control=%q, want no-store", response.Header().Get("Cache-Control"))
		}
	}
}

func TestOIDCAdminSessionCSRFAndLogout(t *testing.T) {
	handler, provider := newOIDCTestHandler(t, []string{"users", loginGroup, adminGroup}, "http://"+testDashboardHost+"/auth/callback")
	login := beginTestLogin(t, handler, provider)
	response := finishTestLogin(t, handler, login)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/" {
		t.Fatalf("successful callback status=%d location=%q", response.Code, response.Header().Get("Location"))
	}
	var bindingCleared, foundSession bool
	var sessionCookieValue *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == loginBindingCookie && cookie.MaxAge < 0 {
			bindingCleared = true
		}
		if cookie.Name == sessionCookie {
			foundSession = true
			sessionCookieValue = cookie
		}
	}
	if !bindingCleared || !foundSession || sessionCookieValue == nil {
		t.Fatalf("callback did not clear binding and set session cookie: %+v", response.Result().Cookies())
	}
	if sessionCookieValue.Value == "" || strings.Count(sessionCookieValue.Value, ".") != 0 || sessionCookieValue.Path != "/" || !sessionCookieValue.HttpOnly || sessionCookieValue.SameSite != http.SameSiteLaxMode || sessionCookieValue.Secure {
		t.Fatalf("unexpected dev session cookie properties: %+v", sessionCookieValue)
	}
	provider.mu.Lock()
	gotVerifier := provider.gotVerifier
	provider.mu.Unlock()
	if gotVerifier == "" {
		t.Fatal("token exchange omitted PKCE verifier")
	}

	sessionReq := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/api/session", nil)
	sessionReq.AddCookie(sessionCookieValue)
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, sessionReq)
	if sessionResponse.Code != http.StatusOK || sessionResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET /api/session status=%d headers=%v", sessionResponse.Code, sessionResponse.Header())
	}
	var sessionBody map[string]any
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &sessionBody); err != nil || sessionBody["csrfToken"] == "" {
		t.Fatalf("GET /api/session exposed unexpected data: %s", sessionResponse.Body)
	}
	csrf, _ := sessionBody["csrfToken"].(string)

	for _, test := range []struct {
		name   string
		method string
		path   string
		origin string
		csrf   string
		want   int
	}{
		{name: "missing token", method: http.MethodPost, path: "/api/applications", origin: "http://" + testDashboardHost, want: http.StatusForbidden},
		{name: "wrong token", method: http.MethodPost, path: "/api/applications", origin: "http://" + testDashboardHost, csrf: "wrong", want: http.StatusForbidden},
		{name: "wrong origin", method: http.MethodPost, path: "/api/applications", origin: "http://attacker.localhost:18080", csrf: csrf, want: http.StatusForbidden},
		{name: "valid token without origin", method: http.MethodPost, path: "/api/applications", csrf: csrf, want: http.StatusUnsupportedMediaType},
		{name: "PATCH missing token", method: http.MethodPatch, path: "/api/clusters/production/environment", origin: "http://" + testDashboardHost, want: http.StatusForbidden},
		{name: "DELETE missing token", method: http.MethodDelete, path: "/api/applications/sample", origin: "http://" + testDashboardHost, want: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := strings.NewReader("{}")
			if test.method == http.MethodDelete {
				body = strings.NewReader("")
			}
			req := httptest.NewRequest(test.method, "http://"+testDashboardHost+test.path, body)
			req.AddCookie(sessionCookieValue)
			if test.want != http.StatusUnsupportedMediaType {
				req.Header.Set("Content-Type", "application/json")
			}
			if test.origin != "" {
				req.Header.Set("Origin", test.origin)
			}
			if test.csrf != "" {
				req.Header.Set("X-CSRF-Token", test.csrf)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != test.want {
				t.Errorf("POST status=%d, want %d body=%s", w.Code, test.want, w.Body)
			}
			if test.name == "PATCH missing token" && !strings.Contains(w.Body.String(), "CSRF validation failed") {
				t.Errorf("PATCH without CSRF body=%q, want CSRF rejection", w.Body.String())
			}
		})
	}

	logout := httptest.NewRequest(http.MethodPost, "http://"+testDashboardHost+"/auth/logout", nil)
	logout.AddCookie(sessionCookieValue)
	logout.Header.Set("Origin", "http://"+testDashboardHost)
	logoutWithoutCSRF := httptest.NewRecorder()
	handler.ServeHTTP(logoutWithoutCSRF, logout)
	if logoutWithoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("logout without CSRF status=%d, want 403", logoutWithoutCSRF.Code)
	}

	logout = httptest.NewRequest(http.MethodPost, "http://"+testDashboardHost+"/auth/logout", nil)
	logout.AddCookie(sessionCookieValue)
	logout.Header.Set("Origin", "http://"+testDashboardHost)
	logout.Header.Set("X-CSRF-Token", csrf)
	logoutResponse := httptest.NewRecorder()
	handler.ServeHTTP(logoutResponse, logout)
	var logoutResult map[string]string
	if err := json.Unmarshal(logoutResponse.Body.Bytes(), &logoutResult); err != nil || logoutResponse.Code != http.StatusOK || logoutResponse.Header().Get("Location") != "" || logoutResult["logoutUrl"] != "/login" {
		t.Fatalf("logout status=%d location=%q body=%q err=%v", logoutResponse.Code, logoutResponse.Header().Get("Location"), logoutResponse.Body.String(), err)
	}
	protected := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/api/applications", nil)
	protected.AddCookie(sessionCookieValue)
	protectedResponse := httptest.NewRecorder()
	handler.ServeHTTP(protectedResponse, protected)
	if protectedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("request after logout status=%d, want 401", protectedResponse.Code)
	}
}

func TestSessionCapabilitiesWithoutRBAC(t *testing.T) {
	handler, provider := newOIDCTestHandler(t, []string{"users"}, "http://"+testDashboardHost+"/auth/callback")
	login := beginTestLogin(t, handler, provider)
	callback := finishTestLogin(t, handler, login)
	if callback.Code != http.StatusSeeOther {
		t.Fatalf("callback status=%d, want 303", callback.Code)
	}
	var sessionCookieValue *http.Cookie
	for _, cookie := range callback.Result().Cookies() {
		if cookie.Name == sessionCookie {
			sessionCookieValue = cookie
		}
	}
	if sessionCookieValue == nil {
		t.Fatal("callback did not issue session cookie")
	}

	sessionRequest := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/api/session", nil)
	sessionRequest.AddCookie(sessionCookieValue)
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, sessionRequest)
	var capabilities struct {
		CSRFToken     string `json:"csrfToken"`
		IsAdmin       bool   `json:"isAdmin"`
		CanCreateApps bool   `json:"canCreateApps"`
		CanDeleteApps bool   `json:"canDeleteApps"`
		CanUpdateApps bool   `json:"canUpdateApps"`
	}
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &capabilities); err != nil || sessionResponse.Code != http.StatusOK {
		t.Fatalf("GET /api/session status=%d: %v", sessionResponse.Code, err)
	}
	// Without an impersonating client factory the dashboard cannot consult the
	// cluster's RBAC, so it falls back to allowing everything and performs
	// writes with its own ServiceAccount.
	if capabilities.CSRFToken == "" || !capabilities.IsAdmin || !capabilities.CanCreateApps || !capabilities.CanDeleteApps || !capabilities.CanUpdateApps {
		t.Fatalf("unexpected fallback capabilities: %+v", capabilities)
	}

	// Writes are no longer blocked by application-level rules; they reach the
	// handler and the API. CSRF is still enforced.
	deleteRequest := httptest.NewRequest(http.MethodDelete, "http://"+testDashboardHost+"/api/applications/invalid_name", nil)
	deleteRequest.AddCookie(sessionCookieValue)
	deleteRequest.Header.Set("Origin", "http://"+testDashboardHost)
	deleteRequest.Header.Set("X-CSRF-Token", capabilities.CSRFToken)
	deleteResponse := httptest.NewRecorder()
	handler.ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusBadRequest {
		t.Fatalf("DELETE invalid name status=%d, want 400", deleteResponse.Code)
	}

	patchWithoutCSRF := httptest.NewRequest(http.MethodPatch, "http://"+testDashboardHost+"/api/applications/invalid_name", strings.NewReader("{}"))
	patchWithoutCSRF.AddCookie(sessionCookieValue)
	patchWithoutCSRF.Header.Set("Origin", "http://"+testDashboardHost)
	patchWithoutCSRF.Header.Set("Content-Type", "application/json")
	patchWithoutCSRFResponse := httptest.NewRecorder()
	handler.ServeHTTP(patchWithoutCSRFResponse, patchWithoutCSRF)
	if patchWithoutCSRFResponse.Code != http.StatusForbidden || !strings.Contains(patchWithoutCSRFResponse.Body.String(), "CSRF validation failed") {
		t.Fatalf("PATCH without CSRF status=%d body=%q", patchWithoutCSRFResponse.Code, patchWithoutCSRFResponse.Body.String())
	}
}

func TestApplicationItemAuthorizationPathBoundary(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/applications/sample":       true,
		"/api/applications":              false,
		"/api/applications/":             false,
		"/api/applications/sample/child": false,
		"/api/application/sample":        false,
	} {
		if got := isApplicationItemPath(path); got != want {
			t.Errorf("isApplicationItemPath(%q)=%t, want %t", path, got, want)
		}
	}
}

func TestClusterEnvironmentAuthorizationPathBoundary(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/clusters/production/environment":       true,
		"/api/clusters/environment":                  false,
		"/api/clusters/production/environment/child": false,
		"/api/clusters/a/b/environment":              false,
		"/api/environments/production/targets":       false,
	} {
		if got := isClusterEnvironmentPath(path); got != want {
			t.Errorf("isClusterEnvironmentPath(%q)=%t, want %t", path, got, want)
		}
	}
}

func TestOIDCAllowsPlatformMemberAndRejectsBadNonce(t *testing.T) {
	t.Run("any authenticated user", func(t *testing.T) {
		handler, provider := newOIDCTestHandler(t, []string{"/users", "/Administrators"}, "http://"+testDashboardHost+"/auth/callback")
		login := beginTestLogin(t, handler, provider)
		response := finishTestLogin(t, handler, login)
		if response.Code != http.StatusSeeOther {
			t.Fatalf("callback status=%d, want 303", response.Code)
		}
		var sessionCookieValue *http.Cookie
		for _, cookie := range response.Result().Cookies() {
			if cookie.Name == sessionCookie {
				sessionCookieValue = cookie
			}
		}
		if sessionCookieValue == nil {
			t.Fatal("authenticated user did not receive a session")
		}
		sessionRequest := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/api/session", nil)
		sessionRequest.AddCookie(sessionCookieValue)
		sessionResponse := httptest.NewRecorder()
		handler.ServeHTTP(sessionResponse, sessionRequest)
		var capabilities struct {
			IsAdmin       bool `json:"isAdmin"`
			CanCreateApps bool `json:"canCreateApps"`
			CanDeleteApps bool `json:"canDeleteApps"`
		}
		if err := json.Unmarshal(sessionResponse.Body.Bytes(), &capabilities); err != nil || sessionResponse.Code != http.StatusOK {
			t.Fatalf("GET /api/session status=%d err=%v", sessionResponse.Code, err)
		}
		// Without a RBAC-backed client factory the fallback allows everything.
		if !capabilities.IsAdmin || !capabilities.CanCreateApps || !capabilities.CanDeleteApps {
			t.Fatalf("unexpected fallback capabilities: %+v", capabilities)
		}
	})
	t.Run("wrong nonce", func(t *testing.T) {
		handler, provider := newOIDCTestHandler(t, []string{loginGroup, adminGroup}, "http://"+testDashboardHost+"/auth/callback")
		login := beginTestLogin(t, handler, provider)
		provider.mu.Lock()
		provider.nonceOverride = "wrong-nonce"
		provider.mu.Unlock()
		response := finishTestLogin(t, handler, login)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("wrong nonce callback status=%d, want 401", response.Code)
		}
	})
	for _, test := range []struct {
		name       string
		groups     any
		audience   any
		expiry     time.Time
		wantStatus int
	}{
		{name: "wrong audience", groups: []string{loginGroup, adminGroup}, audience: "another-client", wantStatus: http.StatusUnauthorized},
		{name: "expired ID token", groups: []string{loginGroup, adminGroup}, expiry: time.Now().Add(-time.Minute), wantStatus: http.StatusUnauthorized},
		{name: "missing groups", groups: nil, wantStatus: http.StatusSeeOther},
		{name: "wrong-typed groups", groups: "administrators", wantStatus: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, provider := newOIDCTestHandler(t, test.groups, "http://"+testDashboardHost+"/auth/callback")
			provider.mu.Lock()
			provider.audience = test.audience
			provider.expiration = test.expiry
			provider.mu.Unlock()
			login := beginTestLogin(t, handler, provider)
			response := finishTestLogin(t, handler, login)
			if response.Code != test.wantStatus {
				t.Fatalf("callback status=%d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}

func TestOIDCTransactionStateBindingAndReplay(t *testing.T) {
	handler, provider := newOIDCTestHandler(t, []string{loginGroup, adminGroup}, "http://"+testDashboardHost+"/auth/callback")
	login := beginTestLogin(t, handler, provider)
	missingBinding := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/auth/callback?state="+url.QueryEscape(login.state)+"&code=test-code", nil)
	missingBindingResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingBindingResponse, missingBinding)
	if missingBindingResponse.Code != http.StatusBadRequest {
		t.Fatalf("missing binding callback status=%d, want 400", missingBindingResponse.Code)
	}
	replay := finishTestLogin(t, handler, login)
	if replay.Code != http.StatusBadRequest {
		t.Fatalf("callback replay status=%d, want 400", replay.Code)
	}

	second := beginTestLogin(t, handler, provider)
	badState := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/auth/callback?state=invalid&code=test-code", nil)
	badState.AddCookie(second.cookie)
	badStateResponse := httptest.NewRecorder()
	handler.ServeHTTP(badStateResponse, badState)
	if badStateResponse.Code != http.StatusBadRequest {
		t.Fatalf("bad state callback status=%d, want 400", badStateResponse.Code)
	}
	if response := finishTestLogin(t, handler, second); response.Code != http.StatusSeeOther {
		t.Fatalf("bad state unexpectedly consumed unrelated valid transaction: status=%d", response.Code)
	}
}

func TestOIDCExpiredTransactionAndSession(t *testing.T) {
	now := time.Now()
	state := "expired-state"
	binding := "binding"
	sessionID := "expired-session"
	store := newMemorySessionStore()
	store.transactions[state] = memoryTransactionEntry{
		transaction: loginTransaction{state: state, binding: binding, expires: now.Add(-time.Second)},
		expires:     now.Add(-time.Second),
	}
	store.sessions[sessionID] = memorySessionEntry{
		session: dashboardSession{lastActivity: now.Add(-sessionIdleTTL - time.Second), expires: now.Add(time.Hour)},
		expires: now.Add(time.Hour),
	}
	auth := &authHandler{
		now:   func() time.Time { return now },
		store: store,
	}
	if _, ok, err := auth.consumeTransaction(context.Background(), state, binding); err != nil || ok {
		t.Fatalf("expired login transaction accepted or failed: ok=%t err=%v", ok, err)
	}
	if _, ok := store.transactions[state]; ok {
		t.Fatal("expired login transaction was not consumed")
	}
	req := httptest.NewRequest(http.MethodGet, "http://dashboard.localhost/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionID})
	response := httptest.NewRecorder()
	if _, ok, err := auth.sessionFromRequest(response, req); err != nil || ok {
		t.Fatalf("idle-expired session accepted or failed: ok=%t err=%v", ok, err)
	}
	if _, ok := store.sessions[sessionID]; ok {
		t.Fatal("expired session was not invalidated")
	}
}

func TestOIDCHTTPSUsesSecureCookies(t *testing.T) {
	handler, provider := newOIDCTestHandler(t, []string{loginGroup, adminGroup}, "https://dashboard.example/auth/callback")
	login := beginTestLoginAt(t, handler, provider, "https", "dashboard.example")
	response := finishTestLoginAt(t, handler, login, "https", "dashboard.example")
	if response.Code != http.StatusSeeOther {
		t.Fatalf("HTTPS callback status=%d body=%s", response.Code, response.Body)
	}
	var found bool
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookie && cookie.Secure && cookie.HttpOnly && cookie.SameSite == http.SameSiteLaxMode && cookie.Path == "/" {
			found = true
		}
	}
	if !found {
		t.Fatalf("HTTPS session cookie lacks secure properties: %+v", response.Result().Cookies())
	}
}
