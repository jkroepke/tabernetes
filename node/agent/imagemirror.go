package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The node image brings pause, kube-proxy, flannel and CoreDNS as an OCI image
// layout, see the Dockerfile. Every join needs them, and their downloads from
// the registries sometimes stall on Docker Desktop. A small read-only registry
// on 127.0.0.1 serves them, and containerd asks it first for every image
// (hosts.toml in _default). For all other images it answers 404, and
// containerd goes on to the real registry.
//
// A mirror instead of an import before the kubelet starts: no ctr in the image,
// nothing to wait for, and containerd unpacks only what a pod needs.
const imageLayoutDir = "/usr/share/tabernetes/images"

const registryConfigDir = "/etc/tabernetes/registry"

var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type imageMirror struct {
	dir string
	// full reference with tag, as crane --annotate-ref writes it:
	// registry.k8s.io/pause:3.10.2
	tags map[string]ociDescriptor
}

func loadImageMirror(dir string) (*imageMirror, error) {
	b, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return nil, err
	}
	var index struct {
		Manifests []ociDescriptor `json:"manifests"`
	}
	if err := json.Unmarshal(b, &index); err != nil {
		return nil, fmt.Errorf("%s/index.json: %w", dir, err)
	}
	m := &imageMirror{dir: dir, tags: map[string]ociDescriptor{}}
	for _, d := range index.Manifests {
		if ref := d.Annotations["org.opencontainers.image.ref.name"]; ref != "" && digestRe.MatchString(d.Digest) {
			m.tags[ref] = d
		}
	}
	return m, nil
}

func (m *imageMirror) blobPath(digest string) string {
	return filepath.Join(m.dir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
}

// ServeHTTP answers the pull part of the registry API:
//
//	/v2/
//	/v2/<name>/manifests/<tag or digest>?ns=<registry>
//	/v2/<name>/blobs/<digest>
//
// containerd adds ns, the registry of the image, when it asks a mirror.
func (m *imageMirror) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "read-only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	p, ok := strings.CutPrefix(r.URL.Path, "/v2/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if p == "" {
		return
	}

	var (
		name, ref string
		manifest  bool
	)
	if i := strings.LastIndex(p, "/manifests/"); i > 0 {
		name, ref, manifest = p[:i], p[i+len("/manifests/"):], true
	} else if i := strings.LastIndex(p, "/blobs/"); i > 0 {
		name, ref = p[:i], p[i+len("/blobs/"):]
	} else {
		http.NotFound(w, r)
		return
	}

	desc := ociDescriptor{Digest: ref, MediaType: "application/octet-stream"}
	if manifest {
		if !digestRe.MatchString(ref) {
			d, ok := m.tags[r.URL.Query().Get("ns")+"/"+name+":"+ref]
			if !ok {
				http.NotFound(w, r)
				return
			}
			desc = d
		} else {
			// A manifest by digest: the media type is in the manifest itself.
			b, err := os.ReadFile(m.blobPath(ref))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			var mt struct {
				MediaType string `json:"mediaType"`
			}
			if json.Unmarshal(b, &mt) != nil || mt.MediaType == "" {
				http.NotFound(w, r)
				return
			}
			desc.MediaType = mt.MediaType
		}
	}
	if !digestRe.MatchString(desc.Digest) {
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(m.blobPath(desc.Digest))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", desc.MediaType)
	w.Header().Set("Docker-Content-Digest", desc.Digest)
	// ServeContent sets Content-Length, answers HEAD and range requests.
	http.ServeContent(w, r, "", time.Time{}, f)
}

// startImageMirror serves the image layout of the node image on 127.0.0.1 and
// writes the hosts.toml that points containerd to it. It returns false when the
// image has no layout.
func startImageMirror() bool {
	m, err := loadImageMirror(imageLayoutDir)
	if err != nil {
		log.Printf("no preloaded images, every image comes from its registry: %v", err)
		return false
	}
	// containerd runs in this network namespace, so loopback reaches it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Printf("image mirror: %v", err)
		return false
	}
	go func() {
		err := (&http.Server{Handler: m, ReadHeaderTimeout: 10 * time.Second}).Serve(l)
		log.Printf("image mirror: %v", err)
	}()
	hosts := fmt.Sprintf("# The images of the node image, served by the agent (imagemirror.go).\n"+
		"# Everything else falls through to the registry itself.\n"+
		"[host.\"http://%s\"]\n"+
		"  capabilities = [\"pull\", \"resolve\"]\n", l.Addr())
	if err := writeFile(filepath.Join(registryConfigDir, "_default", "hosts.toml"), hosts, 0o644); err != nil {
		log.Printf("image mirror: %v", err)
		l.Close()
		return false
	}
	return true
}
