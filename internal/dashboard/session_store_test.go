package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestRedisSessionStoreRoundTrip(t *testing.T) {
	store := newTestRedisSessionStore(t)
	ctx := context.Background()
	activity := time.Date(2026, time.October, 26, 10, 0, 0, 0, time.UTC)
	session := dashboardSession{
		issuer:       "https://issuer.example",
		subject:      "user-1",
		username:     "user@example.com",
		groups:       []string{"platform", "developers"},
		csrfToken:    "csrf",
		idToken:      "id-token",
		lastActivity: activity,
		expires:      activity.Add(8 * time.Hour),
		authMethod:   authMethodOIDC,
	}
	if err := store.PutSession(ctx, "session-1", session, time.Minute); err != nil {
		t.Fatalf("put session: %v", err)
	}
	got, ok, err := store.GetSession(ctx, "session-1")
	if err != nil || !ok {
		t.Fatalf("get session: ok=%t err=%v", ok, err)
	}
	if !reflect.DeepEqual(got, session) {
		t.Fatalf("session round trip mismatch: got=%+v want=%+v", got, session)
	}
	if err := store.DeleteSession(ctx, "session-1"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, ok, err := store.GetSession(ctx, "session-1"); err != nil || ok {
		t.Fatalf("deleted session still present: ok=%t err=%v", ok, err)
	}

	transaction := loginTransaction{state: "state", binding: "binding", nonce: "nonce", verifier: "verifier", expires: activity}
	if err := store.PutTransaction(ctx, "state", transaction, time.Minute); err != nil {
		t.Fatalf("put transaction: %v", err)
	}
	gotTransaction, ok, err := store.TakeTransaction(ctx, "state")
	if err != nil || !ok {
		t.Fatalf("take transaction: ok=%t err=%v", ok, err)
	}
	if !reflect.DeepEqual(gotTransaction, transaction) {
		t.Fatalf("transaction round trip mismatch: got=%+v want=%+v", gotTransaction, transaction)
	}
	if _, ok, err := store.TakeTransaction(ctx, "state"); err != nil || ok {
		t.Fatalf("transaction was not consumed: ok=%t err=%v", ok, err)
	}
}

func TestRedisSessionStoreTTL(t *testing.T) {
	store, redisServer := newTestRedisSessionStoreWithServer(t)
	ctx := context.Background()
	if err := store.PutSession(ctx, "session-1", dashboardSession{username: "user"}, time.Second); err != nil {
		t.Fatalf("put session: %v", err)
	}
	redisServer.FastForward(2 * time.Second)
	if _, ok, err := store.GetSession(ctx, "session-1"); err != nil || ok {
		t.Fatalf("expired session still present: ok=%t err=%v", ok, err)
	}
}

func TestSharedSessionStoreAcrossDashboardReplicas(t *testing.T) {
	for _, test := range []struct {
		name string
		new  func(*testing.T) SessionStore
	}{
		{name: "memory", new: func(*testing.T) SessionStore { return newMemorySessionStore() }},
		{name: "redis", new: func(t *testing.T) SessionStore { return newTestRedisSessionStore(t) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := newFakeOIDCProvider(t, []string{loginGroup})
			_, k8s := testHandler(t)
			store := test.new(t)
			config := OIDCConfig{
				IssuerURL: provider.server.URL, ClientID: testOIDCClientID,
				ClientSecret: "test-client-secret", RedirectURL: "http://" + testDashboardHost + "/auth/callback",
				SessionStore: store,
			}
			first, err := NewOIDCHandler(k8s, testNamespace, config)
			if err != nil {
				t.Fatalf("construct first handler: %v", err)
			}
			second, err := NewOIDCHandler(k8s, testNamespace, config)
			if err != nil {
				t.Fatalf("construct second handler: %v", err)
			}

			login := beginTestLogin(t, first, provider)
			callback := finishTestLogin(t, second, login)
			if callback.Code != http.StatusSeeOther {
				t.Fatalf("callback through second replica status=%d body=%s", callback.Code, callback.Body)
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
			second.ServeHTTP(sessionResponse, sessionRequest)
			if sessionResponse.Code != http.StatusOK {
				t.Fatalf("session on second replica status=%d body=%s", sessionResponse.Code, sessionResponse.Body)
			}
			var payload struct {
				CSRFToken string `json:"csrfToken"`
			}
			if err := json.Unmarshal(sessionResponse.Body.Bytes(), &payload); err != nil || payload.CSRFToken == "" {
				t.Fatalf("decode session response: err=%v body=%s", err, sessionResponse.Body)
			}

			logout := httptest.NewRequest(http.MethodPost, "http://"+testDashboardHost+"/auth/logout", nil)
			logout.AddCookie(sessionCookieValue)
			logout.Header.Set("Origin", "http://"+testDashboardHost)
			logout.Header.Set("X-CSRF-Token", payload.CSRFToken)
			logoutResponse := httptest.NewRecorder()
			first.ServeHTTP(logoutResponse, logout)
			if logoutResponse.Code != http.StatusOK {
				t.Fatalf("logout through first replica status=%d body=%s", logoutResponse.Code, logoutResponse.Body)
			}

			afterLogout := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/api/session", nil)
			afterLogout.AddCookie(sessionCookieValue)
			afterLogoutResponse := httptest.NewRecorder()
			second.ServeHTTP(afterLogoutResponse, afterLogout)
			if afterLogoutResponse.Code != http.StatusUnauthorized {
				t.Fatalf("session survived logout on the other replica: status=%d", afterLogoutResponse.Code)
			}
		})
	}
}

func TestLocalSessionSharedAcrossHandlers(t *testing.T) {
	store := newMemorySessionStore()
	k8s := newIdentityProviderClient(t)
	config := LocalAuthConfig{
		HashPath:     writeLocalAdminHash(t, testLocalAdminPassword, localAdminBcryptCost),
		PublicURL:    "http://" + testDashboardHost,
		SessionStore: store,
	}
	first, err := NewLocalHandler(k8s, testNamespace, config)
	if err != nil {
		t.Fatalf("construct first handler: %v", err)
	}
	second, err := NewLocalHandler(k8s, testNamespace, config)
	if err != nil {
		t.Fatalf("construct second handler: %v", err)
	}

	login := httptest.NewRecorder()
	first.ServeHTTP(login, loginRequest(testLocalAdminPassword, "http://"+testDashboardHost, testDashboardHost))
	cookie := getSessionCookie(t, login)
	if cookie == nil {
		t.Fatalf("login did not issue a session: %d %s", login.Code, login.Body)
	}

	sessionRequest := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/api/session", nil)
	sessionRequest.AddCookie(cookie)
	sessionResponse := httptest.NewRecorder()
	second.ServeHTTP(sessionResponse, sessionRequest)
	if sessionResponse.Code != http.StatusOK {
		t.Fatalf("session on second handler status=%d body=%s", sessionResponse.Code, sessionResponse.Body)
	}
}

func newTestRedisSessionStore(t *testing.T) *RedisSessionStore {
	t.Helper()
	store, _ := newTestRedisSessionStoreWithServer(t)
	return store
}

func newTestRedisSessionStoreWithServer(t *testing.T) (*RedisSessionStore, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
	})
	return NewRedisSessionStore(client, "test:dashboard:"), server
}
