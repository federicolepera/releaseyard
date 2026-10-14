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

package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const remoteProbeTimeout = 5 * time.Second
const maxProbeConfigSize = 1 << 20

type probeConfig struct {
	server     string
	token      string
	tlsConfig  *tls.Config
	caData     []byte
	serverName string
	certData   []byte
	keyData    []byte
}

func probeKubeconfig(ctx context.Context, data []byte) error {
	config, err := parseProbeConfig(data)
	if err != nil {
		return err
	}
	return performProbe(ctx, config)
}

func parseProbeConfig(data []byte) (*probeConfig, error) {
	if len(data) > maxProbeConfigSize {
		return nil, errInvalidKubeconfig
	}
	cfg, err := clientcmd.Load(data)
	if err != nil || cfg.CurrentContext == "" {
		return nil, errInvalidKubeconfig
	}
	if err := validateClusterEntries(cfg.Clusters); err != nil {
		return nil, err
	}
	current, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok || current == nil || current.Cluster == "" || current.AuthInfo == "" {
		return nil, errInvalidKubeconfig
	}
	cluster, ok := cfg.Clusters[current.Cluster]
	if !ok || cluster == nil || cluster.Server == "" {
		return nil, errInvalidKubeconfig
	}
	server, err := validateServerURL(cluster.Server)
	if err != nil {
		return nil, err
	}
	user, ok := cfg.AuthInfos[current.AuthInfo]
	if !ok || user == nil {
		return nil, errInvalidKubeconfig
	}
	if err := validateAuthEntries(cfg.AuthInfos, current.AuthInfo); err != nil {
		return nil, err
	}
	token, certMode, err := validateSelectedAuth(user)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := buildTLSConfig(cluster, user, certMode)
	if err != nil {
		return nil, err
	}
	return &probeConfig{server: server, token: token, tlsConfig: tlsConfig, caData: bytes.Clone(cluster.CertificateAuthorityData), serverName: cluster.TLSServerName, certData: bytes.Clone(user.ClientCertificateData), keyData: bytes.Clone(user.ClientKeyData)}, nil
}

func validateClusterEntries(clusters map[string]*clientcmdapi.Cluster) error {
	for _, entry := range clusters {
		if entry == nil || entry.InsecureSkipTLSVerify || entry.ProxyURL != "" || entry.CertificateAuthority != "" {
			return errInvalidKubeconfig
		}
	}
	return nil
}

func validateServerURL(raw string) (string, error) {
	server, err := url.Parse(raw)
	if err != nil || server.Scheme != "https" || server.Host == "" || server.User != nil || server.RawQuery != "" || server.Fragment != "" {
		return "", errInvalidKubeconfig
	}
	return raw, nil
}

func validateAuthEntries(entries map[string]*clientcmdapi.AuthInfo, selected string) error {
	for name, entry := range entries {
		if entry == nil || !safeAuthInfo(entry) || (name != selected && !emptyAuthInfo(entry)) {
			return errInvalidKubeconfig
		}
	}
	return nil
}

func validateSelectedAuth(user *clientcmdapi.AuthInfo) (string, bool, error) {
	tokenMode := user.Token != ""
	certMode := len(user.ClientCertificateData) > 0 && len(user.ClientKeyData) > 0
	if tokenMode == certMode || (len(user.ClientCertificateData) > 0) != (len(user.ClientKeyData) > 0) {
		return "", false, errInvalidKubeconfig
	}
	return user.Token, certMode, nil
}

func buildTLSConfig(cluster *clientcmdapi.Cluster, user *clientcmdapi.AuthInfo, certMode bool) (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cluster.TLSServerName} //nolint:gosec
	if len(cluster.CertificateAuthorityData) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(cluster.CertificateAuthorityData) {
			return nil, errInvalidKubeconfig
		}
		tlsConfig.RootCAs = pool
	}
	if certMode {
		cert, err := tls.X509KeyPair(user.ClientCertificateData, user.ClientKeyData)
		if err != nil {
			return nil, errInvalidKubeconfig
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	return tlsConfig, nil
}

func performProbe(ctx context.Context, config *probeConfig) error {
	transport := &http.Transport{TLSClientConfig: config.tlsConfig, Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   remoteProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	probeCtx, cancel := context.WithTimeout(ctx, remoteProbeTimeout)
	defer cancel()
	endpoint := strings.TrimRight(config.server, "/") + "/apis/authentication.k8s.io/v1/selfsubjectreviews"
	body := []byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview","spec":{}}`)
	req, err := http.NewRequestWithContext(probeCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return errProbeFailed
	}
	req.Header.Set("Content-Type", "application/json")
	if config.token != "" {
		req.Header.Set("Authorization", "Bearer "+config.token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return errProbeFailed
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return errProbeFailed
	}
	return validateProbeIdentity(resp.Body)
}

func validateProbeIdentity(body io.Reader) error {
	var result struct {
		Status struct {
			UserInfo struct {
				Username string   `json:"username"`
				Groups   []string `json:"groups"`
			} `json:"userInfo"`
		} `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&result); err != nil || result.Status.UserInfo.Username == "" || result.Status.UserInfo.Username == "system:anonymous" || contains(result.Status.UserInfo.Groups, "system:unauthenticated") {
		return errProbeFailed
	}
	return nil
}

func contains(values []string, target string) bool {
	return slices.Contains(values, target)
}

var (
	errInvalidKubeconfig = errors.New("invalid kubeconfig")
	errProbeFailed       = errors.New("remote probe failed")
)

func safeAuthInfo(info *clientcmdapi.AuthInfo) bool {
	return info != nil && info.Username == "" && info.Password == "" && info.TokenFile == "" && info.ClientCertificate == "" && info.ClientKey == "" && info.Impersonate == "" && len(info.ImpersonateGroups) == 0 && len(info.ImpersonateUserExtra) == 0 && info.ImpersonateUID == "" && info.AuthProvider == nil && info.Exec == nil
}

func emptyAuthInfo(info *clientcmdapi.AuthInfo) bool {
	return safeAuthInfo(info) && info.Token == "" && len(info.ClientCertificateData) == 0 && len(info.ClientKeyData) == 0
}

func probeFailure(err error) (string, string) {
	if errors.Is(err, errInvalidKubeconfig) {
		return "InvalidCredentials", "Kubeconfig is invalid or uses unsupported authentication settings"
	}
	return "ProbeFailed", "Remote cluster authentication failed"
}
