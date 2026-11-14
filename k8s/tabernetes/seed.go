package main

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"os"
	"syscall/js"

	"k8s.io/component-base/version"

	"k8s.io/kubernetes/cmd/tabernetes/etcdshim"
)

// The build boots the control plane once in Node.js (k8s/snapshot.mjs) and
// keeps its etcd data in the binary: the cluster right after the addons,
// before the controllers ran. A first visit starts from it instead of an
// empty etcd, like a return visit starts from the cluster of the last visit:
// kube-apiserver finds its bootstrap objects (RBAC roles, priority classes,
// namespaces, the kubernetes Service) and the addons are there. The PKI, the
// tokens and the kubeconfigs are not in the seed: every cluster makes its
// own. kube-apiserver writes its CAs into the ConfigMap extension-apiserver-
// authentication at start and keeps the ones that are there, so the seed
// leaves it out. Nodes, pods, service accounts and their secrets
// do not exist yet when the seed is taken.

// loadSeed returns the seed of the build (globalThis.tabernetesSeed, set by
// web/src/wasm.js from the custom section "tabernetes.seed"), or nil.
func loadSeed() *savedState {
	v := js.Global().Get("tabernetesSeed")
	if v.Type() != js.TypeObject || v.Length() == 0 {
		return nil
	}
	js.Global().Delete("tabernetesSeed")
	b := make([]byte, v.Length())
	js.CopyBytesToGo(b, v)
	var st savedState
	if err := gob.NewDecoder(bytes.NewReader(b)).Decode(&st); err != nil {
		fmt.Fprintln(os.Stderr, "tabernetes: the seed of the build:", err)
		return nil
	}
	if st.Version != version.Get().GitVersion || st.Addons != addonsHash() {
		fmt.Fprintln(os.Stderr, "tabernetes: the seed is from another build")
		return nil
	}
	if err := etcdshim.Check(st.Etcd); err != nil {
		fmt.Fprintln(os.Stderr, "tabernetes: the seed of the build:", err)
		return nil
	}
	return &st
}

// seedSaver hands the first snapshot of etcd to the build (fn), without
// the files of the PKI.
func seedSaver(fn js.Value) func([]byte) {
	ver, addons := version.Get().GitVersion, addonsHash()
	done := false
	return func(etcd []byte) {
		if done {
			return
		}
		done = true
		// The CAs of the build: kube-apiserver would add the new ones
		// and keep these.
		etcd, err := etcdshim.Without(etcd, "/registry/configmaps/kube-system/extension-apiserver-authentication")
		must(err)
		var buf bytes.Buffer
		must(gob.NewEncoder(&buf).Encode(savedState{Version: ver, Etcd: etcd, Addons: addons}))
		a := js.Global().Get("Uint8Array").New(buf.Len())
		js.CopyBytesToJS(a, buf.Bytes())
		fn.Invoke(a)
	}
}
