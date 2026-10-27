// Package githubrepo provides a small GitHub REST API client for repository
// generation and lookup.
package githubrepo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.github.com"
	apiVersion     = "2026-03-10"
	maxBodyBytes   = 1 << 20
	defaultTimeout = 10 * time.Second
)

// ErrorKind classifies errors returned by the GitHub API.
type ErrorKind string

const (
	ErrorUnknown     ErrorKind = "unknown"
	ErrorNotFound    ErrorKind = "not_found"
	ErrorForbidden   ErrorKind = "forbidden"
	ErrorConflict    ErrorKind = "conflict_or_unprocessable"
	ErrorTransient   ErrorKind = "transient"
	ErrorRateLimited ErrorKind = "rate_limited"
)

// APIError reports a safe status classification. It deliberately excludes
// response content and request headers, which may contain sensitive data.
type APIError struct {
	StatusCode int
	Kind       ErrorKind
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub API request failed: %s (HTTP %d)", e.Kind, e.StatusCode)
}

// Is allows errors.Is to match the classification sentinel errors.
func (e *APIError) Is(target error) bool {
	t, ok := target.(*APIError)
	return ok && e.Kind == t.Kind
}

var (
	ErrNotFound    = &APIError{Kind: ErrorNotFound}
	ErrForbidden   = &APIError{Kind: ErrorForbidden}
	ErrConflict    = &APIError{Kind: ErrorConflict}
	ErrTransient   = &APIError{Kind: ErrorTransient}
	ErrRateLimited = &APIError{Kind: ErrorRateLimited}
)

// Repository contains the repository fields used by callers.
type Repository struct {
	ID            int64  `json:"id"`
	Owner         string `json:"-"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	HTMLURL       string `json:"html_url"`
	CloneURL      string `json:"clone_url"`
	SSHURL        string `json:"ssh_url"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Description   string `json:"description"`
}

type repoResponse struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	HTMLURL       string `json:"html_url"`
	CloneURL      string `json:"clone_url"`
	SSHURL        string `json:"ssh_url"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Description   string `json:"description"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// Client makes GitHub REST API calls. It does not retry mutations.
type Client struct {
	token   string
	baseURL *url.URL
	http    *http.Client
}

// NewClient creates a production client using api.github.com.
func NewClient(token string) *Client {
	c, _ := NewClientWithOptions(token, defaultBaseURL, nil)
	return c
}

// NewClientWithOptions creates a client with injectable API base URL and HTTP
// client. It is intended primarily for tests; credentials are still sent only
// to the configured endpoint. Redirects are always disabled.
func NewClientWithOptions(token, baseURL string, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("invalid GitHub API base URL")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return nil, errors.New("GitHub API base URL must use HTTPS except for loopback test servers")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	copyClient := *httpClient
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{token: token, baseURL: u, http: &copyClient}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func escapeSegment(s string) string { return url.PathEscape(s) }

func escapePath(path string) string {
	parts := strings.Split(path, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// CreateRepositoryRequest specifies the destination repository.
type CreateRepositoryRequest struct {
	Owner       string
	Name        string
	Description string
}

// CreateRepository creates a private repository for the authenticated user or
// the requested organization. The repository is initialized separately.
func (c *Client) CreateRepository(ctx context.Context, input CreateRepositoryRequest) (Repository, error) {
	var result Repository
	if input.Owner == "" || input.Name == "" {
		return result, errors.New("owner and repository name are required")
	}
	login, err := c.AuthenticatedLogin(ctx)
	if err != nil {
		return result, err
	}
	path := "/orgs/" + escapeSegment(input.Owner) + "/repos"
	if strings.EqualFold(login, input.Owner) {
		path = "/user/repos"
	}
	body, err := json.Marshal(struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Private     bool   `json:"private"`
		AutoInit    bool   `json:"auto_init"`
		HasIssues   bool   `json:"has_issues"`
		HasWiki     bool   `json:"has_wiki"`
		HasProjects bool   `json:"has_projects"`
	}{input.Name, input.Description, true, false, false, false, false})
	if err != nil {
		return result, errors.New("could not encode GitHub request")
	}
	repo, err := c.doRepository(ctx, http.MethodPost, path, body)
	if err != nil {
		return result, err
	}
	return toRepository(repo), nil
}

// AuthenticatedLogin returns the login of the token owner.
func (c *Client) AuthenticatedLogin(ctx context.Context) (string, error) {
	data, err := c.do(ctx, http.MethodGet, "/user", nil)
	if err != nil {
		return "", err
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(data, &user); err != nil || user.Login == "" {
		return "", errors.New("invalid GitHub API response")
	}
	return user.Login, nil
}

// SeedRepository creates or updates the provided files through the Contents
// API. It is idempotent and works on an empty repository.
func (c *Client) SeedRepository(ctx context.Context, owner, name, branch string, files map[string]string) error {
	if owner == "" || name == "" {
		return errors.New("owner and repository name are required")
	}
	if branch == "" {
		branch = "main"
	}
	for path, content := range files {
		sha := ""
		data, err := c.do(ctx, http.MethodGet, "/repos/"+escapeSegment(owner)+"/"+escapeSegment(name)+"/contents/"+escapePath(path), nil)
		if err == nil {
			var existing struct {
				SHA string `json:"sha"`
			}
			if err := json.Unmarshal(data, &existing); err != nil || existing.SHA == "" {
				return errors.New("invalid GitHub API response")
			}
			sha = existing.SHA
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		payload := map[string]string{
			"message": "Initialize ReleaseYard project",
			"content": base64.StdEncoding.EncodeToString([]byte(content)),
			"branch":  branch,
		}
		if sha != "" {
			payload["sha"] = sha
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return errors.New("could not encode GitHub request")
		}
		if _, err := c.do(ctx, http.MethodPut, "/repos/"+escapeSegment(owner)+"/"+escapeSegment(name)+"/contents/"+escapePath(path), body); err != nil {
			return err
		}
	}
	return nil
}

// GetRepository retrieves a repository by owner and name.
func (c *Client) GetRepository(ctx context.Context, owner, name string) (Repository, error) {
	var result Repository
	if owner == "" || name == "" {
		return result, errors.New("owner and repository name are required")
	}
	repo, err := c.doRepository(ctx, http.MethodGet, "/repos/"+escapeSegment(owner)+"/"+escapeSegment(name), nil)
	if err != nil {
		return result, err
	}
	return toRepository(repo), nil
}

// File is a repository file read through the Contents API.
type File struct {
	Path    string
	SHA     string
	Content []byte
}

// PullRequest is the subset of a GitHub pull request used by the bridge.
type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
}

// CreatePullRequestInput describes a pull request to open.
type CreatePullRequestInput struct {
	Title string
	Head  string
	Base  string
	Body  string
}

// TreeEntry is a file written into a Git tree.
type TreeEntry struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	Type    string `json:"type"`
	Content string `json:"content"`
}

// GetBranchSHA returns the commit SHA at the head of a branch.
func (c *Client) GetBranchSHA(ctx context.Context, owner, name, branch string) (string, error) {
	data, err := c.do(ctx, http.MethodGet, "/repos/"+escapeSegment(owner)+"/"+escapeSegment(name)+"/git/ref/heads/"+escapePath(branch), nil)
	if err != nil {
		return "", err
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(data, &ref); err != nil || ref.Object.SHA == "" {
		return "", errors.New("invalid GitHub API response")
	}
	return ref.Object.SHA, nil
}

// GetCommitTreeSHA returns the tree SHA for a commit.
func (c *Client) GetCommitTreeSHA(ctx context.Context, owner, name, commitSHA string) (string, error) {
	data, err := c.do(ctx, http.MethodGet, "/repos/"+escapeSegment(owner)+"/"+escapeSegment(name)+"/git/commits/"+escapeSegment(commitSHA), nil)
	if err != nil {
		return "", err
	}
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := json.Unmarshal(data, &commit); err != nil || commit.Tree.SHA == "" {
		return "", errors.New("invalid GitHub API response")
	}
	return commit.Tree.SHA, nil
}

// GetFile returns a file at a ref through the Contents API.
func (c *Client) GetFile(ctx context.Context, owner, name, ref, path string) (File, error) {
	var result File
	query := url.Values{}
	if ref != "" {
		query.Set("ref", ref)
	}
	data, err := c.doQuery(ctx, http.MethodGet, "/repos/"+escapeSegment(owner)+"/"+escapeSegment(name)+"/contents/"+escapePath(path), query, nil)
	if err != nil {
		return result, err
	}
	var file struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		SHA      string `json:"sha"`
	}
	if err := json.Unmarshal(data, &file); err != nil || file.SHA == "" {
		return result, errors.New("invalid GitHub API response")
	}
	if file.Encoding != "base64" {
		return result, errors.New("unsupported GitHub file encoding")
	}
	content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return result, errors.New("invalid GitHub file content")
	}
	return File{Path: path, SHA: file.SHA, Content: content}, nil
}

// CreateTree creates a tree based on an existing tree.
func (c *Client) CreateTree(ctx context.Context, owner, name, baseTree string, entries []TreeEntry) (string, error) {
	body, err := json.Marshal(struct {
		BaseTree string      `json:"base_tree"`
		Tree     []TreeEntry `json:"tree"`
	}{BaseTree: baseTree, Tree: entries})
	if err != nil {
		return "", errors.New("could not encode GitHub request")
	}
	data, err := c.do(ctx, http.MethodPost, "/repos/"+escapeSegment(owner)+"/"+escapeSegment(name)+"/git/trees", body)
	if err != nil {
		return "", err
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(data, &tree); err != nil || tree.SHA == "" {
		return "", errors.New("invalid GitHub API response")
	}
	return tree.SHA, nil
}

// CreateCommit creates a commit with one parent.
func (c *Client) CreateCommit(ctx context.Context, owner, name, message, treeSHA, parentSHA string) (string, error) {
	body, err := json.Marshal(struct {
		Message string   `json:"message"`
		Tree    string   `json:"tree"`
		Parents []string `json:"parents"`
	}{Message: message, Tree: treeSHA, Parents: []string{parentSHA}})
	if err != nil {
		return "", errors.New("could not encode GitHub request")
	}
	data, err := c.do(ctx, http.MethodPost, "/repos/"+escapeSegment(owner)+"/"+escapeSegment(name)+"/git/commits", body)
	if err != nil {
		return "", err
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(data, &commit); err != nil || commit.SHA == "" {
		return "", errors.New("invalid GitHub API response")
	}
	return commit.SHA, nil
}

// CreateBranch creates a branch at a commit SHA.
func (c *Client) CreateBranch(ctx context.Context, owner, name, branch, sha string) error {
	body, err := json.Marshal(struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}{Ref: "refs/heads/" + branch, SHA: sha})
	if err != nil {
		return errors.New("could not encode GitHub request")
	}
	_, err = c.do(ctx, http.MethodPost, "/repos/"+escapeSegment(owner)+"/"+escapeSegment(name)+"/git/refs", body)
	return err
}

// FindOpenPullRequest returns the first open pull request whose head is branch.
func (c *Client) FindOpenPullRequest(ctx context.Context, owner, name, branch string) (PullRequest, error) {
	var result PullRequest
	query := url.Values{}
	query.Set("state", "open")
	query.Set("head", owner+":"+branch)
	data, err := c.doQuery(ctx, http.MethodGet, "/repos/"+escapeSegment(owner)+"/"+escapeSegment(name)+"/pulls", query, nil)
	if err != nil {
		return result, err
	}
	var pulls []PullRequest
	if err := json.Unmarshal(data, &pulls); err != nil {
		return result, errors.New("invalid GitHub API response")
	}
	if len(pulls) == 0 || pulls[0].HTMLURL == "" {
		return result, ErrNotFound
	}
	return pulls[0], nil
}

// CreatePullRequest opens a pull request.
func (c *Client) CreatePullRequest(ctx context.Context, owner, name string, input CreatePullRequestInput) (PullRequest, error) {
	var result PullRequest
	body, err := json.Marshal(struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body,omitempty"`
	}{Title: input.Title, Head: input.Head, Base: input.Base, Body: input.Body})
	if err != nil {
		return result, errors.New("could not encode GitHub request")
	}
	data, err := c.do(ctx, http.MethodPost, "/repos/"+escapeSegment(owner)+"/"+escapeSegment(name)+"/pulls", body)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(data, &result); err != nil || result.HTMLURL == "" {
		return PullRequest{}, errors.New("invalid GitHub API response")
	}
	return result, nil
}

func toRepository(r repoResponse) Repository {
	return Repository{
		ID:            r.ID,
		Owner:         r.Owner.Login,
		Name:          r.Name,
		FullName:      r.FullName,
		HTMLURL:       r.HTMLURL,
		CloneURL:      r.CloneURL,
		SSHURL:        r.SSHURL,
		DefaultBranch: r.DefaultBranch,
		Private:       r.Private,
		Description:   r.Description,
	}
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return c.doQuery(ctx, method, path, nil, body)
}

func (c *Client) doQuery(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, error) {
	u := *c.baseURL
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawPath = strings.TrimRight(c.baseURL.EscapedPath(), "/") + path
	// path contains escaped segment text, while Path must hold its decoded form.
	decoded, err := url.PathUnescape(u.RawPath)
	if err != nil {
		return nil, errors.New("invalid GitHub API path")
	}
	u.Path = decoded
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, errors.New("could not construct GitHub API request")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Do not wrap arbitrary transport errors: custom transports can include secrets.
		return nil, errors.New("GitHub API transport request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	limited := io.LimitReader(resp.Body, maxBodyBytes+1)
	data, readErr := io.ReadAll(limited)
	if readErr != nil {
		return nil, errors.New("could not read GitHub API response")
	}
	if len(data) > maxBodyBytes {
		return nil, errors.New("GitHub API response exceeded size limit")
	}
	okStatus := resp.StatusCode == http.StatusOK
	if method == http.MethodPost {
		okStatus = resp.StatusCode == http.StatusCreated
	}
	if method == http.MethodPut {
		okStatus = resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated
	}
	if !okStatus {
		return nil, classify(resp)
	}
	return data, nil
}

func (c *Client) doRepository(ctx context.Context, method, path string, body []byte) (repoResponse, error) {
	var result repoResponse
	data, err := c.do(ctx, method, path, body)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return result, errors.New("invalid GitHub API response")
	}
	return result, nil
}

func classify(resp *http.Response) error {
	kind := ErrorUnknown
	switch resp.StatusCode {
	case http.StatusNotFound:
		kind = ErrorNotFound
	case http.StatusForbidden:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" {
			kind = ErrorRateLimited
		} else {
			kind = ErrorForbidden
		}
	case http.StatusTooManyRequests:
		kind = ErrorRateLimited
	case http.StatusConflict, http.StatusUnprocessableEntity:
		kind = ErrorConflict
	default:
		if resp.StatusCode >= 500 {
			kind = ErrorTransient
		}
	}
	return &APIError{StatusCode: resp.StatusCode, Kind: kind}
}
