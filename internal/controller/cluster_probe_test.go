package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const pemCertificateType = "CERTIFICATE"

func TestProbeKubeconfig(t *testing.T) {
	successHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/apis/authentication.k8s.io/v1/selfsubjectreviews" || r.Header.Get("Authorization") != "Bearer probe-token" {
			t.Errorf("unexpected probe request: method=%s path=%s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"status":{"userInfo":{"username":"alice","groups":["developers"]}}}`))
	}
	tests := []struct {
		name        string
		handler     http.HandlerFunc
		mutate      func([]byte) []byte
		wantErr     bool
		wantInvalid bool
	}{
		{name: "authenticated identity succeeds", handler: successHandler},
		{name: "anonymous identity rejected", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":{"userInfo":{"username":"system:anonymous"}}}`))
		}, wantErr: true},
		{name: "unauthenticated group rejected", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":{"userInfo":{"username":"alice","groups":["system:unauthenticated"]}}}`))
		}, wantErr: true},
		{name: "forbidden response rejected", handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "sensitive remote body", http.StatusForbidden)
		}, wantErr: true},
		{name: "redirect rejected", handler: func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/redirect-target", http.StatusTemporaryRedirect)
		}, wantErr: true},
		{name: "exec authentication rejected", handler: successHandler, mutate: mutateKubeconfig("  user:\n    token: probe-token", "  user:\n    exec:\n      command: malicious\n    token: probe-token"), wantErr: true, wantInvalid: true},
		{name: "auth provider rejected", handler: successHandler, mutate: mutateKubeconfig("  user:\n    token: probe-token", "  user:\n    auth-provider:\n      name: gcp\n    token: probe-token"), wantErr: true, wantInvalid: true},
		{name: "file credentials rejected", handler: successHandler, mutate: mutateKubeconfig("  user:\n    token: probe-token", "  user:\n    client-certificate: /tmp/client.crt\n    client-key: /tmp/client.key\n    token: probe-token"), wantErr: true, wantInvalid: true},
		{name: "impersonation rejected", handler: successHandler, mutate: mutateKubeconfig("  user:\n    token: probe-token", "  user:\n    as: privileged-user\n    token: probe-token"), wantErr: true, wantInvalid: true},
		{name: "insecure TLS rejected", handler: successHandler, mutate: mutateKubeconfig("  cluster:\n", "  cluster:\n    insecure-skip-tls-verify: true\n"), wantErr: true, wantInvalid: true},
		{name: "malformed CA rejected", handler: successHandler, mutate: func(config []byte) []byte {
			return []byte(strings.Replace(string(config), "certificate-authority-data: "+caDataFromConfig(config), "certificate-authority-data: "+base64.StdEncoding.EncodeToString([]byte("not a certificate")), 1))
		}, wantErr: true, wantInvalid: true},
		{name: "TLS server name override succeeds", handler: successHandler, mutate: mutateKubeconfig("  cluster:\n", "  cluster:\n    tls-server-name: 127.0.0.1\n")},
		{name: "TLS server name override is verified", handler: successHandler, mutate: mutateKubeconfig("  cluster:\n", "  cluster:\n    tls-server-name: wrong.example\n"), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(tt.handler)
			defer server.Close()
			config := testKubeconfig(server)
			if tt.mutate != nil {
				config = tt.mutate(config)
			}
			err := probeKubeconfig(context.Background(), config)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantInvalid && err != errInvalidKubeconfig {
				t.Fatalf("error = %v, want invalid kubeconfig", err)
			}
		})
	}

	t.Run("Kind-style client certificate authentication succeeds", func(t *testing.T) {
		clientCertPEM, clientKeyPEM, clientCert := makeTestClientCertificate(t)
		clientCAPool := x509.NewCertPool()
		clientCAPool.AddCert(clientCert)
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
				t.Error("probe did not present its client certificate")
			}
			_, _ = w.Write([]byte(`{"status":{"userInfo":{"username":"alice"}}}`))
		}))
		server.TLS = &tls.Config{
			ClientAuth: tls.RequireAndVerifyClientCert,
			ClientCAs:  clientCAPool,
			MinVersion: tls.VersionTLS12,
		} //nolint:gosec
		server.StartTLS()
		defer server.Close()
		config := testKubeconfig(server)
		certAuth := fmt.Sprintf("  user:\n    client-certificate-data: %s\n    client-key-data: %s", base64.StdEncoding.EncodeToString(clientCertPEM), base64.StdEncoding.EncodeToString(clientKeyPEM))
		config = []byte(strings.Replace(string(config), "  user:\n    token: probe-token", certAuth, 1))
		if err := probeKubeconfig(context.Background(), config); err != nil {
			t.Fatalf("mTLS probe failed: %v", err)
		}
	})

	t.Run("valid but untrusted CA is rejected", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(successHandler))
		defer server.Close()
		config := testKubeconfig(server)
		wrongCA := base64.StdEncoding.EncodeToString(makeTestCA(t))
		config = []byte(strings.Replace(string(config), caDataFromConfig(config), wrongCA, 1))
		if err := probeKubeconfig(context.Background(), config); err == nil || err == errInvalidKubeconfig {
			t.Fatalf("expected TLS trust failure, got %v", err)
		}
	})

	t.Run("probe timeout is bounded", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(6 * time.Second):
				_, _ = w.Write([]byte(`{"status":{"userInfo":{"username":"alice"}}}`))
			case <-r.Context().Done():
			}
		}))
		defer server.Close()
		started := time.Now()
		if err := probeKubeconfig(context.Background(), testKubeconfig(server)); err == nil || time.Since(started) > 7*time.Second {
			t.Fatalf("expected bounded timeout, got %v after %s", err, time.Since(started))
		}
	})
}

func makeTestClientCertificate(t *testing.T) ([]byte, []byte, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "kind-style-client"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemCertificateType, Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey}), cert
}

func testKubeconfig(server *httptest.Server) []byte {
	cert, _ := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	caPEM := pem.EncodeToMemory(&pem.Block{Type: pemCertificateType, Bytes: cert.Raw})
	ca := base64.StdEncoding.EncodeToString(caPEM)
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: test
clusters:
- name: test
  cluster:
    server: %s
    certificate-authority-data: %s
contexts:
- name: test
  context:
    cluster: test
    user: test
users:
- name: test
  user:
    token: probe-token
`, server.URL, ca))
}

func caDataFromConfig(config []byte) string {
	for line := range strings.SplitSeq(string(config), "\n") {
		if after, ok := strings.CutPrefix(line, "    certificate-authority-data: "); ok {
			return after
		}
	}
	return ""
}

func makeTestCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "untrusted test CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemCertificateType, Bytes: der})
}

func mutateKubeconfig(old, replacement string) func([]byte) []byte {
	return func(config []byte) []byte {
		return []byte(strings.Replace(string(config), old, replacement, 1))
	}
}
