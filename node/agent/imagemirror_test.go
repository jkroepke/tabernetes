package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeBlob(t *testing.T, dir string, b []byte) string {
	t.Helper()
	sum := sha256.Sum256(b)
	if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", hex.EncodeToString(sum[:])), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestImageMirror(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	layer := writeBlob(t, dir, []byte("layer bytes"))
	const mt = "application/vnd.oci.image.manifest.v1+json"
	manifest := []byte(`{"schemaVersion":2,"mediaType":"` + mt + `","layers":[{"digest":"` + layer + `"}]}`)
	md := writeBlob(t, dir, manifest)
	index, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"manifests": []ociDescriptor{{
			MediaType:   mt,
			Digest:      md,
			Size:        int64(len(manifest)),
			Annotations: map[string]string{"org.opencontainers.image.ref.name": "registry.k8s.io/pause:3.10.2"},
		}},
	})
	if err := os.WriteFile(filepath.Join(dir, "index.json"), index, 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := loadImageMirror(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(m)
	defer srv.Close()

	do := func(method, path string, header ...string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	resp, _ := do(http.MethodGet, "/v2/")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Docker-Distribution-API-Version") != "registry/2.0" {
		t.Errorf("/v2/: %s", resp.Status)
	}

	// containerd resolves a tag with HEAD and needs digest, type and length.
	resp, _ = do(http.MethodHead, "/v2/pause/manifests/3.10.2?ns=registry.k8s.io")
	if resp.StatusCode != http.StatusOK ||
		resp.Header.Get("Docker-Content-Digest") != md ||
		resp.Header.Get("Content-Type") != mt ||
		resp.ContentLength != int64(len(manifest)) {
		t.Errorf("HEAD tag: %s %v", resp.Status, resp.Header)
	}

	resp, body := do(http.MethodGet, "/v2/pause/manifests/"+md+"?ns=registry.k8s.io")
	if resp.StatusCode != http.StatusOK || body != string(manifest) || resp.Header.Get("Content-Type") != mt {
		t.Errorf("GET manifest by digest: %s %q", resp.Status, body)
	}

	resp, body = do(http.MethodGet, "/v2/pause/blobs/"+layer+"?ns=registry.k8s.io")
	if resp.StatusCode != http.StatusOK || body != "layer bytes" {
		t.Errorf("GET blob: %s %q", resp.Status, body)
	}

	// containerd resumes a broken download with a range request.
	resp, body = do(http.MethodGet, "/v2/pause/blobs/"+layer, "Range", "bytes=6-")
	if resp.StatusCode != http.StatusPartialContent || body != "bytes" {
		t.Errorf("GET blob range: %s %q", resp.Status, body)
	}

	// Everything else is a 404, so containerd goes on to the real registry.
	for _, p := range []string{
		"/v2/pause/manifests/3.10.2",                 // no ns
		"/v2/pause/manifests/3.10.2?ns=ghcr.io",      // other registry
		"/v2/pause/manifests/3.9?ns=registry.k8s.io", // other tag
		"/v2/kube-proxy/manifests/v1.37.2?ns=registry.k8s.io",
		"/v2/pause/blobs/sha256:" + hex.EncodeToString(make([]byte, 32)),
		"/v2/pause/blobs/sha256:../../index.json",
		"/v2/pause/blobs/" + layer + "/../../index.json",
		"/v2/pause/manifests/" + layer, // a blob, not a manifest
		"/v3/",
	} {
		if resp, _ := do(http.MethodGet, p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: %s, want 404", p, resp.Status)
		}
	}

	if resp, _ := do(http.MethodPut, "/v2/pause/manifests/3.10.2?ns=registry.k8s.io"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT: %s", resp.Status)
	}
}
