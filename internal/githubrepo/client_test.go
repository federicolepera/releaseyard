package githubrepo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testToken = "super-secret-token"

func TestCreateRepositoryForUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.EscapedPath() == "/user":
			checkHeaders(t, r, false)
			_, _ = w.Write([]byte(`{"login":"team"}`))
		case r.Method == http.MethodPost && r.URL.EscapedPath() == "/user/repos":
			checkHeaders(t, r, true)
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["name"] != "new-repo" || payload["description"] != "description" || payload["private"] != true || payload["auto_init"] != false || payload["has_issues"] != false {
				t.Errorf("unexpected payload: %#v", payload)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":42,"name":"new-repo","full_name":"team/new-repo","html_url":"https://github.com/team/new-repo","clone_url":"https://github.com/team/new-repo.git","ssh_url":"git@github.com:team/new-repo.git","default_branch":"main","private":true,"description":"description","owner":{"login":"team"}}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
		}
	}))
	defer srv.Close()
	repo, err := testClient(t, srv.URL, nil).CreateRepository(context.Background(), CreateRepositoryRequest{Owner: "team", Name: "new-repo", Description: "description"})
	if err != nil {
		t.Fatal(err)
	}
	if repo.ID != 42 || repo.Owner != "team" || repo.Name != "new-repo" || repo.FullName != "team/new-repo" || repo.HTMLURL != "https://github.com/team/new-repo" || repo.CloneURL != "https://github.com/team/new-repo.git" || repo.SSHURL != "git@github.com:team/new-repo.git" || repo.DefaultBranch != "main" || !repo.Private {
		t.Fatalf("unexpected repo: %#v", repo)
	}
}

func TestCreateRepositoryForOrganization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.EscapedPath() == "/user":
			_, _ = w.Write([]byte(`{"login":"team"}`))
		case r.Method == http.MethodPost && r.URL.EscapedPath() == "/orgs/acme/repos":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":7,"name":"repo","full_name":"acme/repo","html_url":"https://github.com/acme/repo","clone_url":"https://github.com/acme/repo.git","ssh_url":"git@github.com:acme/repo.git","default_branch":"main","private":true,"owner":{"login":"acme"}}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
		}
	}))
	defer srv.Close()
	repo, err := testClient(t, srv.URL, nil).CreateRepository(context.Background(), CreateRepositoryRequest{Owner: "acme", Name: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if repo.Owner != "acme" || repo.Name != "repo" {
		t.Fatalf("unexpected repo: %#v", repo)
	}
}

func TestSeedRepositoryCreatesFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.EscapedPath() == "/repos/acme/demo/contents/apps/kustomization.yaml":
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPut && r.URL.EscapedPath() == "/repos/acme/demo/contents/apps/kustomization.yaml":
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["content"] != "aGVsbG8=" || payload["branch"] != "main" || payload["sha"] != "" {
				t.Errorf("unexpected payload: %#v", payload)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"content":{"path":"apps/kustomization.yaml"}}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
		}
	}))
	defer srv.Close()
	if err := testClient(t, srv.URL, nil).SeedRepository(context.Background(), "acme", "demo", "main", map[string]string{"apps/kustomization.yaml": "hello"}); err != nil {
		t.Fatal(err)
	}
}

func TestSeedRepositoryUpdatesExistingFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.EscapedPath() == "/repos/acme/demo/contents/README.md":
			_, _ = w.Write([]byte(`{"sha":"file-sha"}`))
		case r.Method == http.MethodPut && r.URL.EscapedPath() == "/repos/acme/demo/contents/README.md":
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["sha"] != "file-sha" || payload["content"] != "aGVsbG8=" {
				t.Errorf("unexpected payload: %#v", payload)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"content":{"path":"README.md"}}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
		}
	}))
	defer srv.Close()
	if err := testClient(t, srv.URL, nil).SeedRepository(context.Background(), "acme", "demo", "main", map[string]string{"README.md": "hello"}); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultTimeoutAndBaseURLValidation(t *testing.T) {
	c := NewClient(testToken)
	if c.http.Timeout != defaultTimeout || c.http.Timeout <= 0 {
		t.Fatalf("default timeout = %v, want %v", c.http.Timeout, defaultTimeout)
	}
	for _, base := range []string{"", "not a URL", "http://example.com", "https://user:pass@example.com", "https://example.com?query=1", "https://example.com#fragment"} {
		if _, err := NewClientWithOptions(testToken, base, nil); err == nil {
			t.Errorf("NewClientWithOptions(%q) unexpectedly succeeded", base)
		}
	}
	if _, err := NewClientWithOptions(testToken, "http://localhost:8080", nil); err != nil {
		t.Fatalf("loopback HTTP test URL rejected: %v", err)
	}
}

func TestOnlyExpectedSuccessStatusesAccepted(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Client) error
	}{
		{name: "login", call: func(c *Client) error {
			_, err := c.AuthenticatedLogin(context.Background())
			return err
		}},
		{name: "repo", call: func(c *Client) error {
			_, err := c.GetRepository(context.Background(), "owner", "repo")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
			defer srv.Close()
			err := tc.call(testClient(t, srv.URL, nil))
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNoContent || apiErr.Kind != ErrorUnknown {
				t.Fatalf("expected typed unexpected-status error, got %v", err)
			}
		})
	}
}

func TestGetRepositoryRequestAndResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/repos/a%2Fb/repo%20name" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
		}
		checkHeaders(t, r, false)
		_, _ = w.Write([]byte(`{"id":7,"name":"repo name","full_name":"a/repo name","clone_url":"https://github.com/a/repo-name.git","ssh_url":"git@github.com:a/repo-name.git","default_branch":"main","owner":{"login":"a"}}`))
	}))
	defer srv.Close()
	repo, err := testClient(t, srv.URL, nil).GetRepository(context.Background(), "a/b", "repo name")
	if err != nil || repo.ID != 7 || repo.Owner != "a" || repo.CloneURL == "" || repo.SSHURL == "" || repo.DefaultBranch != "main" {
		t.Fatalf("repo=%#v err=%v", repo, err)
	}
}

func TestAPIErrorCategoriesAndSanitization(t *testing.T) {
	cases := []struct {
		status  int
		headers map[string]string
		want    ErrorKind
	}{
		{403, nil, ErrorForbidden}, {404, nil, ErrorNotFound}, {422, nil, ErrorConflict}, {409, nil, ErrorConflict}, {500, nil, ErrorTransient}, {429, nil, ErrorRateLimited},
		{403, map[string]string{"X-RateLimit-Remaining": "0"}, ErrorRateLimited},
	}
	for _, tc := range cases {
		t.Run(string(tc.want)+"-"+strconv.Itoa(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("provider says " + testToken))
			}))
			defer srv.Close()
			_, err := testClient(t, srv.URL, nil).GetRepository(context.Background(), "o", "r")
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Kind != tc.want {
				t.Fatalf("error=%#v", err)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatalf("error leaked token: %v", err)
			}
		})
	}
}

func TestTransportTimeoutDoesNotLeakToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(100 * time.Millisecond) }))
	defer srv.Close()
	hc := &http.Client{Timeout: 10 * time.Millisecond}
	_, err := testClient(t, srv.URL, hc).GetRepository(context.Background(), "o", "r")
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func checkHeaders(t *testing.T, r *http.Request, contentType bool) {
	t.Helper()
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		t.Errorf("missing bearer auth")
	}
	if r.Header.Get("Accept") != "application/vnd.github+json" {
		t.Errorf("bad Accept: %q", r.Header.Get("Accept"))
	}
	if r.Header.Get("X-GitHub-Api-Version") != apiVersion {
		t.Errorf("bad API version: %q", r.Header.Get("X-GitHub-Api-Version"))
	}
	if contentType && r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("bad Content-Type: %q", r.Header.Get("Content-Type"))
	}
}

func testClient(t *testing.T, base string, hc *http.Client) *Client {
	t.Helper()
	c, err := NewClientWithOptions(testToken, base, hc)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
