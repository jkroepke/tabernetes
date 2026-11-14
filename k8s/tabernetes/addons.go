package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"k8s.io/component-base/version"
)

//go:embed addons/*.yaml
var addonFS embed.FS

// addons returns the manifests of this build by file name.
func addons() (map[string][]byte, error) {
	entries, err := addonFS.ReadDir("addons")
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, e := range entries {
		b, err := addonFS.ReadFile("addons/" + e.Name())
		if err != nil {
			return nil, err
		}
		files[e.Name()] = bytes.ReplaceAll(b, []byte("KUBERNETES_VERSION"), []byte(version.Get().GitVersion))
	}
	return files, nil
}

// addonsHash names the addons of this build. A saved cluster remembers it
// (state.go): another build may bring other addons.
func addonsHash() string {
	files, err := addons()
	if err != nil {
		return ""
	}
	h := sha256.New()
	for _, name := range slices.Sorted(maps.Keys(files)) {
		fmt.Fprintf(h, "%s %d\n", name, len(files[name]))
		h.Write(files[name])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// installAddons applies the addons. update is for a restored cluster: the
// manifests of this build win over what the user changed in the old ones.
func installAddons(ctx context.Context, update bool) error {
	files, err := addons()
	if err != nil {
		return err
	}
	dir := p("addons")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			return err
		}
	}
	// --validate=false: kubectl validated the manifests against the
	// OpenAPI v3 specs of the apiserver, which it built for that alone
	// (~0.6 s). They are ours and known to be valid.
	args, tries := []string{"apply", "--server-side", "--validate=false", "-f", dir}, 10
	if update {
		args, tries = append(args, "--force-conflicts"), 3
	}
	// The controllers may still be busy creating default service accounts etc.
	for i := 0; ; i++ {
		code := runKubectl(ctx, args, nil, os.Stderr, os.Stderr)
		if code == 0 {
			return nil
		}
		if i == tries {
			return fmt.Errorf("applying addons failed with exit code %d", code)
		}
		time.Sleep(2 * time.Second)
	}
}
