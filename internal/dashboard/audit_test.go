package dashboard

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestAuditLogger(buffer *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buffer, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func decodeAuditEvents(t *testing.T, buffer *bytes.Buffer) []map[string]any {
	t.Helper()
	var events []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buffer.String()), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode audit event %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func findAuditEvent(t *testing.T, events []map[string]any, name, outcome string) map[string]any {
	t.Helper()
	for _, event := range events {
		if event["event"] == name && event["outcome"] == outcome {
			return event
		}
	}
	t.Fatalf("audit event %s/%s not found in %+v", name, outcome, events)
	return nil
}

func TestAuditLogsFailedLocalLogin(t *testing.T) {
	var buffer bytes.Buffer
	k8s := newIdentityProviderClient(t)
	handler, err := NewLocalHandler(k8s, testNamespace, LocalAuthConfig{
		HashPath:     writeLocalAdminHash(t, testLocalAdminPassword, localAdminBcryptCost),
		PublicURL:    "http://" + testDashboardHost,
		AuditLogger:  newTestAuditLogger(&buffer),
		SessionStore: newMemorySessionStore(),
	})
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, loginRequest("wrong-password", "http://"+testDashboardHost, testDashboardHost))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("login status=%d body=%s", response.Code, response.Body)
	}
	if response.Header().Get("X-Request-ID") == "" {
		t.Fatal("failed login did not return a request ID")
	}

	event := findAuditEvent(t, decodeAuditEvents(t, &buffer), "auth.login.local", "denied")
	if event["actor"] != localAdminUsername || event["reason"] != "invalid_credentials" {
		t.Fatalf("unexpected failed-login audit event: %+v", event)
	}
	if event["request_id"] == "" || event["remote_addr"] == "" {
		t.Fatalf("audit event is missing request metadata: %+v", event)
	}
	if strings.Contains(buffer.String(), "wrong-password") {
		t.Fatal("audit log contains the attempted password")
	}
}

func TestAuditLogsOIDCLoginAndWrite(t *testing.T) {
	var buffer bytes.Buffer
	provider := newFakeOIDCProvider(t, []string{loginGroup})
	_, k8s := testHandler(t)
	handler, err := NewOIDCHandler(k8s, testNamespace, OIDCConfig{
		IssuerURL: provider.server.URL, ClientID: testOIDCClientID,
		ClientSecret: "test-client-secret", RedirectURL: "http://" + testDashboardHost + "/auth/callback",
		AuditLogger: newTestAuditLogger(&buffer),
	})
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}

	login := beginTestLogin(t, handler, provider)
	callback := finishTestLogin(t, handler, login)
	if callback.Code != http.StatusSeeOther {
		t.Fatalf("callback status=%d body=%s", callback.Code, callback.Body)
	}
	var sessionCookieValue *http.Cookie
	for _, cookie := range callback.Result().Cookies() {
		if cookie.Name == sessionCookie {
			sessionCookieValue = cookie
		}
	}
	if sessionCookieValue == nil {
		t.Fatal("callback did not issue a session cookie")
	}

	sessionRequest := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/api/session", nil)
	sessionRequest.AddCookie(sessionCookieValue)
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, sessionRequest)
	var sessionPayload struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &sessionPayload); err != nil || sessionPayload.CSRFToken == "" {
		t.Fatalf("read CSRF token: err=%v body=%s", err, sessionResponse.Body)
	}

	writeRequest := httptest.NewRequest(http.MethodPost, "http://"+testDashboardHost+"/api/applications", strings.NewReader("{}"))
	writeRequest.AddCookie(sessionCookieValue)
	writeRequest.Header.Set("Origin", "http://"+testDashboardHost)
	writeRequest.Header.Set("Content-Type", "application/json")
	writeRequest.Header.Set("X-CSRF-Token", sessionPayload.CSRFToken)
	writeResponse := httptest.NewRecorder()
	handler.ServeHTTP(writeResponse, writeRequest)
	if writeResponse.Code != http.StatusBadRequest {
		t.Fatalf("write status=%d body=%s", writeResponse.Code, writeResponse.Body)
	}

	events := decodeAuditEvents(t, &buffer)
	loginEvent := findAuditEvent(t, events, "auth.login.oidc", "success")
	if loginEvent["actor"] != "user-123" {
		t.Fatalf("unexpected OIDC login audit event: %+v", loginEvent)
	}
	writeEvent := findAuditEvent(t, events, "api.write", "failure")
	if writeEvent["actor"] != "user-123" || writeEvent["status"] != float64(http.StatusBadRequest) {
		t.Fatalf("unexpected write audit event: %+v", writeEvent)
	}
	groups, ok := writeEvent["groups"].([]any)
	if !ok || len(groups) != 1 || groups[0] != loginGroup {
		t.Fatalf("unexpected groups in write audit event: %+v", writeEvent["groups"])
	}
	if writeEvent["auth_method"] != string(authMethodOIDC) {
		t.Fatalf("unexpected auth method in write audit event: %+v", writeEvent)
	}
}

func TestAuditMarksImpersonatedWrites(t *testing.T) {
	var buffer bytes.Buffer
	auth := &authHandler{auditLog: newTestAuditLogger(&buffer)}
	request := httptest.NewRequest(http.MethodPost, "http://"+testDashboardHost+"/api/applications", nil)
	request = request.WithContext(withAuditRequestID(request.Context(), "request-1"))
	request = request.WithContext(withImpersonatingClient(request.Context(), newIdentityProviderClient(t)))

	auth.auditHTTP(request, dashboardSession{username: "user@example.com", groups: []string{loginGroup}, authMethod: authMethodOIDC}, true, http.StatusCreated)

	event := findAuditEvent(t, decodeAuditEvents(t, &buffer), "api.write", "success")
	if event["actor"] != "user@example.com" || event["impersonated"] != true {
		t.Fatalf("unexpected impersonated write audit event: %+v", event)
	}
}

func TestAuditEventForRequest(t *testing.T) {
	for _, test := range []struct {
		method string
		path   string
		want   string
	}{
		{http.MethodPut, "/api/administration/identity-provider", "admin.identity_provider.update"},
		{http.MethodDelete, "/api/administration/identity-provider", "admin.identity_provider.delete"},
		{http.MethodPut, "/api/administration/definition-access", "admin.definition_access.update"},
		{http.MethodDelete, "/api/administration/cluster-access", "admin.cluster_access.delete"},
		{http.MethodPut, "/api/administration/application-access", "admin.application_access.update"},
		{http.MethodPost, "/api/applications", "api.write"},
	} {
		if got := auditEventForRequest(test.method, test.path); got != test.want {
			t.Errorf("auditEventForRequest(%s, %s)=%q, want %q", test.method, test.path, got, test.want)
		}
	}
}
