/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package chartfetch retrieves and verifies opaque HTTP Helm chart archives.
package chartfetch

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

const (
	maxIndexBytes    = int64(2 << 20)
	maxArchiveBytes  = int64(50 << 20)
	operationTimeout = 30 * time.Second
	httpTimeout      = 15 * time.Second
	httpScheme       = "http"
	httpsScheme      = "https"
)

var (
	errInvalidURL = errors.New("invalid chart repository URL")
	errTooLarge   = errors.New("chart response exceeds size limit")
)

// Puller fetches HTTP Helm indexes and chart archives from explicitly allowed hosts.
type Puller struct {
	allowedHosts map[string]struct{}
	client       *http.Client
}

// NewPuller constructs a puller limited to the supplied DNS hosts/IPs, optionally
// including a port. The allowlist is the trust boundary: HTTP and HTTPS are both
// permitted, but only for allowlisted authorities.
func NewPuller(allowedHosts []string) (*Puller, error) {
	hosts := make(map[string]struct{}, len(allowedHosts))
	for _, value := range allowedHosts {
		host, port, err := parseAuthority(value)
		if err != nil {
			return nil, fmt.Errorf("invalid allowed chart host")
		}
		hosts[authority(host, port)] = struct{}{}
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("chart host allowlist must not be empty")
	}
	transport := &http.Transport{
		Proxy: nil, DisableCompression: true,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second,
		IdleConnTimeout: 30 * time.Second, MaxIdleConns: 16, MaxIdleConnsPerHost: 4,
	}
	return &Puller{
		allowedHosts: hosts,
		client: &http.Client{
			Transport: &safeTransport{hosts: hosts, base: transport}, Timeout: httpTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Pull returns the SHA-256 digest of the downloaded chart archive without
// unpacking or interpreting it.
func (p *Puller) Pull(ctx context.Context, chart platformv1alpha1.ApplicationDefinitionChart) (string, error) {
	_, digest, err := p.PullVerified(ctx, chart)
	return digest, err
}

// PullVerified returns the downloaded chart archive and its verified SHA-256
// digest without unpacking or interpreting the archive.
func (p *Puller) PullVerified(ctx context.Context, chart platformv1alpha1.ApplicationDefinitionChart) ([]byte, string, error) {
	if p == nil || p.client == nil || chart.Name == "" || chart.Version == "" {
		return nil, "", fmt.Errorf("invalid chart request")
	}
	base, err := parseBaseURL(chart.Repository, p.allowedHosts)
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()

	indexURL := *base
	indexURL.Path = strings.TrimSuffix(indexURL.Path, "/") + "/index.yaml"
	indexBytes, err := p.get(ctx, &indexURL, maxIndexBytes)
	if err != nil {
		return nil, "", fmt.Errorf("chart index could not be retrieved")
	}
	var index struct {
		Entries map[string][]struct {
			Version string   `json:"version"`
			URLs    []string `json:"urls"`
			Digest  string   `json:"digest"`
		} `json:"entries"`
	}
	if err := yaml.Unmarshal(indexBytes, &index); err != nil {
		return nil, "", fmt.Errorf("chart index is invalid")
	}
	var selected *struct {
		Version string   `json:"version"`
		URLs    []string `json:"urls"`
		Digest  string   `json:"digest"`
	}
	for i := range index.Entries[chart.Name] {
		entry := &index.Entries[chart.Name][i]
		if entry.Version == chart.Version {
			selected = entry
			break
		}
	}
	if selected == nil || len(selected.URLs) == 0 {
		return nil, "", fmt.Errorf("requested chart version was not found")
	}
	if len(selected.URLs) != 1 {
		return nil, "", fmt.Errorf("chart index has ambiguous archive URLs")
	}
	expectedDigest, err := parseSHA256Digest(selected.Digest)
	if err != nil {
		return nil, "", fmt.Errorf("chart index digest is invalid")
	}
	archiveURL, err := resolveArchiveURL(&indexURL, selected.URLs[0], p.allowedHosts)
	if err != nil {
		return nil, "", fmt.Errorf("chart archive URL is invalid")
	}
	archive, err := p.get(ctx, archiveURL, maxArchiveBytes)
	if err != nil {
		return nil, "", fmt.Errorf("chart archive could not be retrieved")
	}
	actual := sha256.Sum256(archive)
	if !equalBytes(actual[:], expectedDigest) {
		return nil, "", fmt.Errorf("chart archive digest does not match index")
	}
	return archive, "sha256:" + hex.EncodeToString(actual[:]), nil
}

func parseSHA256Digest(digest string) ([]byte, error) {
	digest = strings.TrimPrefix(digest, "sha256:")
	if len(digest) != sha256.Size*2 {
		return nil, fmt.Errorf("invalid SHA-256 digest")
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size {
		return nil, fmt.Errorf("invalid SHA-256 digest")
	}
	return decoded, nil
}

func (p *Puller) get(ctx context.Context, target *url.URL, maxBytes int64) ([]byte, error) {
	if err := validateTarget(target, p.allowedHosts); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errTooLarge
	}
	return data, nil
}

func parseBaseURL(raw string, allowed map[string]struct{}) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Host == "" {
		return nil, errInvalidURL
	}
	if (u.Scheme != httpScheme && u.Scheme != httpsScheme) || strings.Contains(u.Host, "\\") || strings.Contains(u.EscapedPath(), "%") {
		return nil, errInvalidURL
	}
	if err := validateTarget(u, allowed); err != nil {
		return nil, errInvalidURL
	}
	if hasTraversal(u.Path) || strings.Contains(u.Path, "//") {
		return nil, errInvalidURL
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u, nil
}

func resolveArchiveURL(indexURL *url.URL, raw string, allowed map[string]struct{}) (*url.URL, error) {
	if strings.TrimSpace(raw) != raw || raw == "" || strings.Contains(raw, "\\") {
		return nil, errInvalidURL
	}
	ref, err := url.Parse(raw)
	if err != nil || ref.User != nil || ref.RawQuery != "" || ref.ForceQuery || ref.Fragment != "" || ref.RawFragment != "" || hasTraversal(ref.Path) {
		return nil, errInvalidURL
	}
	resolved := indexURL.ResolveReference(ref)
	if resolved.Scheme != indexURL.Scheme || !strings.EqualFold(resolved.Host, indexURL.Host) || hasTraversal(resolved.Path) || strings.Contains(resolved.EscapedPath(), "%") {
		return nil, errInvalidURL
	}
	if err := validateTarget(resolved, allowed); err != nil {
		return nil, err
	}
	return resolved, nil
}

func hasTraversal(path string) bool {
	for part := range strings.SplitSeq(path, "/") {
		decoded, err := url.PathUnescape(part)
		if err != nil || decoded == "." || decoded == ".." {
			return true
		}
	}
	return false
}

func validateTarget(u *url.URL, allowed map[string]struct{}) error {
	if u == nil || u.User != nil || u.Host == "" || (u.Scheme != httpsScheme && u.Scheme != httpScheme) {
		return errInvalidURL
	}
	host, port, err := parseAuthority(u.Host)
	if err != nil {
		return errInvalidURL
	}
	if _, ok := allowed[authority(host, port)]; !ok {
		return errInvalidURL
	}
	return nil
}

func parseAuthority(value string) (string, string, error) {
	if value == "" || strings.ContainsAny(value, "/\\%@?#") {
		return "", "", errInvalidURL
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		host, port = value, ""
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" || strings.HasSuffix(host, ".") {
		return "", "", errInvalidURL
	}
	if port != "" {
		n, parseErr := strconv.Atoi(port)
		if parseErr != nil || n < 1 || n > 65535 {
			return "", "", errInvalidURL
		}
		port = strconv.Itoa(n)
	}
	if net.ParseIP(host) == nil && host != "localhost" && !validHostname(host) {
		return "", "", errInvalidURL
	}
	return host, port, nil
}

func authority(host, port string) string {
	return host + ":" + port
}

func validHostname(host string) bool {
	if len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

type safeTransport struct {
	hosts map[string]struct{}
	base  http.RoundTripper
}

func (t *safeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := validateTarget(req.URL, t.hosts); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}
