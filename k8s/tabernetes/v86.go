package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"syscall/js"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// A container image on the tab node runs in v86 (https://github.com/copy/v86),
// an x86 emulator that compiles x86 code to WebAssembly. The page pulls the
// linux/386 variant of the image from a pull-through mirror of Docker Hub
// (Docker Hub sends no CORS headers), unpacks the layers into the image store
// of the browser and boots a small Linux kernel with them as its root file
// system over 9p (web/src/vm.js, vmworker.js, layers.js, rootfs.js). PID 1 of
// the VM (vm/init) starts the entrypoint as runc would. The console of the
// VM is the log of the container, and port-forward reaches the ports of the
// pod through the network card of the VM.

type v86VM struct {
	tp    *tabPod
	h     js.Value // the handle of vm.js start()
	funcs []js.Func
	id    string

	mu      sync.Mutex
	ready   chan error // the container started, or did not
	exited  chan int
	isReady bool
}

// hasVM tells whether the tab can run container images (web/src/vm.js).
func (n *tabNode) hasVM() bool {
	return n.host.Get("vm").Type() == js.TypeObject
}

// vmRuntime is the container runtime of the node, as kubectl get nodes -o
// wide shows it, or "" if the tab runs no container images.
func (n *tabNode) vmRuntime() string {
	if !n.hasVM() {
		return ""
	}
	return "v86://" + n.host.Get("vm").Get("version").String()
}

func newV86(tp *tabPod, _ *corev1.Container) (workload, error) {
	return &v86VM{tp: tp, ready: make(chan error, 1), exited: make(chan int, 1)}, nil
}

func (v *v86VM) imageID() string { return v.id }

func (v *v86VM) runtime() string { return "v86" }

// gateway is the first address of the pod CIDR of the node, as a bridge
// would have it.
func (n *tabNode) gateway() string {
	ip := n.podCIDR.IP.To4()
	return net.IPv4(ip[0], ip[1], ip[2], ip[3]+1).String()
}

func (v *v86VM) start(ctx context.Context, log io.Writer, pull *imagePull) error {
	vm := v.tp.n.host.Get("vm")
	if vm.Type() != js.TypeObject {
		return errors.New("this tab cannot run container images")
	}
	c := v.tp.container
	env := []any{}
	for _, e := range c.Env {
		if value, ok := v.tp.envValue(e); ok {
			env = append(env, e.Name+"="+value)
		}
	}
	command, args := []any{}, []any{}
	for _, s := range c.Command {
		command = append(command, s)
	}
	for _, s := range c.Args {
		args = append(args, s)
	}
	var memory, storage int64
	if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
		memory = q.Value()
	}
	// df shows it as the size of the root file system (web/src/vmworker.js)
	if q, ok := c.Resources.Limits[corev1.ResourceEphemeralStorage]; ok {
		storage = q.Value()
	}
	opts := map[string]any{
		"image": c.Image,
		// the API server defaults it: Always for :latest, else IfNotPresent
		"pullPolicy": string(c.ImagePullPolicy),
		"hostname":   v.tp.hostname,
		"ip":         v.tp.ip,
		"gateway":    v.tp.n.gateway(),
		"hosts":      v.tp.hostsFile(),
		"resolvConf": v.tp.resolvConf(),
		// what the network card hands to the cluster network (clusternet.go)
		"clusterCIDRs":     []any{clusterCIDR, serviceCIDR},
		"memory":           memory,
		"ephemeralStorage": storage,
		"container": map[string]any{
			"command":    command,
			"args":       args,
			"env":        env,
			"workingDir": c.WorkingDir,
			"stdin":      c.Stdin,
			"stdinOnce":  c.StdinOnce,
			"tty":        c.TTY,
		},
	}

	fn := func(f func(a []js.Value)) js.Func {
		jf := js.FuncOf(func(_ js.Value, a []js.Value) any { f(a); return nil })
		v.funcs = append(v.funcs, jf)
		return jf
	}
	// settle reports the start, once; false if it was reported already.
	settle := func(err error) bool {
		v.mu.Lock()
		defer v.mu.Unlock()
		if v.isReady {
			return false
		}
		v.isReady = true
		v.ready <- err
		return true
	}
	on := map[string]any{
		"pulling": fn(func([]js.Value) { pull.fetching() }),
		"pulled": fn(func(a []js.Value) {
			v.id = a[0].Get("imageID").String()
			// The kubelet logs the pull and creates the container right
			// after; here, the VM boots next.
			pull.done(int64(a[0].Get("size").Float()))
		}),
		"started": fn(func([]js.Value) { settle(nil) }),
		"console": fn(func(a []js.Value) { io.WriteString(log, a[0].String()) }),
		"exit": fn(func(a []js.Value) {
			code := a[0].Int()
			// init could not start the entrypoint: what runc would say
			msg := fmt.Sprintf("exited with code %d", code)
			if len(a) > 1 && a[1].Truthy() {
				msg = a[1].String()
			}
			settle(&startError{code: code, msg: "failed to create containerd task: failed to create shim task: OCI runtime create failed: " +
				"runc create failed: unable to start container process: error during container init: " + msg + ": unknown"})
			select {
			case v.exited <- code:
			default:
			}
		}),
		// the pod opens a connection into the cluster network
		"dial": fn(func(a []js.Value) { go v.outbound(a[0].Int(), a[1].String(), a[2].Int()) }),
		"dns": fn(func(a []js.Value) {
			q := make([]byte, a[0].Length())
			js.CopyBytesToGo(q, a[0])
			reply := a[1]
			go v.resolve(q, reply)
		}),
		"error": fn(func(a []js.Value) {
			err := errors.New(a[0].String())
			if len(a) > 2 && a[2].Truthy() {
				err = errNeverPull{err}
			}
			if !settle(err) {
				// the VM failed while the container ran
				fmt.Fprintln(log, "error:", err)
			}
			select {
			case v.exited <- 128:
			default:
			}
		}),
	}
	v.h = vm.Call("start", js.ValueOf(opts), js.ValueOf(on))
	select {
	case err := <-v.ready:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// wait returns the exit code of the container once it exits.
func (v *v86VM) wait() <-chan int { return v.exited }

func (v *v86VM) stop() {
	if v.h.Type() == js.TypeObject {
		v.h.Call("stop")
	}
	funcs := v.funcs
	v.funcs = nil
	// the worker may still have sent messages that are not delivered yet
	time.AfterFunc(time.Minute, func() {
		for _, f := range funcs {
			f.Release()
		}
	})
}

// dial opens a TCP connection to port of the pod, through the network card
// of the VM.
func (v *v86VM) dial(port int32) (net.Conn, error) { return v.dialFrom(port, "") }

// dialFrom is dial with a source address: another pod, or "" for the
// gateway of the pod.
func (v *v86VM) dialFrom(port int32, src string) (net.Conn, error) {
	if v.h.Type() != js.TypeObject {
		return nil, errors.New("the container is not running")
	}
	from := src
	if from == "" {
		from = v.tp.n.gateway()
	}
	c := newVMConn(&net.TCPAddr{IP: net.ParseIP(from)}, &net.TCPAddr{IP: net.ParseIP(v.tp.ip), Port: int(port)})
	onConnect, onData, onEnd, onClose := c.callbacks()
	c.h = v.h.Call("connect", int(port), onConnect, onData, onEnd, onClose, src)
	select {
	case ok := <-c.connected:
		if !ok {
			c.Close()
			return nil, fmt.Errorf("dial tcp %s:%d: connect: connection refused", v.tp.ip, port)
		}
	case <-time.After(10 * time.Second):
		c.Close()
		return nil, fmt.Errorf("dial tcp %s:%d: i/o timeout", v.tp.ip, port)
	}
	return c, nil
}

// outbound takes a TCP connection that the pod opened to ip:port in the
// cluster network (vmworker.js holds the SYN): it reaches the destination
// first and then accepts it, else the pod gets a reset, as from a host
// where nothing listens.
func (v *v86VM) outbound(id int, ip string, port int) {
	ctx, cancel := context.WithTimeout(v.tp.ctx, 10*time.Second)
	defer cancel()
	dst, err := dialFromPod(ctx, v.tp.ip, ip, port)
	if err != nil {
		v.h.Call("refuse", id)
		return
	}
	c := newVMConn(&net.TCPAddr{IP: net.ParseIP(ip), Port: port}, &net.TCPAddr{IP: net.ParseIP(v.tp.ip)})
	_, onData, onEnd, onClose := c.callbacks()
	c.h = v.h.Call("accept", id, onData, onEnd, onClose)
	pipeTCP(c, dst)
}

// resolve answers a DNS query of the pod (clusternet.go).
func (v *v86VM) resolve(query []byte, reply js.Value) {
	answer := clusterDNS(v.tp.namespace, query)
	if answer == nil {
		reply.Invoke(js.Null())
	} else {
		arr := js.Global().Get("Uint8Array").New(len(answer))
		js.CopyBytesToJS(arr, answer)
		reply.Invoke(arr)
	}
}

func (v *v86VM) String() string { return "v86 " + strconv.Quote(v.id) }
