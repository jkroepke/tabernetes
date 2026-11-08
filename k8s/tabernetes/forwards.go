package main

import (
	"fmt"
	"slices"
	"sync"
	"syscall/js"

	clientpf "k8s.io/client-go/tools/portforward"
	"k8s.io/kubectl/pkg/cmd/portforward"
)

// The ports of "kubectl port-forward" while they listen. The page shows each
// one as a link under the terminal that opens it like "proxy <port>".

type forward struct {
	port   uint16
	target string // pod:port, or namespace/pod:port outside default
}

var forwards struct {
	sync.Mutex
	list []*forward
}

func init() { portforward.OnForward = addForwards }

func addForwards(namespace, pod string, ports []clientpf.ForwardedPort) func() {
	if namespace != "" && namespace != "default" {
		pod = namespace + "/" + pod
	}
	var mine []*forward
	for _, p := range ports {
		mine = append(mine, &forward{p.Local, fmt.Sprintf("%s:%d", pod, p.Remote)})
	}
	forwards.Lock()
	forwards.list = append(forwards.list, mine...)
	forwards.Unlock()
	publishForwards()
	return func() {
		forwards.Lock()
		forwards.list = slices.DeleteFunc(forwards.list, func(f *forward) bool { return slices.Contains(mine, f) })
		forwards.Unlock()
		publishForwards()
	}
}

// publishForwards hands the list to globalThis.tabernetesForwards of the page.
func publishForwards() {
	cb := js.Global().Get("tabernetesForwards")
	if cb.Type() != js.TypeFunction {
		return
	}
	forwards.Lock()
	list := js.Global().Get("Array").New()
	for _, f := range forwards.list {
		o := js.Global().Get("Object").New()
		o.Set("port", int(f.port))
		o.Set("target", f.target)
		list.Call("push", o)
	}
	forwards.Unlock()
	cb.Invoke(list)
}
