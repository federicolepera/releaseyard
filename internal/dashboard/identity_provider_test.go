package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testIdentityProviderSecretName = "releaseyard-dashboard-identity-provider"

func newIdentityProviderClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func newLocalAdminHandlerWithClient(t *testing.T, k8s client.Client) http.Handler {
	t.Helper()
	handler, err := NewLocalHandler(k8s, testNamespace, LocalAuthConfig{
		HashPath:  writeLocalAdminHash(t, testLocalAdminPassword, localAdminBcryptCost),
		PublicURL: "http://" + testDashboardHost,
	})
	if err != nil {
		t.Fatalf("construct local handler: %v", err)
	}
	return handler
}

func localAdminSession(t *testing.T, handler http.Handler) (*http.Cookie, string) {
	t.Helper()
	login := httptest.NewRecorder()
	handler.ServeHTTP(login, loginRequest(testLocalAdminPassword, "http://"+testDashboardHost, testDashboardHost))
	cookie := getSessionCookie(t, login)
	if cookie == nil {
		t.Fatalf("login did not issue a session: %d %s", login.Code, login.Body)
	}
	session := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/api/session", nil)
	session.AddCookie(cookie)
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, session)
	var body struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &body); err != nil || body.CSRFToken == "" {
		t.Fatalf("could not read CSRF token: err=%v body=%s", err, sessionResponse.Body)
	}
	return cookie, body.CSRFToken
}

func identityProviderRequestFor(method, body string, cookie *http.Cookie, csrf string) *http.Request {
	req := httptest.NewRequest(method, "http://"+testDashboardHost+"/api/administration/identity-provider", strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	req.Header.Set("Origin", "http://"+testDashboardHost)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	return req
}

func TestIdentityProviderStorageRoundTrip(t *testing.T) {
	k8s := newIdentityProviderClient(t)
	ctx := context.Background()
	config := identityProviderConfig{IssuerURL: "https://sso.example.com/realms/main", ClientID: "dashboard", ClientSecret: "top-secret"}

	if err := saveIdentityProvider(ctx, k8s, testNamespace, testIdentityProviderSecretName, config); err != nil {
		t.Fatalf("save identity provider: %v", err)
	}
	got, ok, err := loadIdentityProvider(ctx, k8s, testNamespace, testIdentityProviderSecretName)
	if err != nil || !ok {
		t.Fatalf("load identity provider: ok=%t err=%v", ok, err)
	}
	if got != config {
		t.Fatalf("loaded config=%+v, want %+v", got, config)
	}

	if err := deleteIdentityProvider(ctx, k8s, testNamespace, testIdentityProviderSecretName); err != nil {
		t.Fatalf("delete identity provider: %v", err)
	}
	if _, ok, err := loadIdentityProvider(ctx, k8s, testNamespace, testIdentityProviderSecretName); err != nil || ok {
		t.Fatalf("after delete ok=%t err=%v, want absent", ok, err)
	}
}

func TestLoadIdentityProviderRejectsIncompleteSecret(t *testing.T) {
	k8s := newIdentityProviderClient(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testIdentityProviderSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{identityProviderIssuerKey: []byte("https://sso.example.com")},
	})
	if _, ok, err := loadIdentityProvider(context.Background(), k8s, testNamespace, testIdentityProviderSecretName); err != nil || ok {
		t.Fatalf("incomplete secret ok=%t err=%v, want absent", ok, err)
	}
}

func TestBuildOIDCRuntimeFromStoredConfig(t *testing.T) {
	provider := newFakeOIDCProvider(t, []string{loginGroup, adminGroup})
	publicURL, err := url.Parse("http://" + testDashboardHost)
	if err != nil {
		t.Fatal(err)
	}
	idpRuntime, err := buildOIDCRuntime(context.Background(), identityProviderConfig{
		IssuerURL: provider.server.URL, ClientID: testOIDCClientID, ClientSecret: "secret",
	}, publicURL)
	if err != nil {
		t.Fatalf("build runtime: %v", err)
	}
	if idpRuntime.issuer != provider.server.URL {
		t.Fatalf("runtime issuer=%q, want %q", idpRuntime.issuer, provider.server.URL)
	}
	if idpRuntime.oauth.RedirectURL != "http://"+testDashboardHost+"/auth/callback" {
		t.Fatalf("runtime redirect=%q", idpRuntime.oauth.RedirectURL)
	}
	if idpRuntime.verifier == nil {
		t.Fatal("runtime verifier is nil")
	}
}

func TestBuildOIDCRuntimeRejectsInsecureIssuer(t *testing.T) {
	publicURL, _ := url.Parse("http://" + testDashboardHost)
	if _, err := buildOIDCRuntime(context.Background(), identityProviderConfig{
		IssuerURL: "http://issuer.example", ClientID: "id", ClientSecret: "secret",
	}, publicURL); err == nil {
		t.Fatal("expected insecure issuer to be rejected")
	}
}

func TestIdentityProviderAdminRequiresSessionAndCSRF(t *testing.T) {
	k8s := newIdentityProviderClient(t)
	handler := newLocalAdminHandlerWithClient(t, k8s)

	anonymous := httptest.NewRecorder()
	handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/api/administration/identity-provider", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET status=%d, want 401", anonymous.Code)
	}

	_, csrf := localAdminSession(t, handler)
	noCSRF := httptest.NewRecorder()
	handler.ServeHTTP(noCSRF, identityProviderRequestFor(http.MethodPut, `{}`, nil, csrf))
	if noCSRF.Code != http.StatusUnauthorized {
		t.Fatalf("PUT without session status=%d, want 401", noCSRF.Code)
	}
}

func TestIdentityProviderAdminSaveAndDelete(t *testing.T) {
	k8s := newIdentityProviderClient(t)
	handler := newLocalAdminHandlerWithClient(t, k8s)
	cookie, csrf := localAdminSession(t, handler)
	provider := newFakeOIDCProvider(t, []string{loginGroup, adminGroup})

	put := httptest.NewRecorder()
	handler.ServeHTTP(put, identityProviderRequestFor(http.MethodPut, `{"issuerURL":"`+provider.server.URL+`","clientID":"`+testOIDCClientID+`","clientSecret":"s3cr3t"}`, cookie, csrf))
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", put.Code, put.Body)
	}
	var status identityProviderStatus
	if err := json.Unmarshal(put.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode PUT response: %v", err)
	}
	if !status.Enabled || status.IssuerURL != provider.server.URL || !status.HasClientSecret {
		t.Fatalf("unexpected PUT status: %+v", status)
	}
	if status.RedirectURL != "http://"+testDashboardHost+"/auth/callback" {
		t.Fatalf("unexpected redirect URL: %q", status.RedirectURL)
	}
	// The stored configuration is now active without a restart.
	loginPage := httptest.NewRecorder()
	handler.ServeHTTP(loginPage, httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/api/auth-methods", nil))
	var methods map[string]any
	if err := json.Unmarshal(loginPage.Body.Bytes(), &methods); err != nil || methods["oidc"] != true {
		t.Fatalf("auth methods after save: %s (err=%v)", loginPage.Body, err)
	}

	get := httptest.NewRecorder()
	handler.ServeHTTP(get, identityProviderRequestFor(http.MethodGet, "", cookie, ""))
	if get.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", get.Code, get.Body)
	}

	del := httptest.NewRecorder()
	handler.ServeHTTP(del, identityProviderRequestFor(http.MethodDelete, "", cookie, csrf))
	if del.Code != http.StatusNoContent {
		t.Fatalf("DELETE status=%d body=%s", del.Code, del.Body)
	}
	if _, ok, err := loadIdentityProvider(context.Background(), k8s, testNamespace, testIdentityProviderSecretName); err != nil || ok {
		t.Fatalf("secret still present after delete: ok=%t err=%v", ok, err)
	}
}

func TestIdentityProviderAdminWithoutImpersonationIsPrivileged(t *testing.T) {
	handler, provider := newOIDCTestHandler(t, []string{"releaseyard:developers"}, "http://"+testDashboardHost+"/auth/callback")
	login := beginTestLogin(t, handler, provider)
	callback := finishTestLogin(t, handler, login)
	var cookie *http.Cookie
	for _, c := range callback.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("OIDC login did not issue a session cookie")
	}

	session := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/api/session", nil)
	session.AddCookie(cookie)
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, session)
	var flags struct {
		IsLocalAdmin  bool `json:"isLocalAdmin"`
		CanAdminister bool `json:"canAdminister"`
	}
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &flags); err != nil {
		t.Fatal(err)
	}
	if flags.IsLocalAdmin {
		t.Fatal("OIDC session was reported as the local administrator")
	}
	// A pinned-provider handler has no impersonating client, so the dashboard
	// cannot consult RBAC and falls back to treating the session as privileged.
	if !flags.CanAdminister {
		t.Fatal("OIDC session without impersonation should be able to administer")
	}

	get := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/api/administration/identity-provider", nil)
	get.AddCookie(cookie)
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("OIDC user GET admin endpoint status=%d, want 200", getResponse.Code)
	}
}

func TestIdentityProviderAdminSaveAndDeleteUsesStoredClientSecret(t *testing.T) {
	k8s := newIdentityProviderClient(t)
	handler := newLocalAdminHandlerWithClient(t, k8s)
	cookie, csrf := localAdminSession(t, handler)
	provider := newFakeOIDCProvider(t, []string{loginGroup, adminGroup})

	put := httptest.NewRecorder()
	handler.ServeHTTP(put, identityProviderRequestFor(http.MethodPut, `{"issuerURL":"`+provider.server.URL+`","clientID":"`+testOIDCClientID+`","clientSecret":"first-secret"}`, cookie, csrf))
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", put.Code, put.Body)
	}
	// A second PUT without a client secret keeps the stored one.
	update := httptest.NewRecorder()
	handler.ServeHTTP(update, identityProviderRequestFor(http.MethodPut, `{"issuerURL":"`+provider.server.URL+`","clientID":"`+testOIDCClientID+`"}`, cookie, csrf))
	if update.Code != http.StatusOK {
		t.Fatalf("PUT without secret status=%d body=%s", update.Code, update.Body)
	}
	config, ok, err := loadIdentityProvider(context.Background(), k8s, testNamespace, testIdentityProviderSecretName)
	if err != nil || !ok || config.ClientSecret != "first-secret" {
		t.Fatalf("stored client secret was not preserved: ok=%t err=%v config=%+v", ok, err, config)
	}
}

func TestLoadAndBuildIdentityProviderReportsPresence(t *testing.T) {
	provider := newFakeOIDCProvider(t, nil)
	defer provider.server.Close()
	publicURL, err := url.Parse("http://" + testDashboardHost)
	if err != nil {
		t.Fatal(err)
	}
	secret := func(issuer string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: testIdentityProviderSecretName, Namespace: testNamespace},
			Data: map[string][]byte{
				identityProviderIssuerKey:       []byte(issuer),
				identityProviderClientIDKey:     []byte(testOIDCClientID),
				identityProviderClientSecretKey: []byte("secret"),
			},
		}
	}
	newHandler := func(objects ...client.Object) *authHandler {
		return &authHandler{
			k8s:                    newIdentityProviderClient(t, objects...),
			namespace:              testNamespace,
			identityProviderSecret: testIdentityProviderSecretName,
			publicURL:              publicURL,
		}
	}

	// No stored configuration: nothing to retry.
	if built, present := newHandler().loadAndBuildIdentityProvider(context.Background()); built || present {
		t.Fatalf("empty config: built=%v present=%v", built, present)
	}

	// Unreachable issuer: the configuration is present, so callers keep retrying.
	unreachable := newHandler(secret("http://127.0.0.1:1/realms/releaseyard"))
	if built, present := unreachable.loadAndBuildIdentityProvider(context.Background()); built || !present {
		t.Fatalf("unreachable issuer: built=%v present=%v", built, present)
	}
	if unreachable.oidcAvailable() {
		t.Fatal("OIDC must stay disabled while discovery fails")
	}

	// Reachable issuer: the runtime is built.
	reachable := newHandler(secret(provider.server.URL))
	if built, present := reachable.loadAndBuildIdentityProvider(context.Background()); !built || !present {
		t.Fatalf("reachable issuer: built=%v present=%v", built, present)
	}
	if !reachable.oidcAvailable() {
		t.Fatal("OIDC must be enabled after a successful discovery")
	}
}
