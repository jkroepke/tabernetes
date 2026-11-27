package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall/js"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// A container image on the tab node runs in v86 (https://github.com/copy/v86),
// an x86 emulator that compiles x86 code to WebAssembly. The page pulls the
// linux/386 variant of the image from a pull-through mirror of Docker Hub
// (Docker Hub sends no CORS headers), unpacks the layers into the image store
// of the browser and boots a small Linux kernel with them as its root file
// system over 9p (web/src/vm.js, vmworker.js, layers.js, rootfs.js). The VM
// is the sandbox of the pod: PID 1 of the VM (vm/init) starts and stops its
// containers as runc would, on the command of the kubelet here. The console
// of the VM is the log of the first container, port-forward reaches the
// ports of the pod through the network card of the VM.

// v86Pod is the VM of a pod.
type v86Pod struct {
	tp    *tabPod
	gen   int      // the run of the sandbox; events of an old one are dropped
	h     js.Value // the handle of vm.js start()
	funcs []js.Func
	ids   map[int]string // image IDs by container index
	next  int            // the index of the next container (kubectl debug)
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

func (v *v86Pod) runtime() string { return "v86" }

// gateway is the first address of the pod CIDR of the node, as a bridge
// would have it.
func (n *tabNode) gateway() string {
	ip := n.podCIDR.IP.To4()
	return net.IPv4(ip[0], ip[1], ip[2], ip[3]+1).String()
}

// podEvent is what happens to a pod: an event of its VM, a probe result, a
// timer of a restart, a container of kubectl debug.
type podEvent struct {
	kind    string
	gen     int
	i       int     // the container in the VM
	ctr     *tabCtr // for probe, restart and add
	run     int     // the run of ctr, for probes
	code    int
	msg     string
	size    int64
	imageID string
	pull    bool
	never   bool
	ok      bool
}

// post hands e to the loop of the pod. It never blocks: a JS callback may
// call it.
func (tp *tabPod) post(e podEvent) {
	tp.evMu.Lock()
	tp.evs = append(tp.evs, e)
	tp.evMu.Unlock()
	select {
	case tp.notify <- struct{}{}:
	default:
	}
}

func (tp *tabPod) drain() []podEvent {
	tp.evMu.Lock()
	defer tp.evMu.Unlock()
	evs := tp.evs
	tp.evs = nil
	return evs
}

// vmSize is the memory and the ephemeral storage of the VM: the limits of
// the containers that run at the same time (the containers and sidecars,
// or the largest init container), as the scheduler counts the requests of
// a pod. 0 without any limit: the default of vmworker.js.
func (tp *tabPod) vmSize(r corev1.ResourceName) int64 {
	var sum, initMax int64
	for _, c := range tp.ctrs {
		q, ok := c.spec.Resources.Limits[r]
		switch {
		case !ok || c.kind == ctrEphemeral:
		case c.kind == ctrInit && !c.sidecar:
			initMax = max(initMax, q.Value())
		default:
			sum += q.Value()
		}
	}
	return max(sum, initMax)
}

// vmContainer is a container for the page (vmworker.js): what the image
// config of the image does not say.
func (tp *tabPod) vmContainer(c *tabCtr, env []string) map[string]any {
	strs := func(l []string) []any {
		out := make([]any, len(l))
		for i, s := range l {
			out[i] = s
		}
		return out
	}
	m := map[string]any{
		"index": c.index,
		"name":  c.spec.Name,
		"image": c.spec.Image,
		// the API server defaults it: Always for :latest, else IfNotPresent
		"pullPolicy": string(c.spec.ImagePullPolicy),
		"command":    strs(expandAll(c.spec.Command, env)),
		"args":       strs(expandAll(c.spec.Args, env)),
		"env":        strs(env),
		"workingDir": c.spec.WorkingDir,
		"stdin":      c.spec.Stdin,
		"stdinOnce":  c.spec.StdinOnce,
		"tty":        c.spec.TTY,
		"console":    c == tp.main(),
		"user":       tp.user(c),
		"mounts":     tp.vmMounts(c),
	}
	return m
}

// startVM starts the VM of the pod with all of its init containers and
// containers; the first ones start at once.
func (tp *tabPod) startVM(gen int) *v86Pod {
	v := &v86Pod{tp: tp, gen: gen, ids: map[int]string{}}
	host := tp.n.host.Get("vm")
	cfg, pending := tp.localConfig()
	var containers, start []any
	tp.mu.Lock()
	for _, c := range tp.ctrs {
		if c.kind == ctrEphemeral {
			// kubectl debug containers do not come back with a new sandbox
			continue
		}
		c.index = v.next
		v.next++
		c.pull = &imagePull{c: c, t0: time.Now()}
		var env []string
		if !pending {
			env = cfg.env[c.spec.Name]
		}
		containers = append(containers, tp.vmContainer(c, env))
	}
	// the first init container, or else all containers
	for _, c := range tp.ctrs {
		if c.kind == ctrInit || c.kind == ctrRegular && len(tp.spec.InitContainers) == 0 {
			start = append(start, c.index)
			tp.newRun(c)
			c.pending = true
			if c.kind == ctrInit {
				break
			}
		}
	}
	tp.mu.Unlock()
	opts := map[string]any{
		"hostname": tp.hostname,
		"ip":       tp.ip,
		"gateway":  tp.n.gateway(),
		"hosts":    tp.hostsFile(),
		// what the network card hands to the cluster network (clusternet.go)
		"resolvConf":   tp.resolvConf(),
		"clusterCIDRs": []any{clusterCIDR, serviceCIDR},
		"memory":       tp.vmSize(corev1.ResourceMemory),
		// df shows it as the size of the root file system (web/src/vmworker.js)
		"ephemeralStorage": tp.vmSize(corev1.ResourceEphemeralStorage),
		"containers":       containers,
		"start":            start,
	}
	if !pending {
		opts["config"] = tp.configJS(cfg)
	}
	// may the pod reach names outside the cluster (netpol.go)? Not known
	// before the first pod started: then it may, until Go tells it.
	if allowed, known := egressAllowed(tp.ip); known {
		opts["egress"] = allowed
	}
	v.h = host.Call("start", js.ValueOf(opts), js.ValueOf(v.callbacks()))
	addVM(v)
	if pending {
		go tp.resolveConfig(v)
	}
	return v
}

// resolveConfig reads the ConfigMaps and Secrets of the pod and asks for
// its token while the images come, and hands them to the VM. It tries again
// while one is missing, as the kubelet does.
func (tp *tabPod) resolveConfig(v *v86Pod) {
	var last string
	for tp.ctx.Err() == nil {
		cfg, err := tp.apiConfig(tp.ctx)
		if err == nil {
			v.h.Call("config", tp.configJS(cfg))
			return
		}
		if msg := err.Error(); msg != last {
			last = msg
			var ce *configError
			if errors.As(err, &ce) && ce.ctr != nil {
				ce.ctr.event(corev1.EventTypeWarning, "Failed", "Error: "+ce.Error())
				tp.setWaiting(ce.ctr, "CreateContainerConfigError", ce.Error())
			} else {
				tp.event(corev1.EventTypeWarning, "FailedMount", err.Error())
			}
		}
		if !tp.sleep(2 * time.Second) {
			return
		}
	}
}

func (v *v86Pod) callbacks() map[string]any {
	tp := v.tp
	fn := func(f func(a []js.Value)) js.Func {
		jf := js.FuncOf(func(_ js.Value, a []js.Value) any { f(a); return nil })
		v.funcs = append(v.funcs, jf)
		return jf
	}
	post := func(e podEvent) {
		e.gen = v.gen
		tp.post(e)
	}
	return map[string]any{
		"pulling": fn(func(a []js.Value) { post(podEvent{kind: "pulling", i: a[0].Int()}) }),
		"pulled": fn(func(a []js.Value) {
			post(podEvent{kind: "pulled", i: a[0].Int(), imageID: a[1].Get("imageID").String(), size: int64(a[1].Get("size").Float())})
		}),
		"started": fn(func(a []js.Value) {
			post(podEvent{kind: "started", i: a[0].Int()})
			// the informers of the cluster network and the policies start
			// now, after the first pod of a tab, not before it
			go clusterNet(context.Background())
		}),
		// the console: the first container, and the kernel
		"console": fn(func(a []js.Value) { tp.writeLog(tp.main(), a[0].String()) }),
		"log": fn(func(a []js.Value) {
			if c := tp.ctrAt(a[0].Int()); c != nil {
				tp.writeLog(c, a[1].String())
			}
		}),
		"exit": fn(func(a []js.Value) {
			post(podEvent{kind: "exit", i: a[0].Int(), code: a[1].Int(), msg: a[2].String()})
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
			post(podEvent{kind: "error", msg: a[0].String(), pull: a[1].Truthy(), never: a[2].Truthy(), i: a[3].Int()})
		}),
	}
}

// writeLog adds output of c to its log.
func (tp *tabPod) writeLog(c *tabCtr, s string) {
	tp.mu.Lock()
	logs := c.logs
	tp.mu.Unlock()
	logs.Write([]byte(s))
}

// ctrAt is the container with the index i in the VM.
func (tp *tabPod) ctrAt(i int) *tabCtr {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	for _, c := range tp.ctrs {
		if c.index == i {
			return c
		}
	}
	return nil
}

func (v *v86Pod) command(line string) {
	if v.h.Type() == js.TypeObject {
		v.h.Call("command", line)
	}
}

// kill signals the processes of a container.
func (v *v86Pod) kill(c *tabCtr, sig int) {
	v.command(fmt.Sprintf("kill %d %d", c.index, sig))
}

func (v *v86Pod) stop() {
	removeVM(v)
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

type driveResult int

const (
	driveDone  driveResult = iota // the pod ended, or it is gone
	driveRetry                    // a new sandbox after the back-off
)

// runVM runs the pod in a VM, and in a new one after the VM failed or an
// image did not come.
func (tp *tabPod) runVM() {
	backoff := backoffStart
	for gen := 1; tp.ctx.Err() == nil; gen++ {
		v := tp.startVM(gen)
		res, pullCtr, pullErr := tp.drive(v)
		v.stop()
		tp.mu.Lock()
		if tp.w == v {
			tp.w = nil
		}
		tp.mu.Unlock()
		if res != driveRetry || !tp.sleep(backoff) {
			return
		}
		if pullCtr != nil {
			tp.pullBackOff(pullCtr, pullErr)
		}
		backoff = min(backoff*2, backoffMax)
	}
}

// drive runs the containers of the pod in the VM v until the pod ends or
// the VM fails.
func (tp *tabPod) drive(v *v86Pod) (driveResult, *tabCtr, error) {
	var inits []*tabCtr
	for _, c := range tp.ctrs {
		if c.kind == ctrInit {
			inits = append(inits, c)
		}
	}
	next := 0 // the init container that runs, or len(inits)
	// advance goes on after the init container next completed (or started,
	// a sidecar): the next one, or the containers.
	advance := func(c *tabCtr) {
		if next >= len(inits) || inits[next] != c {
			return
		}
		next++
		if next < len(inits) {
			tp.startCtr(v, inits[next])
			return
		}
		tp.update()
		for _, c := range tp.ctrs {
			if c.kind == ctrRegular {
				tp.startCtr(v, c)
			}
		}
	}
	// finished: every container ended for good. The sidecars stop too, and
	// the pod is done.
	finished := func() bool {
		tp.mu.Lock()
		defer tp.mu.Unlock()
		code := 0
		for _, c := range tp.ctrs {
			if c.kind != ctrRegular {
				continue
			}
			if !c.done {
				return false
			}
			if t := c.status.State.Terminated; t != nil && t.ExitCode != 0 {
				code = 1
			}
		}
		tp.phase = corev1.PodSucceeded
		if code != 0 {
			tp.phase = corev1.PodFailed
		}
		return true
	}
	for {
		select {
		case <-tp.ctx.Done():
			return driveDone, nil, nil
		case <-tp.notify:
		}
		for _, e := range tp.drain() {
			if e.gen != v.gen && e.kind != "add" {
				continue
			}
			c := e.ctr
			if c == nil && e.i >= 0 {
				c = tp.ctrAt(e.i)
			}
			switch e.kind {
			case "pulling":
				if c != nil && c.pull != nil {
					c.pull.fetching()
				}
			case "pulled":
				if c != nil && c.pull != nil {
					v.ids[e.i] = e.imageID
					c.pull.done(e.size)
				}
			case "started":
				if c == nil {
					break
				}
				tp.mu.Lock()
				c.pending = false
				tp.w = v
				if c.kind == ctrRegular {
					tp.phase = corev1.PodRunning
				}
				run := c.run
				tp.mu.Unlock()
				c.event(corev1.EventTypeNormal, "Started", "Started container "+c.spec.Name)
				tp.setRunning(c, v.ids[c.index], v.runtime())
				tp.startProbes(v, c, run)
				if c.sidecar && c.spec.StartupProbe == nil {
					advance(c)
				}
			case "exit":
				if c == nil {
					break
				}
				reason, msg := "Completed", ""
				switch {
				case e.msg != "":
					// the kubelet tells the error of runc only in the events
					reason = "StartError"
					msg = "failed to create containerd task: failed to create shim task: OCI runtime create failed: " +
						"runc create failed: unable to start container process: error during container init: " + e.msg + ": unknown"
					c.event(corev1.EventTypeWarning, "Failed", "Error: "+msg)
				case e.code != 0:
					reason = "Error"
				}
				restart := tp.restarts(c, e.code)
				tp.mu.Lock()
				c.pending = false
				ran := time.Since(c.runAt)
				tp.mu.Unlock()
				tp.setTerminated(c, e.code, reason, msg, restart)
				switch {
				case c.kind == ctrInit && !c.sidecar && e.code == 0:
					advance(c)
				case c.kind == ctrInit && !c.sidecar && !restart:
					// the pod cannot initialize
					tp.mu.Lock()
					tp.phase = corev1.PodFailed
					tp.mu.Unlock()
					tp.update()
					return driveDone, nil, nil
				}
				if restart {
					tp.restartLater(v, c, ran)
				} else if c.kind == ctrRegular && finished() {
					tp.stopSidecars(v)
					tp.update()
					return driveDone, nil, nil
				}
			case "restart":
				tp.mu.Lock()
				ok := !c.done && !c.pending && c.status.State.Running == nil
				tp.mu.Unlock()
				if ok {
					tp.startCtr(v, c)
				}
			case "probe":
				tp.probed(v, c, e, advance)
			case "add":
				tp.mu.Lock()
				c.index = v.next
				v.next++
				c.pull = &imagePull{c: c, t0: time.Now()}
				tp.newRun(c)
				c.pending = true
				tp.mu.Unlock()
				env, err := tp.containerEnv(tp.ctx, c)
				if err != nil {
					c.event(corev1.EventTypeWarning, "Failed", "Error: "+err.Error())
					tp.setWaiting(c, "CreateContainerConfigError", err.Error())
					break
				}
				v.h.Call("add", js.ValueOf(tp.vmContainer(c, env)))
			case "error":
				if e.pull && c != nil {
					err := errors.New(e.msg)
					if e.never {
						err = errNeverPull{err}
					}
					tp.pullFailed(c, err)
					if c.kind == ctrEphemeral {
						break
					}
					return driveRetry, c, err
				}
				if c != nil && c.kind == ctrEphemeral {
					tp.setTerminated(c, 128, "StartError", e.msg, false)
					break
				}
				// the VM failed: its containers ended
				retry := false
				for _, c := range tp.ctrs {
					tp.mu.Lock()
					skip := c.done || c.kind == ctrEphemeral || c.status.State.Running == nil && !c.pending
					tp.mu.Unlock()
					if skip {
						continue
					}
					tp.writeLog(c, "error: "+e.msg+"\n")
					restart := tp.restarts(c, 128)
					retry = retry || restart
					tp.setTerminated(c, 128, "Error", e.msg, restart)
				}
				if !retry {
					finished()
					tp.update()
					return driveDone, nil, nil
				}
				return driveRetry, nil, nil
			}
		}
	}
}

// startCtr starts c in the VM.
func (tp *tabPod) startCtr(v *v86Pod, c *tabCtr) {
	tp.mu.Lock()
	tp.newRun(c)
	c.pending = true
	tp.mu.Unlock()
	// what the kubelet says when it creates a container again
	if c.status.RestartCount > 0 {
		c.event(corev1.EventTypeNormal, "Pulled", fmt.Sprintf("Container image %q already present on machine and can be accessed by the pod", c.spec.Image))
		c.event(corev1.EventTypeNormal, "Created", "Created container: "+c.spec.Name)
	}
	v.command("start " + strconv.Itoa(c.index))
}

// restartLater starts c again after its back-off, as CrashLoopBackOff.
func (tp *tabPod) restartLater(v *v86Pod, c *tabCtr, ran time.Duration) {
	tp.mu.Lock()
	// the kubelet forgets the back-off of a container that ran for 10 minutes
	if c.backoff == 0 || ran > 10*time.Minute {
		c.backoff = backoffStart
	}
	d := c.backoff
	c.backoff = min(c.backoff*2, backoffMax)
	tp.mu.Unlock()
	tp.crashLoop(c, d)
	time.AfterFunc(d, func() { tp.post(podEvent{kind: "restart", gen: v.gen, ctr: c}) })
}

// stopSidecars ends the sidecars once the containers are done.
func (tp *tabPod) stopSidecars(v *v86Pod) {
	for _, c := range tp.ctrs {
		tp.mu.Lock()
		running := c.sidecar && c.status.State.Running != nil
		tp.mu.Unlock()
		if running {
			c.event(corev1.EventTypeNormal, "Killing", "Stopping container "+c.spec.Name)
			v.kill(c, 15)
			tp.setTerminated(c, 0, "Completed", "", false)
		}
	}
}

// debug adds the ephemeral containers of kubectl debug that are new in pod.
func (tp *tabPod) debug(pod *corev1.Pod) {
	tp.mu.Lock()
	var added []*tabCtr
	for _, e := range pod.Spec.EphemeralContainers {
		if tp.container(e.Name) == nil {
			added = append(added, tp.addCtr(corev1.Container(e.EphemeralContainerCommon), ctrEphemeral))
		}
	}
	tp.pod.Spec.EphemeralContainers = pod.Spec.EphemeralContainers
	tp.mu.Unlock()
	for _, c := range added {
		tp.post(podEvent{kind: "add", ctr: c})
	}
	if len(added) > 0 {
		go tp.update()
	}
}

// dial opens a TCP connection to port of the pod, through the network card
// of the VM.
func (v *v86Pod) dial(port int32) (net.Conn, error) { return v.dialFrom(port, "") }

// dialFrom is dial with a source address: another pod, or "" for the
// gateway of the pod.
func (v *v86Pod) dialFrom(port int32, src string) (net.Conn, error) {
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
func (v *v86Pod) outbound(id int, ip string, port int) {
	ctx, cancel := context.WithTimeout(v.tp.ctx, 10*time.Second)
	defer cancel()
	dst, err := dialFromPod(ctx, v.tp.ip, ip, port)
	if errors.Is(err, errDropped) {
		// no answer: the pod sends the SYN again, and that one is checked
		// again
		v.h.Call("drop", id)
		return
	}
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
func (v *v86Pod) resolve(query []byte, reply js.Value) {
	ctx, cancel := context.WithTimeout(v.tp.ctx, 5*time.Second)
	defer cancel()
	if !dnsAllowed(ctx, v.tp.ip) {
		reply.Invoke(js.Null()) // dropped by a NetworkPolicy
		return
	}
	answer := clusterDNS(v.tp.namespace, query)
	if answer == nil {
		reply.Invoke(js.Null())
	} else {
		arr := js.Global().Get("Uint8Array").New(len(answer))
		js.CopyBytesToJS(arr, answer)
		reply.Invoke(arr)
	}
}
