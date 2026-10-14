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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	platformv1alpha1 "github.com/federicolepera/idp/api/v1alpha1"
)

var opaqueArchive = []byte("not a tarball: arbitrary opaque chart artifact bytes")

func TestPullReturnsArchiveDigestWithoutInspectingBytes(t *testing.T) {
	fixture := newChartMuseum(t, opaqueArchive, "")
	puller := pullerForServer(t, fixture)
	digest, err := puller.Pull(context.Background(), chartForServer(fixture))
	if err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	want := sha256.Sum256(opaqueArchive)
	if digest != "sha256:"+hex.EncodeToString(want[:]) {
		t.Fatalf("Pull() digest = %q, want archive digest", digest)
	}
	if fixture.authorization != "" || fixture.cookie != "" {
		t.Errorf("request included credentials: Authorization=%q Cookie=%q", fixture.authorization, fixture.cookie)
	}
}

func TestPullVerifiedReturnsExactVerifiedArchive(t *testing.T) {
	fixture := newChartMuseum(t, opaqueArchive, "")
	archive, digest, err := pullerForServer(t, fixture).PullVerified(context.Background(), chartForServer(fixture))
	if err != nil {
		t.Fatalf("PullVerified() error = %v", err)
	}
	if string(archive) != string(opaqueArchive) {
		t.Errorf("PullVerified() archive = %q, want exact served bytes %q", archive, opaqueArchive)
	}
	want := sha256.Sum256(opaqueArchive)
	if digest != "sha256:"+hex.EncodeToString(want[:]) {
		t.Errorf("PullVerified() digest = %q, want SHA-256 digest", digest)
	}
	if fixture.archiveRequests != 1 {
		t.Errorf("archive requests = %d, want 1", fixture.archiveRequests)
	}
}

func TestPullVerifiedReturnsNoArchiveForUnverifiedContent(t *testing.T) {
	tests := []struct {
		name    string
		archive []byte
		digest  string
	}{
		{name: "tampered index digest", archive: opaqueArchive, digest: digestFor([]byte("different archive"))},
		{name: "tampered archive", archive: []byte("changed archive"), digest: digestFor(opaqueArchive)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newChartMuseum(t, tc.archive, tc.digest)
			archive, digest, err := pullerForServer(t, fixture).PullVerified(context.Background(), chartForServer(fixture))
			if err == nil {
				t.Fatal("PullVerified() error = nil, want digest verification failure")
			}
			if archive != nil || digest != "" {
				t.Errorf("PullVerified() = (%q, %q), want no archive or digest on error", archive, digest)
			}
		})
	}
}

func TestPullResolvesRelativeArchiveURLWithRepositoryBasePath(t *testing.T) {
	for _, repository := range []string{"PATH", "PATH/"} {
		t.Run(repository, func(t *testing.T) {
			fixture := newChartMuseum(t, opaqueArchive, "")
			fixture.basePath = "/charts/repo"
			fixture.index = indexDocument("charts/releaseyard-demo-0.1.0.tgz", digestFor(opaqueArchive))
			chart := requestedChart(fixture.server.URL + fixture.basePath)
			if repository == "PATH/" {
				chart.Repository += "/"
			}
			if _, err := pullerForServer(t, fixture).Pull(context.Background(), chart); err != nil {
				t.Fatalf("Pull() error = %v", err)
			}
			if fixture.archiveRequests != 1 {
				t.Errorf("archive requests = %d, want 1", fixture.archiveRequests)
			}
		})
	}
}

func TestPullRejectsMissingChartOrVersionAndInvalidIndex(t *testing.T) {
	for _, tc := range []struct {
		name  string
		index string
	}{
		{"missing chart", "apiVersion: v1\nentries: {}\n"},
		{"missing version", "apiVersion: v1\nentries:\n  releaseyard-demo: []\n"},
		{"invalid yaml", "entries: [broken"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newChartMuseum(t, opaqueArchive, "")
			fixture.index = tc.index
			if _, err := pullerForServer(t, fixture).Pull(context.Background(), chartForServer(fixture)); err == nil {
				t.Fatal("Pull() error = nil, want failure")
			}
		})
	}
}

func TestPullRejectsBadDigestAndTamperedArchive(t *testing.T) {
	tests := []struct {
		name, digest string
		archive      []byte
	}{
		{"malformed digest", "not-a-digest", opaqueArchive},
		{"tampered archive", digestFor(opaqueArchive), []byte("changed archive")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newChartMuseum(t, tc.archive, tc.digest)
			if _, err := pullerForServer(t, fixture).Pull(context.Background(), chartForServer(fixture)); err == nil {
				t.Fatal("Pull() error = nil, want digest verification failure")
			}
			if tc.name == "malformed digest" && fixture.archiveRequests != 0 {
				t.Errorf("archive requests = %d, want 0 for malformed digest", fixture.archiveRequests)
			}
		})
	}
}

func TestPullRejectsMissingDigestBeforeArchiveRequest(t *testing.T) {
	fixture := newChartMuseum(t, opaqueArchive, "")
	fixture.index = indexDocument("charts/releaseyard-demo-0.1.0.tgz", "")
	if _, err := pullerForServer(t, fixture).Pull(context.Background(), chartForServer(fixture)); err == nil {
		t.Fatal("Pull() error = nil, want missing digest rejection")
	}
	if fixture.archiveRequests != 0 {
		t.Errorf("archive requests = %d, want 0 for missing digest", fixture.archiveRequests)
	}
}

func TestPullRejectsOversizedIndexAndArchive(t *testing.T) {
	t.Run("index", func(t *testing.T) {
		fixture := newChartMuseum(t, opaqueArchive, "")
		fixture.index = "#" + strings.Repeat("x", int(maxIndexBytes))
		if _, err := pullerForServer(t, fixture).Pull(context.Background(), chartForServer(fixture)); err == nil {
			t.Fatal("Pull() error = nil, want index size rejection")
		}
	})
	t.Run("archive", func(t *testing.T) {
		fixture := newChartMuseum(t, make([]byte, maxArchiveBytes+1), "")
		if _, err := pullerForServer(t, fixture).Pull(context.Background(), chartForServer(fixture)); err == nil {
			t.Fatal("Pull() error = nil, want archive size rejection")
		}
	})
}

func TestPullRejectsOffHostArchiveURLAndRedirect(t *testing.T) {
	var otherRequests int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherRequests++
		_, _ = w.Write(opaqueArchive)
	}))
	defer other.Close()
	t.Run("off-host URL", func(t *testing.T) {
		fixture := newChartMuseum(t, opaqueArchive, "")
		fixture.index = indexDocument(other.URL+"/chart.tgz", "")
		if _, err := pullerForServer(t, fixture).Pull(context.Background(), chartForServer(fixture)); err == nil {
			t.Fatal("Pull() error = nil, want off-host rejection")
		}
	})
	t.Run("redirect", func(t *testing.T) {
		fixture := newChartMuseum(t, opaqueArchive, "")
		fixture.redirectArchive = other.URL + "/stolen"
		if _, err := pullerForServer(t, fixture).Pull(context.Background(), chartForServer(fixture)); err == nil {
			t.Fatal("Pull() error = nil, want redirect rejection")
		}
	})
	if otherRequests != 0 {
		t.Errorf("off-host server received %d requests", otherRequests)
	}
}

func TestPullRejectsInvalidRepositoryURLs(t *testing.T) {
	puller, err := NewPuller([]string{"registry.example.test"})
	if err != nil {
		t.Fatalf("NewPuller() error = %v", err)
	}
	for _, raw := range []string{
		"https://user:secret@registry.example.test",
		"https://registry.example.test/path/../escape",
		"https://registry.example.test/path?query=1",
		"https://registry.example.test/path#fragment",
		"ftp://registry.example.test",
		"https://untrusted.example.test",
	} {
		t.Run(raw, func(t *testing.T) {
			chart := requestedChart(raw)
			if _, err := puller.Pull(context.Background(), chart); err == nil {
				t.Errorf("Pull(%q) error = nil, want rejection", raw)
			}
		})
	}
	if _, err := NewPuller([]string{"registry.example.test/path"}); err == nil {
		t.Fatal("NewPuller() error = nil for invalid allowlist host")
	}
}

func TestPullHonorsCanceledContext(t *testing.T) {
	fixture := newChartMuseum(t, opaqueArchive, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pullerForServer(t, fixture).Pull(ctx, chartForServer(fixture)); err == nil {
		t.Fatal("Pull() error = nil for canceled context")
	}
}

type chartMuseumFixture struct {
	server          *httptest.Server
	index           string
	archive         []byte
	archiveDigest   string
	redirectArchive string
	authorization   string
	cookie          string
	basePath        string
	archiveRequests int
}

func newChartMuseum(t *testing.T, archive []byte, indexDigest string) *chartMuseumFixture {
	t.Helper()
	if indexDigest == "" {
		indexDigest = digestFor(archive)
	}
	fixture := &chartMuseumFixture{archive: archive, archiveDigest: indexDigest}
	fixture.index = indexDocument("charts/releaseyard-demo-0.1.0.tgz", indexDigest)
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.authorization = r.Header.Get("Authorization")
		fixture.cookie = r.Header.Get("Cookie")
		switch r.URL.Path {
		case fixture.basePath + "/index.yaml":
			w.Header().Set("Content-Type", "application/yaml")
			_, _ = w.Write([]byte(fixture.index))
		case fixture.basePath + "/charts/releaseyard-demo-0.1.0.tgz":
			fixture.archiveRequests++
			if fixture.redirectArchive != "" {
				http.Redirect(w, r, fixture.redirectArchive, http.StatusFound)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(fixture.archive)))
			_, _ = w.Write(fixture.archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func indexDocument(archiveURL, digest string) string {
	return fmt.Sprintf("apiVersion: v1\nentries:\n  releaseyard-demo:\n  - name: releaseyard-demo\n    version: 0.1.0\n    urls:\n    - %q\n    digest: %q\n", archiveURL, digest)
}

func pullerForServer(t *testing.T, fixture *chartMuseumFixture) *Puller {
	t.Helper()
	puller, err := NewPuller([]string{fixture.server.Listener.Addr().String()})
	if err != nil {
		t.Fatalf("NewPuller() error = %v", err)
	}
	return puller
}

func requestedChart(repository string) platformv1alpha1.ApplicationDefinitionChart {
	return platformv1alpha1.ApplicationDefinitionChart{Repository: repository, Name: "releaseyard-demo", Version: "0.1.0"}
}

func chartForServer(fixture *chartMuseumFixture) platformv1alpha1.ApplicationDefinitionChart {
	return requestedChart(fixture.server.URL)
}

func digestFor(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
