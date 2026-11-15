package main

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"os"
	"syscall/js"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

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
// do not exist yet when the seed is taken. Its objects get the time of the
// visit as their creation time (freshTimes), else `kubectl get ds -A`
// showed the age of the release in a new cluster.

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
	now := time.Now()
	etcd, err := etcdshim.Map(st.Etcd, func(_ string, v []byte) []byte { return freshTimes(v, now) })
	if err != nil {
		fmt.Fprintln(os.Stderr, "tabernetes: the seed of the build:", err)
		return nil
	}
	st.Etcd = etcd
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

// freshTimes returns the stored object v with now as its creation time and
// as the time of its managed fields. kube-apiserver stores objects as
// protobuf ("k8s\x00" and a runtime.Unknown), custom resources as JSON.
// Anything else, or a value it cannot parse, it returns as it is.
func freshTimes(v []byte, now time.Time) []byte {
	if bytes.HasPrefix(v, protoMagic) {
		// runtime.Unknown: raw (2) is the object, metadata (1) its ObjectMeta.
		out := withField(v[len(protoMagic):], 2, func(raw []byte) []byte {
			return withField(raw, 1, func(meta []byte) []byte { return freshMeta(meta, now) })
		})
		if out == nil {
			return v
		}
		return append(append([]byte{}, protoMagic...), out...)
	}
	if len(v) > 0 && v[0] == '{' {
		var obj map[string]any
		if json.Unmarshal(v, &obj) != nil {
			return v
		}
		meta, ok := obj["metadata"].(map[string]any)
		if !ok {
			return v
		}
		ts := now.UTC().Format(time.RFC3339)
		if _, ok := meta["creationTimestamp"]; ok {
			meta["creationTimestamp"] = ts
		}
		if mf, ok := meta["managedFields"].([]any); ok {
			for _, e := range mf {
				if e, ok := e.(map[string]any); ok && e["time"] != nil {
					e["time"] = ts
				}
			}
		}
		b, err := json.Marshal(obj)
		if err != nil {
			return v
		}
		return append(b, '\n')
	}
	return v
}

var protoMagic = []byte("k8s\x00")

// freshMeta sets creationTimestamp (8) and the time (4) of every entry of
// managedFields (17) of an ObjectMeta.
func freshMeta(meta []byte, now time.Time) []byte {
	// metav1.Time: seconds (1); nanos (2) are 0 in RFC 3339 seconds.
	var t []byte
	t = protowire.AppendTag(t, 1, protowire.VarintType)
	t = protowire.AppendVarint(t, uint64(now.Unix()))
	setTime := func([]byte) []byte { return t }
	meta = withField(meta, 8, setTime)
	if meta == nil {
		return nil
	}
	return withField(meta, 17, func(e []byte) []byte { return withField(e, 4, setTime) })
}

// withField returns the protobuf message m with fn applied to every
// length-delimited field num, or nil if m does not parse or fn returns nil.
func withField(m []byte, num protowire.Number, fn func([]byte) []byte) []byte {
	out := make([]byte, 0, len(m)+8)
	for len(m) > 0 {
		n, typ, l := protowire.ConsumeTag(m)
		if l < 0 {
			return nil
		}
		fl := protowire.ConsumeFieldValue(n, typ, m[l:])
		if fl < 0 {
			return nil
		}
		if n == num && typ == protowire.BytesType {
			b, _ := protowire.ConsumeBytes(m[l:])
			b = fn(b)
			if b == nil {
				return nil
			}
			out = protowire.AppendTag(out, n, typ)
			out = protowire.AppendBytes(out, b)
		} else {
			out = append(out, m[:l+fl]...)
		}
		m = m[l+fl:]
	}
	return out
}
