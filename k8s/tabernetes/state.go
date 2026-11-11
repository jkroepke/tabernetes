package main

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall/js"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/component-base/version"

	"k8s.io/kubernetes/cmd/tabernetes/etcdshim"
)

// The cluster survives a reload: the page keeps it in the IndexedDB of the
// browser (web/src/state.js, globalThis.tabernetesState). It is what a
// control plane node keeps on its disk: the files of the PKI, the tokens and
// kubeconfigs in dataDir, and the data of etcd (a snapshot, etcdshim).
//
// The nodes do not survive: the tabs and the docker nodes of the last visit
// are gone. The control plane removes them with their pods before the
// controllers start, so that Deployments and DaemonSets start their pods on
// the nodes of this visit.

// savedState is what the page keeps.
type savedState struct {
	// Version of Kubernetes that wrote it. Another minor version starts
	// a new cluster.
	Version string
	Files   map[string][]byte // name in dataDir -> content
	Etcd    []byte
	// Addons names the addons it got (addonsHash). A build with other
	// addons applies them again.
	Addons string
}

// stateAPI is globalThis.tabernetesState: load(): Promise<Uint8Array|null>
// and save(data: Uint8Array): Promise. Undefined where nothing is kept.
func stateAPI() js.Value {
	if role == "tab" {
		return js.Undefined()
	}
	if v := js.Global().Get("tabernetesState"); v.Type() == js.TypeObject {
		return v
	}
	return js.Undefined()
}

func minor(v string) string {
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

// loadState returns the cluster of the last visit, or nil.
func loadState() *savedState {
	api := stateAPI()
	if api.IsUndefined() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v, err := awaitPromise(ctx, api.Call("load"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "tabernetes: loading the saved cluster:", err)
		return nil
	}
	if v.IsNull() || v.IsUndefined() || v.Length() == 0 {
		return nil
	}
	b := make([]byte, v.Length())
	js.CopyBytesToGo(b, v)
	var st savedState
	if err := gob.NewDecoder(bytes.NewReader(b)).Decode(&st); err != nil {
		fmt.Fprintln(os.Stderr, "tabernetes: the saved cluster:", err)
		status("the saved cluster is broken, starting a new one")
		return nil
	}
	if minor(st.Version) != minor(version.Get().GitVersion) {
		status(fmt.Sprintf("the saved cluster is from Kubernetes %s, starting a new one", st.Version))
		return nil
	}
	err = etcdshim.Check(st.Etcd)
	if st.Files["ca.crt"] == nil || st.Files["tokens.csv"] == nil || st.Files["admin.kubeconfig"] == nil {
		err = errors.New("files are missing")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tabernetes: the saved cluster:", err)
		status("the saved cluster is broken, starting a new one")
		return nil
	}
	return &st
}

// restoreFiles writes the files of a saved cluster back to dataDir and
// returns the cluster.
func (st *savedState) restoreFiles() (*cluster, error) {
	for name, b := range st.Files {
		if err := os.WriteFile(p(name), b, 0o600); err != nil {
			return nil, err
		}
	}
	ca, err := loadPKI(p("ca.crt"), p("ca.key"))
	if err != nil {
		return nil, err
	}
	// the first line of tokens.csv is the admin (boot)
	admin, _, _ := strings.Cut(string(st.Files["tokens.csv"]), ",")
	return &cluster{ca: ca, adminToken: admin}, nil
}

// stateFiles reads the files of dataDir that a restore needs.
func stateFiles() map[string][]byte {
	files := map[string][]byte{}
	entries, err := os.ReadDir(dataDir)
	must(err)
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		b, err := os.ReadFile(p(e.Name()))
		must(err)
		files[e.Name()] = b
	}
	return files
}

// stateSaver returns the function that etcdshim calls with a snapshot, or
// nil where nothing is kept.
func stateSaver(files map[string][]byte) func([]byte) {
	api := stateAPI()
	if api.IsUndefined() {
		return nil
	}
	ver, addons := version.Get().GitVersion, addonsHash()
	return func(etcd []byte) {
		var buf bytes.Buffer
		if err := gob.NewEncoder(&buf).Encode(savedState{Version: ver, Files: files, Etcd: etcd, Addons: addons}); err != nil {
			fmt.Fprintln(os.Stderr, "tabernetes: saving the cluster:", err)
			return
		}
		a := js.Global().Get("Uint8Array").New(buf.Len())
		js.CopyBytesToJS(a, buf.Bytes())
		api.Call("save", a)
	}
}

// dropOldNodes removes the nodes of the last visit, and their pods.
func dropOldNodes(ctx context.Context) error {
	cs, err := adminClient()
	if err != nil {
		return err
	}
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, n := range nodes.Items {
		dropNode(n.Name)
	}
	return nil
}
