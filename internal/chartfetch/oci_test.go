package chartfetch

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
)

func TestPullOCIVerifiesChartLayer(t *testing.T) {
	archive := []byte("oci chart archive bytes")
	digest := sha256.Sum256(archive)
	registry := newOCIRegistry(t, archive, "sha256:"+hex.EncodeToString(digest[:]))
	puller := ociPullerForServer(t, registry.server)
	chart := platformv1alpha1.ApplicationDefinitionChart{
		Repository: "oci://" + registry.server.Listener.Addr().String() + "/federicolepera/helm-charts",
		Name:       "releaseyard-demo",
		Version:    "0.2.0",
	}
	got, gotDigest, err := puller.PullVerified(context.Background(), chart)
	if err != nil {
		t.Fatalf("PullVerified() error = %v", err)
	}
	if string(got) != string(archive) || gotDigest != "sha256:"+hex.EncodeToString(digest[:]) {
		t.Fatalf("PullVerified() = (%q, %q)", got, gotDigest)
	}
	if !registry.authenticatedManifest || !registry.authenticatedBlob {
		t.Fatalf("OCI requests did not use the bearer token: %#v", registry)
	}
}

func TestPullOCIRejectsTamperedChartLayer(t *testing.T) {
	archive := []byte("oci chart archive bytes")
	digest := sha256.Sum256([]byte("different archive"))
	registry := newOCIRegistry(t, archive, "sha256:"+hex.EncodeToString(digest[:]))
	puller := ociPullerForServer(t, registry.server)
	chart := platformv1alpha1.ApplicationDefinitionChart{
		Repository: "oci://" + registry.server.Listener.Addr().String() + "/federicolepera/helm-charts",
		Name:       "releaseyard-demo",
		Version:    "0.2.0",
	}
	if _, _, err := puller.PullVerified(context.Background(), chart); err == nil {
		t.Fatal("PullVerified() error = nil, want digest verification failure")
	}
}

func TestPullOCIRejectsOffHostAuthRealm(t *testing.T) {
	archive := []byte("oci chart archive bytes")
	digest := sha256.Sum256(archive)
	registry := newOCIRegistry(t, archive, "sha256:"+hex.EncodeToString(digest[:]))
	registry.realmOverride = "https://evil.example.test/token"
	puller := ociPullerForServer(t, registry.server)
	chart := platformv1alpha1.ApplicationDefinitionChart{
		Repository: "oci://" + registry.server.Listener.Addr().String() + "/federicolepera/helm-charts",
		Name:       "releaseyard-demo",
		Version:    "0.2.0",
	}
	if _, _, err := puller.PullVerified(context.Background(), chart); err == nil {
		t.Fatal("PullVerified() error = nil, want off-host realm rejection")
	}
}

func TestParseOCIReferenceAcceptsBaseOrChartPath(t *testing.T) {
	allowed := map[string]struct{}{"ghcr.io:": {}}
	for _, raw := range []string{
		"oci://ghcr.io/federicolepera/helm-charts",
		"oci://ghcr.io/federicolepera/helm-charts/releaseyard-demo",
	} {
		ref, err := parseOCIReference(raw, "releaseyard-demo", allowed, "0.2.0")
		if err != nil || ref.repository != "federicolepera/helm-charts/releaseyard-demo" {
			t.Fatalf("parseOCIReference(%q) = %#v, %v", raw, ref, err)
		}
	}
	for _, raw := range []string{
		"oci://GHCR.io/federicolepera/helm-charts",
		"oci://ghcr.io/federicolepera/../helm-charts",
		"oci://ghcr.io/federicolepera/helm-charts?tag=0.2.0",
	} {
		if _, err := parseOCIReference(raw, "releaseyard-demo", allowed, "0.2.0"); err == nil {
			t.Fatalf("parseOCIReference(%q) unexpectedly succeeded", raw)
		}
	}
}

type ociRegistryFixture struct {
	server                *httptest.Server
	realm                 string
	realmOverride         string
	authenticatedManifest bool
	authenticatedBlob     bool
}

func newOCIRegistry(t *testing.T, archive []byte, layerDigest string) *ociRegistryFixture {
	t.Helper()
	fixture := &ociRegistryFixture{}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.URL.Query().Get("service") != "ghcr.io" || r.URL.Query().Get("scope") != "repository:federicolepera/helm-charts/releaseyard-demo:pull" {
				t.Errorf("unexpected token query: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-token"})
		case "/v2/federicolepera/helm-charts/releaseyard-demo/manifests/0.2.0":
			if r.Header.Get("Authorization") != "Bearer test-token" {
				fixture.realm = "https://" + r.Host + "/token"
				realm := fixture.realm
				if fixture.realmOverride != "" {
					realm = fixture.realmOverride
				}
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="ghcr.io",scope="repository:federicolepera/helm-charts/releaseyard-demo:pull"`, realm))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fixture.authenticatedManifest = true
			manifest := map[string]any{
				"schemaVersion": 2,
				"layers": []map[string]any{{
					"mediaType": ociHelmChartMediaType,
					"digest":    layerDigest,
					"size":      len(archive),
				}},
				"annotations": map[string]string{
					"org.opencontainers.image.title":   "releaseyard-demo",
					"org.opencontainers.image.version": "0.2.0",
				},
			}
			_ = json.NewEncoder(w).Encode(manifest)
		case "/v2/federicolepera/helm-charts/releaseyard-demo/blobs/" + layerDigest:
			if r.Header.Get("Authorization") != "Bearer test-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fixture.authenticatedBlob = true
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func ociPullerForServer(t *testing.T, server *httptest.Server) *Puller {
	t.Helper()
	puller, err := NewPuller([]string{server.Listener.Addr().String()})
	if err != nil {
		t.Fatalf("NewPuller() error = %v", err)
	}
	puller.client.Transport = &safeTransport{
		hosts: puller.allowedHosts,
		base: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- local TLS test server.
		},
	}
	return puller
}
