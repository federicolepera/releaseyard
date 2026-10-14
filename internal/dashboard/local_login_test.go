package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

const (
	testLocalAdminPassword = "correct-horse-battery-staple"
	testHTTPSAdminPassword = "secure-transport-only-password"
	testLoginPath          = "/login"
	testMissingHashPath    = "/tmp/password-hash"
)

// writeLocalAdminHash writes a valid bcrypt hash to a temporary file and
// returns its path, mimicking the mounted password-hash Secret key.
func writeLocalAdminHash(t *testing.T, password string, cost int) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		t.Fatalf("create test hash: %v", err)
	}
	path := filepath.Join(t.TempDir(), "password-hash")
	if err := os.WriteFile(path, hash, 0o600); err != nil {
		t.Fatalf("write test hash: %v", err)
	}
	return path
}

func newLocalTestHandler(t *testing.T, hashPath, publicURL string) http.Handler {
	t.Helper()
	_, k8s := testHandler(t)
	handler, err := NewLocalHandler(k8s, testNamespace, LocalAuthConfig{HashPath: hashPath, PublicURL: publicURL})
	if err != nil {
		t.Fatalf("construct local handler: %v", err)
	}
	return handler
}

func loginRequest(password, origin, host string) *http.Request {
	form := url.Values{}
	if password != "" {
		form.Set("password", password)
	}
	req := httptest.NewRequest(http.MethodPost, "http://"+host+"/auth/local", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	return req
}

func getSessionCookie(t *testing.T, response *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookie {
			return cookie
		}
	}
	return nil
}

func TestLocalHandlerConfigurationFailsClosed(t *testing.T) {
	_, k8s := testHandler(t)
	for name, config := range map[string]LocalAuthConfig{
		"missing hash path": {HashPath: "", PublicURL: "https://dashboard.example"},
		"missing public URL": {
			HashPath: testMissingHashPath, PublicURL: "",
		},
		"public URL with path": {
			HashPath: testMissingHashPath, PublicURL: "https://dashboard.example/app",
		},
		"insecure public URL": {
			HashPath: testMissingHashPath, PublicURL: "http://dashboard.example",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewLocalHandler(k8s, testNamespace, config); err == nil {
				t.Fatal("expected configuration error")
			}
		})
	}
}

func TestLocalUnauthenticatedRoutesAndHealth(t *testing.T) {
	handler := newLocalTestHandler(t, writeLocalAdminHash(t, testLocalAdminPassword, localAdminBcryptCost), "http://"+testDashboardHost)
	for _, test := range []struct {
		method string
		path   string
		status int
		loc    string
	}{
		{http.MethodGet, "/healthz", http.StatusOK, ""},
		{http.MethodGet, testLoginPath, http.StatusSeeOther, "/login.html"},
		{http.MethodGet, "/login.html", http.StatusOK, ""},
		{http.MethodGet, "/login.js", http.StatusOK, ""},
		{http.MethodGet, "/style.css", http.StatusOK, ""},
		{http.MethodGet, "/api/session", http.StatusUnauthorized, ""},
		{http.MethodGet, testApplicationsPath, http.StatusUnauthorized, ""},
		{http.MethodGet, "/", http.StatusSeeOther, testLoginPath},
		{http.MethodGet, "/applications.html", http.StatusSeeOther, testLoginPath},
		{http.MethodPost, "/auth/logout", http.StatusUnauthorized, ""},
	} {
		req := httptest.NewRequest(test.method, "http://"+testDashboardHost+test.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != test.status {
			t.Errorf("%s %s status=%d, want %d", test.method, test.path, response.Code, test.status)
		}
		if test.loc != "" && response.Header().Get("Location") != test.loc {
			t.Errorf("%s %s Location=%q, want %q", test.method, test.path, response.Header().Get("Location"), test.loc)
		}
		if strings.HasPrefix(test.path, "/api/") && test.status == http.StatusUnauthorized && !strings.Contains(response.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s response Content-Type=%q, want JSON", test.path, response.Header().Get("Content-Type"))
		}
	}
}

func TestLocalLoginSuccessAndCapabilities(t *testing.T) {
	handler := newLocalTestHandler(t, writeLocalAdminHash(t, testLocalAdminPassword, localAdminBcryptCost), "http://"+testDashboardHost)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, loginRequest(testLocalAdminPassword, "http://"+testDashboardHost, testDashboardHost))
	if response.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", response.Code, response.Body)
	}
	cookie := getSessionCookie(t, response)
	if cookie == nil || cookie.Value == "" || cookie.Path != "/" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Secure {
		t.Fatalf("unexpected local session cookie: %+v", cookie)
	}

	sessionReq := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/api/session", nil)
	sessionReq.AddCookie(cookie)
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, sessionReq)
	if sessionResponse.Code != http.StatusOK {
		t.Fatalf("GET /api/session status=%d body=%s", sessionResponse.Code, sessionResponse.Body)
	}
	var capabilities struct {
		CSRFToken     string `json:"csrfToken"`
		IsAdmin       bool   `json:"isAdmin"`
		CanCreateApps bool   `json:"canCreateApps"`
		CanDeleteApps bool   `json:"canDeleteApps"`
		CanUpdateApps bool   `json:"canUpdateApps"`
	}
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &capabilities); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if capabilities.CSRFToken == "" || !capabilities.IsAdmin || !capabilities.CanCreateApps || !capabilities.CanDeleteApps || !capabilities.CanUpdateApps {
		t.Fatalf("local admin session lacks full capabilities: %+v", capabilities)
	}

	adminReq := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/identity-provider.html", nil)
	adminReq.AddCookie(cookie)
	adminResponse := httptest.NewRecorder()
	handler.ServeHTTP(adminResponse, adminReq)
	if adminResponse.Code != http.StatusOK {
		t.Fatalf("GET /identity-provider.html status=%d, want 200", adminResponse.Code)
	}
}

func TestLocalLoginRejections(t *testing.T) {
	handler := newLocalTestHandler(t, writeLocalAdminHash(t, testLocalAdminPassword, localAdminBcryptCost), "http://"+testDashboardHost)
	for name, test := range map[string]struct {
		request *http.Request
		want    int
	}{
		"wrong password":   {loginRequest("wrong-password", "http://"+testDashboardHost, testDashboardHost), http.StatusUnauthorized},
		"missing password": {loginRequest("", "http://"+testDashboardHost, testDashboardHost), http.StatusBadRequest},
		"foreign origin":   {loginRequest(testLocalAdminPassword, "http://attacker.localhost:18080", testDashboardHost), http.StatusForbidden},
		"wrong host":       {loginRequest(testLocalAdminPassword, "http://"+testDashboardHost, "attacker.example"), http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, test.request)
			if response.Code != test.want {
				t.Fatalf("login status=%d, want %d body=%s", response.Code, test.want, response.Body)
			}
			if getSessionCookie(t, response) != nil {
				t.Fatal("rejected login issued a session cookie")
			}
		})
	}
}

func TestLocalLoginUnavailableWithoutCredentials(t *testing.T) {
	handler := newLocalTestHandler(t, filepath.Join(t.TempDir(), "missing-password-hash"), "http://"+testDashboardHost)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, loginRequest(testLocalAdminPassword, "http://"+testDashboardHost, testDashboardHost))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("login without credentials status=%d, want 503 body=%s", response.Code, response.Body)
	}
	if getSessionCookie(t, response) != nil {
		t.Fatal("login without credentials issued a session cookie")
	}
}

func TestLocalLoginRejectsWeakHashCost(t *testing.T) {
	handler := newLocalTestHandler(t, writeLocalAdminHash(t, testLocalAdminPassword, bcrypt.DefaultCost), "http://"+testDashboardHost)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, loginRequest(testLocalAdminPassword, "http://"+testDashboardHost, testDashboardHost))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("login with weak hash status=%d, want 503 body=%s", response.Code, response.Body)
	}
}

func TestLocalLoginLogout(t *testing.T) {
	handler := newLocalTestHandler(t, writeLocalAdminHash(t, testLocalAdminPassword, localAdminBcryptCost), "http://"+testDashboardHost)
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, loginRequest(testLocalAdminPassword, "http://"+testDashboardHost, testDashboardHost))
	cookie := getSessionCookie(t, loginResponse)
	if cookie == nil {
		t.Fatal("login did not issue a session cookie")
	}

	sessionReq := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/api/session", nil)
	sessionReq.AddCookie(cookie)
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, sessionReq)
	var capabilities struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &capabilities); err != nil || capabilities.CSRFToken == "" {
		t.Fatalf("decode session csrf: err=%v body=%s", err, sessionResponse.Body)
	}

	withoutCSRF := httptest.NewRequest(http.MethodPost, "http://"+testDashboardHost+"/auth/logout", nil)
	withoutCSRF.AddCookie(cookie)
	withoutCSRF.Header.Set("Origin", "http://"+testDashboardHost)
	withoutCSRFResponse := httptest.NewRecorder()
	handler.ServeHTTP(withoutCSRFResponse, withoutCSRF)
	if withoutCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("logout without CSRF status=%d, want 403", withoutCSRFResponse.Code)
	}

	logout := httptest.NewRequest(http.MethodPost, "http://"+testDashboardHost+"/auth/logout", nil)
	logout.AddCookie(cookie)
	logout.Header.Set("Origin", "http://"+testDashboardHost)
	logout.Header.Set("X-CSRF-Token", capabilities.CSRFToken)
	logoutResponse := httptest.NewRecorder()
	handler.ServeHTTP(logoutResponse, logout)
	var body map[string]string
	if err := json.Unmarshal(logoutResponse.Body.Bytes(), &body); err != nil || logoutResponse.Code != http.StatusOK || body["logoutUrl"] != testLoginPath {
		t.Fatalf("logout status=%d body=%s err=%v", logoutResponse.Code, logoutResponse.Body, err)
	}

	afterReq := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/api/session", nil)
	afterReq.AddCookie(cookie)
	afterResponse := httptest.NewRecorder()
	handler.ServeHTTP(afterResponse, afterReq)
	if afterResponse.Code != http.StatusUnauthorized {
		t.Fatalf("session after logout status=%d, want 401", afterResponse.Code)
	}
}

func TestLocalLoginHTTPSUsesSecureCookies(t *testing.T) {
	handler := newLocalTestHandler(t, writeLocalAdminHash(t, testHTTPSAdminPassword, localAdminBcryptCost), "https://dashboard.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, loginRequest(testHTTPSAdminPassword, "https://dashboard.example", "dashboard.example"))
	if response.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", response.Code, response.Body)
	}
	cookie := getSessionCookie(t, response)
	if cookie == nil || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("HTTPS session cookie lacks secure properties: %+v", cookie)
	}
}
