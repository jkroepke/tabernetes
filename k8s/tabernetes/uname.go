package main

import (
	"context"
	"strings"
	"syscall/js"

	"k8s.io/component-base/version"
	"mvdan.cc/sh/v3/interp"
)

// tabernetesVersion is the release of the page, like "1.4.0", or "dev".
// The binary is built once for many releases (CI caches it), so the version
// comes from the web bundle (TABERNETES_VERSION in mise.toml), which sets
// globalThis.tabernetesVersion in the tab and in the shared worker.
func tabernetesVersion() string {
	if v := js.Global().Get("tabernetesVersion"); v.Type() == js.TypeString && v.String() != "" {
		return strings.TrimPrefix(v.String(), "v")
	}
	return "dev"
}

// uname prints the system like GNU uname. The kernel is Tabernetes, its
// release is the Tabernetes version, its version the one of Kubernetes, and
// the node is the tab. -p and -i are "unknown", so -a leaves them out, like
// GNU uname does.
func uname(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, rest, err := opts("uname", args, "asnrvmpio", map[string]string{
		"all": "a", "kernel-name": "s", "nodename": "n", "kernel-release": "r", "kernel-version": "v",
		"machine": "m", "processor": "p", "hardware-platform": "i", "operating-system": "o",
	})
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if len(rest) > 0 {
		return fail(hc, 1, "uname: extra operand '%s'", rest[0])
	}
	node := "tabernetes"
	if s := sessionFrom(ctx); s != nil {
		node = s.name
	}
	fields := []struct {
		flag, value string
	}{
		{"s", "Tabernetes"},
		{"n", node},
		{"r", tabernetesVersion()},
		{"v", "Kubernetes " + version.Get().GitVersion},
		{"m", "wasm32"},
		{"p", "unknown"},
		{"i", "unknown"},
		{"o", "Browser"},
	}
	if len(o) == 0 {
		o["s"] = ""
	}
	var out []string
	for _, f := range fields {
		if has(o, f.flag) || has(o, "a") && f.value != "unknown" {
			out = append(out, f.value)
		}
	}
	_, err = hc.Stdout.Write([]byte(strings.Join(out, " ") + "\n"))
	return err
}
