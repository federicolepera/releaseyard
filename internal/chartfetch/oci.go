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

package chartfetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
)

const (
	ociScheme              = "oci"
	ociManifestMediaType   = "application/vnd.oci.image.manifest.v1+json"
	ociHelmChartMediaType  = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	ociHelmLegacyMediaType = "application/tar+gzip"
	maxManifestBytes       = int64(2 << 20)
	maxTokenBytes          = int64(1 << 20)
	ociRegistryScheme      = httpsScheme
)

type ociReference struct {
	registry   string
	repository string
	version    string
}

type ociLayer struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type ociManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	Layers        []ociLayer        `json:"layers"`
	Annotations   map[string]string `json:"annotations"`
}

func (p *Puller) pullOCI(ctx context.Context, chart platformv1alpha1.ApplicationDefinitionChart) ([]byte, string, error) {
	ref, err := parseOCIReference(chart.Repository, chart.Name, p.allowedHosts, chart.Version)
	if err != nil {
		return nil, "", err
	}
	manifestURL := ociManifestURL(ref)
	manifestBytes, status, headers, err := p.ociRequest(ctx, manifestURL, "", ociManifestMediaType, maxManifestBytes)
	if err != nil {
		return nil, "", fmt.Errorf("OCI manifest could not be retrieved")
	}
	token := ""
	if status == http.StatusUnauthorized {
		token, err = p.ociToken(ctx, headers.Get("WWW-Authenticate"), ref)
		if err != nil {
			return nil, "", err
		}
		manifestBytes, status, _, err = p.ociRequest(ctx, manifestURL, token, ociManifestMediaType, maxManifestBytes)
		if err != nil {
			return nil, "", fmt.Errorf("OCI manifest could not be retrieved")
		}
	}
	if status != http.StatusOK {
		return nil, "", fmt.Errorf("OCI manifest request returned status %d", status)
	}
	var manifest ociManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, "", fmt.Errorf("OCI manifest is invalid")
	}
	if title := manifest.Annotations["org.opencontainers.image.title"]; title != "" && title != chart.Name {
		return nil, "", fmt.Errorf("OCI manifest chart name does not match the request")
	}
	if version := manifest.Annotations["org.opencontainers.image.version"]; version != "" && version != chart.Version {
		return nil, "", fmt.Errorf("OCI manifest chart version does not match the request")
	}
	layer, err := ociChartLayer(manifest)
	if err != nil {
		return nil, "", err
	}
	expected, err := parseSHA256Digest(layer.Digest)
	if err != nil {
		return nil, "", fmt.Errorf("OCI chart digest is invalid")
	}
	if layer.Size <= 0 || layer.Size > maxArchiveBytes {
		return nil, "", fmt.Errorf("OCI chart layer size is invalid")
	}
	blobURL := ociBlobURL(ref, layer.Digest)
	archive, status, headers, err := p.ociRequest(ctx, blobURL, token, "application/octet-stream", maxArchiveBytes)
	if err != nil {
		return nil, "", fmt.Errorf("OCI chart blob could not be retrieved")
	}
	if isRedirect(status) {
		archive, status, err = p.ociRedirectedBlob(ctx, headers.Get("Location"))
		if err != nil {
			return nil, "", err
		}
	}
	if status != http.StatusOK {
		return nil, "", fmt.Errorf("OCI chart blob request returned status %d", status)
	}
	actual := sha256.Sum256(archive)
	if !equalBytes(actual[:], expected) || int64(len(archive)) != layer.Size {
		return nil, "", fmt.Errorf("OCI chart digest or size does not match the manifest")
	}
	return archive, "sha256:" + hex.EncodeToString(actual[:]), nil
}

func ociChartLayer(manifest ociManifest) (ociLayer, error) {
	var selected ociLayer
	count := 0
	for _, layer := range manifest.Layers {
		if layer.MediaType != ociHelmChartMediaType && layer.MediaType != ociHelmLegacyMediaType {
			continue
		}
		selected = layer
		count++
	}
	if count != 1 {
		return selected, fmt.Errorf("OCI manifest must contain exactly one Helm chart layer")
	}
	return selected, nil
}

func parseOCIReference(raw, chartName string, allowed map[string]struct{}, version string) (ociReference, error) {
	var ref ociReference
	if chartName == "" || version == "" || strings.ContainsAny(chartName, "/\\") {
		return ref, errInvalidURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != ociScheme || u.Opaque != "" || u.User != nil || u.Host == "" || u.Host != strings.ToLower(u.Host) || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || strings.Contains(u.Host, "\\") || strings.Contains(u.EscapedPath(), "%") {
		return ref, errInvalidURL
	}
	host, port, err := parseAuthority(u.Host)
	if err != nil {
		return ref, errInvalidURL
	}
	if _, ok := allowed[authority(host, port)]; !ok {
		return ref, errInvalidURL
	}
	path := strings.Trim(u.Path, "/")
	if path == "" || hasTraversal(path) || strings.Contains(path, "//") || strings.Contains(path, "\\") || strings.Contains(path, ":") || !validRepositoryPath(path) {
		return ref, errInvalidURL
	}
	repository := path
	if repository != chartName && !strings.HasSuffix(repository, "/"+chartName) {
		repository += "/" + chartName
	}
	registryHost := host
	if port != "" {
		registryHost = net.JoinHostPort(host, port)
	}
	return ociReference{registry: registryHost, repository: repository, version: version}, nil
}

func validRepositoryPath(path string) bool {
	for part := range strings.SplitSeq(path, "/") {
		if part == "" || part == "." || part == ".." || strings.ToLower(part) != part {
			return false
		}
		for _, r := range part {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
				return false
			}
		}
	}
	return true
}

func ociManifestURL(ref ociReference) *url.URL {
	return &url.URL{
		Scheme: ociRegistryScheme,
		Host:   ref.registry,
		Path:   "/v2/" + ref.repository + "/manifests/" + url.PathEscape(ref.version),
	}
}

func ociBlobURL(ref ociReference, digest string) *url.URL {
	return &url.URL{
		Scheme: ociRegistryScheme,
		Host:   ref.registry,
		Path:   "/v2/" + ref.repository + "/blobs/" + digest,
	}
}

func (p *Puller) ociRequest(ctx context.Context, target *url.URL, token, accept string, maxBytes int64) ([]byte, int, http.Header, error) {
	if err := validateTarget(target, p.allowedHosts); err != nil {
		return nil, 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, 0, nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := p.client.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, 0, nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, 0, nil, errTooLarge
	}
	return data, response.StatusCode, response.Header.Clone(), nil
}

func (p *Puller) ociRedirectedBlob(ctx context.Context, location string) ([]byte, int, error) {
	if location == "" {
		return nil, 0, fmt.Errorf("OCI chart blob redirect is missing a location")
	}
	u, err := url.Parse(location)
	if err != nil || u.Scheme != httpsScheme || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawFragment != "" {
		return nil, 0, fmt.Errorf("OCI chart blob redirect is invalid")
	}
	if !trustedOCIBlobRedirectHost(u.Host) {
		return nil, 0, fmt.Errorf("OCI chart blob redirect host is not allowed")
	}
	client := p.blobClient
	if client == nil {
		return nil, 0, fmt.Errorf("OCI chart blob redirect client is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("OCI chart blob redirect request is invalid")
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("OCI chart blob redirect request failed")
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxArchiveBytes+1))
	if err != nil {
		return nil, 0, fmt.Errorf("OCI chart blob redirect response could not be read")
	}
	if int64(len(data)) > maxArchiveBytes {
		return nil, 0, errTooLarge
	}
	return data, response.StatusCode, nil
}

func isRedirect(status int) bool {
	return status == http.StatusMovedPermanently || status == http.StatusFound || status == http.StatusSeeOther || status == http.StatusTemporaryRedirect || status == http.StatusPermanentRedirect
}

func trustedOCIBlobRedirectHost(host string) bool {
	parsedHost, port, err := parseAuthority(host)
	if err != nil || port != "" {
		return false
	}
	return parsedHost == "pkg-containers.githubusercontent.com" || strings.HasSuffix(parsedHost, ".githubusercontent.com")
}

func (p *Puller) ociToken(ctx context.Context, challenge string, ref ociReference) (string, error) {
	realm, service, scope, err := parseBearerChallenge(challenge)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(realm)
	if err != nil || u.Scheme != httpsScheme || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawFragment != "" {
		return "", fmt.Errorf("OCI authentication realm is invalid")
	}
	if err := validateTarget(u, p.allowedHosts); err != nil {
		return "", fmt.Errorf("OCI authentication realm is not allowed")
	}
	if scope == "" {
		scope = "repository:" + ref.repository + ":pull"
	}
	query := u.Query()
	if service != "" {
		query.Set("service", service)
	}
	query.Set("scope", scope)
	u.RawQuery = query.Encode()
	body, status, _, err := p.ociRequest(ctx, u, "", "application/json", maxTokenBytes)
	if err != nil {
		return "", fmt.Errorf("OCI authentication request failed")
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("OCI authentication request returned status %d", status)
	}
	var token struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return "", fmt.Errorf("OCI authentication response is invalid")
	}
	if token.Token != "" {
		return token.Token, nil
	}
	if token.AccessToken != "" {
		return token.AccessToken, nil
	}
	return "", fmt.Errorf("OCI authentication response did not contain a token")
}

func parseBearerChallenge(header string) (string, string, string, error) {
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", "", "", fmt.Errorf("OCI registry did not provide a Bearer challenge")
	}
	params := map[string]string{}
	for _, part := range splitChallengeParameters(strings.TrimSpace(header[len(prefix):])) {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return "", "", "", fmt.Errorf("OCI Bearer challenge is invalid")
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.Trim(strings.TrimSpace(value), "\"")
		if key == "" || value == "" {
			return "", "", "", fmt.Errorf("OCI Bearer challenge is invalid")
		}
		params[key] = value
	}
	realm := params["realm"]
	if realm == "" {
		return "", "", "", fmt.Errorf("OCI Bearer challenge is missing a realm")
	}
	return realm, params["service"], params["scope"], nil
}

func splitChallengeParameters(value string) []string {
	var result []string
	start := 0
	quoted := false
	for i, r := range value {
		switch r {
		case '"':
			quoted = !quoted
		case ',':
			if !quoted {
				result = append(result, value[start:i])
				start = i + 1
			}
		}
	}
	result = append(result, value[start:])
	return result
}
