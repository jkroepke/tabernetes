package main

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/component-base/version"
)

//go:embed addons/*.yaml
var addonFS embed.FS

func installAddons(ctx context.Context, c *cluster) error {
	entries, err := addonFS.ReadDir("addons")
	if err != nil {
		return err
	}
	dir := p("addons")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		b, err := addonFS.ReadFile("addons/" + e.Name())
		if err != nil {
			return err
		}
		b = bytes.ReplaceAll(b, []byte("KUBERNETES_VERSION"), []byte(version.Get().GitVersion))
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o644); err != nil {
			return err
		}
	}
	// The controllers may still be busy creating default service accounts etc.
	for i := 0; ; i++ {
		code := runKubectl(ctx, []string{"apply", "--server-side", "-f", dir}, nil, os.Stderr, os.Stderr)
		if code == 0 {
			return nil
		}
		if i == 10 {
			return fmt.Errorf("applying addons failed with exit code %d", code)
		}
		time.Sleep(2 * time.Second)
	}
}
