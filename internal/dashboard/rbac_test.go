package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestWriteUsesImpersonatedClient proves that state-changing requests are
// executed with a client that impersonates the logged-in user, so the
// management cluster's RBAC (not the dashboard) decides the outcome. The
// impersonated client is deliberately empty, so deleting an existing
// Application returns NotFound instead of succeeding with the ServiceAccount.
func TestWriteUsesImpersonatedClient(t *testing.T) {
	app := &platformv1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "sample", Namespace: testNamespace}}
	_, k8s := testHandler(t, app)

	provider := newFakeOIDCProvider(t, []string{"releaseyard:developers"})
	var gotUser string
	var gotGroups []string
	factoryCalls := 0
	factory := func(user string, groups []string) (client.Client, error) {
		factoryCalls++
		gotUser = user
		gotGroups = groups
		return newIdentityProviderClient(t), nil
	}

	publicURL, err := url.Parse("http://" + testDashboardHost)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := url.Parse(provider.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	redirect, err := url.Parse("http://" + testDashboardHost + "/auth/callback")
	if err != nil {
		t.Fatal(err)
	}

	auth := newAuthHandler(k8s, testNamespace, publicURL, "", "", factory, "", "", nil)
	discoveredRuntime, err := discoverOIDCRuntime(context.Background(), issuer, redirect, testOIDCClientID, "secret")
	if err != nil {
		t.Fatalf("discover runtime: %v", err)
	}
	auth.oidc.Store(discoveredRuntime)
	handler := securityHeaders(auth.middleware(NewHandler(k8s, testNamespace)))

	login := beginTestLogin(t, handler, provider)
	callback := finishTestLogin(t, handler, login)
	if callback.Code != http.StatusSeeOther {
		t.Fatalf("callback status=%d", callback.Code)
	}
	var cookie *http.Cookie
	for _, c := range callback.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("callback did not issue a session cookie")
	}

	sessionRequest := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/api/session", nil)
	sessionRequest.AddCookie(cookie)
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, sessionRequest)
	var session struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &session); err != nil || session.CSRFToken == "" {
		t.Fatalf("GET /api/session body=%s err=%v", sessionResponse.Body, err)
	}

	request := httptest.NewRequest(http.MethodDelete, "http://"+testDashboardHost+"/api/applications/sample", nil)
	request.AddCookie(cookie)
	request.Header.Set("Origin", "http://"+testDashboardHost)
	request.Header.Set("X-CSRF-Token", session.CSRFToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("DELETE status=%d body=%s, want 404 from the impersonated client", response.Code, response.Body)
	}
	if factoryCalls == 0 || gotUser != "user-123" {
		t.Fatalf("impersonation factory calls=%d user=%q", factoryCalls, gotUser)
	}
	if len(gotGroups) != 2 || gotGroups[0] != authenticatedGroup || gotGroups[1] != "releaseyard:developers" {
		t.Fatalf("unexpected impersonation groups: %v", gotGroups)
	}
}

// TestOIDCAdminGateFollowsClusterRBAC proves that an OIDC user reaches the
// Administration area only when the management cluster's RBAC already allows
// the actions the panel performs, checked with a SelfSubjectAccessReview as the
// impersonated user.
func TestOIDCAdminGateFollowsClusterRBAC(t *testing.T) {
	for _, tc := range []struct {
		name       string
		allowAdmin bool
		wantStatus int
	}{
		{name: "with permission", allowAdmin: true, wantStatus: http.StatusOK},
		{name: "without permission", allowAdmin: false, wantStatus: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := newFakeOIDCProvider(t, []string{"releaseyard:administrators"})

			scheme := runtime.NewScheme()
			if err := platformv1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := authorizationv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			impersonated := fake.NewClientBuilder().WithScheme(scheme).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						if review, ok := obj.(*authorizationv1.SelfSubjectAccessReview); ok {
							attrs := review.Spec.ResourceAttributes
							review.Status.Allowed = tc.allowAdmin && attrs != nil &&
								attrs.Group == "rbac.authorization.k8s.io" && attrs.Resource == "clusterrolebindings" && attrs.Verb == "create"
							return nil
						}
						return c.Create(ctx, obj, opts...)
					},
				}).Build()
			factory := func(string, []string) (client.Client, error) { return impersonated, nil }

			k8s := newIdentityProviderClient(t)
			publicURL, err := url.Parse("http://" + testDashboardHost)
			if err != nil {
				t.Fatal(err)
			}
			issuer, err := url.Parse(provider.server.URL)
			if err != nil {
				t.Fatal(err)
			}
			redirect, err := url.Parse("http://" + testDashboardHost + "/auth/callback")
			if err != nil {
				t.Fatal(err)
			}
			auth := newAuthHandler(k8s, testNamespace, publicURL, "", "", factory, "", "", nil)
			rt, err := discoverOIDCRuntime(context.Background(), issuer, redirect, testOIDCClientID, "secret")
			if err != nil {
				t.Fatalf("discover runtime: %v", err)
			}
			auth.oidc.Store(rt)
			handler := securityHeaders(auth.middleware(NewHandler(k8s, testNamespace)))

			login := beginTestLogin(t, handler, provider)
			callback := finishTestLogin(t, handler, login)
			var cookie *http.Cookie
			for _, c := range callback.Result().Cookies() {
				if c.Name == sessionCookie {
					cookie = c
				}
			}
			if cookie == nil {
				t.Fatal("callback did not issue a session cookie")
			}

			request := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+"/api/administration/identity-provider", nil)
			request.AddCookie(cookie)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.wantStatus {
				t.Fatalf("admin endpoint status=%d, want %d body=%s", response.Code, tc.wantStatus, response.Body)
			}
		})
	}
}

// TestOIDCAuthorizationCaching proves that expensive authorization reviews are
// skipped for static assets, cached for normal API requests, and refreshed for
// administration requests.
func TestOIDCAuthorizationCaching(t *testing.T) {
	provider := newFakeOIDCProvider(t, []string{"releaseyard:developers"})

	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := authorizationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	var reviews int
	impersonated := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if review, ok := obj.(*authorizationv1.SelfSubjectAccessReview); ok {
					reviews++
					review.Status.Allowed = true
					return nil
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	factory := func(string, []string) (client.Client, error) { return impersonated, nil }

	k8s := newIdentityProviderClient(t)
	publicURL, err := url.Parse("http://" + testDashboardHost)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := url.Parse(provider.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	redirect, err := url.Parse("http://" + testDashboardHost + "/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	auth := newAuthHandler(k8s, testNamespace, publicURL, "", "", factory, "", "", nil)
	rt, err := discoverOIDCRuntime(context.Background(), issuer, redirect, testOIDCClientID, "secret")
	if err != nil {
		t.Fatalf("discover runtime: %v", err)
	}
	auth.oidc.Store(rt)
	handler := securityHeaders(auth.middleware(NewHandler(k8s, testNamespace)))

	login := beginTestLogin(t, handler, provider)
	callback := finishTestLogin(t, handler, login)
	var cookie *http.Cookie
	for _, c := range callback.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("callback did not issue a session cookie")
	}
	request := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://"+testDashboardHost+target, nil)
		req.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}

	if response := request("/app.js"); response.Code != http.StatusOK {
		t.Fatalf("static asset status=%d body=%s", response.Code, response.Body)
	}
	if reviews != 0 {
		t.Fatalf("static asset issued %d authorization reviews, want 0", reviews)
	}

	if response := request("/api/session"); response.Code != http.StatusOK {
		t.Fatalf("first /api/session status=%d body=%s", response.Code, response.Body)
	}
	first := reviews
	if first == 0 {
		t.Fatal("first /api/session did not issue authorization reviews")
	}

	if response := request("/api/session"); response.Code != http.StatusOK {
		t.Fatalf("second /api/session status=%d body=%s", response.Code, response.Body)
	}
	if reviews != first {
		t.Fatalf("second /api/session issued %d extra authorization reviews, want 0", reviews-first)
	}

	if response := request("/api/administration/identity-provider"); response.Code != http.StatusOK {
		t.Fatalf("admin endpoint status=%d body=%s", response.Code, response.Body)
	}
	if reviews <= first {
		t.Fatal("admin endpoint did not refresh authorization reviews")
	}
}
