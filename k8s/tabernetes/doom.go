package main

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// doom starts KubeDoom (https://github.com/storax/kubedoom) on the node and
// shows it with the VNC viewer of the terminal (web/src/vnc.js). The pods of
// the default namespace are the monsters.

//go:embed doom/kubedoom.yaml
var kubedoomYAML string

const doomPort = 5900

func doomCmd(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error {
	if len(args) > 0 {
		fmt.Fprintln(hc.Stderr, "usage: doom")
		return interp.ExitStatus(2)
	}
	var nodes strings.Builder
	if code := runKubectl(ctx, []string{"get", "nodes", "-l", "kubernetes.io/os=linux", "-o", "name"}, nil, &nodes, hc.Stderr); code != 0 {
		return exitCode(code)
	}
	if strings.TrimSpace(nodes.String()) == "" {
		fmt.Fprintln(hc.Stderr, "doom: KubeDoom runs on a node, and there is none yet: join")
		return interp.ExitStatus(1)
	}
	if code := runKubectl(ctx, []string{"apply", "--server-side", "-f", "-"}, strings.NewReader(kubedoomYAML), hc.Stdout, hc.Stderr); code != 0 {
		return exitCode(code)
	}
	fmt.Fprintln(hc.Stdout, "Waiting for KubeDoom. The first start pulls 126 MB onto the node.")
	if code := runKubectl(ctx, []string{"-n", "kubedoom", "rollout", "status", "deployment/kubedoom", "--timeout=15m"}, nil, hc.Stdout, hc.Stderr); code != 0 {
		return exitCode(code)
	}

	// The forward lives as long as the viewer.
	pfCtx, stop := context.WithCancel(ctx)
	defer stop()
	go runKubectl(pfCtx, []string{"-n", "kubedoom", "port-forward", "deployment/kubedoom", fmt.Sprintf("%d:5900", doomPort)}, nil, hc.Stdout, hc.Stderr)
	if !waitForward(ctx, doomPort, 30*time.Second) {
		fmt.Fprintf(hc.Stderr, "doom: kubectl port-forward did not listen on localhost:%d\n", doomPort)
		return interp.ExitStatus(1)
	}

	fmt.Fprintln(hc.Stdout, "Cheats: idkfa for all weapons, idspispopd to walk through walls. Ctrl fires.")
	err := exec(ctx, []string{"vnc", fmt.Sprint(doomPort), "idbehold"})
	fmt.Fprintln(hc.Stdout, "KubeDoom keeps running: doom shows it again.")
	fmt.Fprintln(hc.Stdout, "kubectl delete ns kubedoom; kubectl delete deploy monsters ends it.")
	return err
}

// waitForward waits until kubectl port-forward listens on port (forwards.go).
func waitForward(ctx context.Context, port uint16, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		forwards.Lock()
		for _, f := range forwards.list {
			if f.port == port {
				forwards.Unlock()
				return true
			}
		}
		forwards.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
