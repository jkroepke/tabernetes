// tabernetes runs a Kubernetes control plane (kube-apiserver, kube-controller-manager,
// kube-scheduler and an in-memory etcd replacement) plus kubectl inside a
// single Go binary built for GOOS=js.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall/js"
	"time"

	"github.com/spf13/cobra"

	"k8s.io/client-go/rest"
	logsapi "k8s.io/component-base/logs/api/v1"

	"k8s.io/kubernetes/cmd/kube-apiserver/app"
	cmapp "k8s.io/kubernetes/cmd/kube-controller-manager/app"
	schedapp "k8s.io/kubernetes/cmd/kube-scheduler/app"
	"k8s.io/kubernetes/cmd/tabernetes/etcdshim"
)

const (
	dataDir      = "/tmp/tabernetes"
	advertiseIP  = "10.0.0.1"
	serviceCIDR  = "10.96.0.0/16"
	clusterCIDR  = "10.244.0.0/16"
	kubernetesIP = "10.96.0.1"
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "tabernetes:", err)
		panic(err)
	}
}

var status = func(string) {}

// bootTimeout bounds the wait for kube-apiserver. It is ready after about
// 7 seconds; a slow machine may take a few times that.
const bootTimeout = 3 * time.Minute

// run starts a component. The channel gets its error once it exits.
func run(name string, cmd *cobra.Command, args []string) <-chan error {
	cmd.SetArgs(args)
	exited := make(chan error, 1)
	go func() {
		err := cmd.Execute()
		if err == nil {
			err = fmt.Errorf("%s exited", name)
		} else {
			status(name + " failed: " + err.Error())
		}
		fmt.Fprintln(os.Stderr, name, "exited:", err)
		exited <- err
	}()
	return exited
}

type cluster struct {
	ca         *pki
	adminToken string
}

// current is the cluster once it booted.
var current *cluster

func boot() (*cluster, error) {
	// All components share one process and therefore one klog instance.
	logsapi.ReapplyHandling = logsapi.ReapplyHandlingIgnoreUnchanged
	// Deprecation warnings of the in-process controllers are only noise.
	rest.SetDefaultWarningHandler(rest.NoWarnings{})
	must(os.MkdirAll(p("certs"), 0o755))
	must(os.MkdirAll("/root", 0o755))
	os.Setenv("HOME", "/root")

	// The cluster of the last visit (state.go), or a new one.
	st := loadState()
	var c *cluster
	if st != nil {
		status("restoring the cluster of your last visit")
		var err error
		if c, err = st.restoreFiles(); err != nil {
			return nil, fmt.Errorf("restoring the cluster: %w", err)
		}
	} else {
		c = newCluster()
	}
	cfg, err := os.ReadFile(p("admin.kubeconfig"))
	must(err)
	must(os.MkdirAll("/root/.kube", 0o755))
	must(os.WriteFile("/root/.kube/config", cfg, 0o600))

	status("starting etcd")
	l, err := net.Listen("tcp", "127.0.0.1:2379")
	must(err)
	// The page keeps the cluster once it has booted: a reload in the middle
	// of the first boot must not find a cluster without its addons. The
	// leases are heartbeats: they alone are no reason to write it again.
	booted := make(chan struct{})
	opts := etcdshim.Options{
		Save:     stateSaver(stateFiles()),
		Interval: time.Second,
		Ready:    booted,
		Volatile: []string{"/registry/leases/", "/registry/masterleases/", "/registry/peerserverleases/"},
	}
	if st != nil {
		opts.Snapshot = st.Etcd
	}
	go func() { must(etcdshim.ServeWith(l, opts)) }()

	status("starting kube-apiserver")
	apiserver := run("kube-apiserver", app.NewAPIServerCommand(), apiserverArgs())
	if err := waitReady(c, "https://127.0.0.1:6443/readyz", apiserver); err != nil {
		return nil, fmt.Errorf("kube-apiserver did not get ready: %w", err)
	}
	if st != nil {
		status("removing the nodes of your last visit")
		if err := dropOldNodes(context.Background()); err != nil {
			return nil, fmt.Errorf("removing the old nodes: %w", err)
		}
	}

	startControllers()

	// A restored cluster has its addons, maybe changed by the user. It gets
	// them again only when this build brings other ones.
	switch {
	case st == nil:
		status("installing addons")
		if err := installAddons(context.Background(), false); err != nil {
			return nil, fmt.Errorf("installing addons: %w", err)
		}
	case st.Addons != addonsHash():
		status("updating the addons")
		if err := installAddons(context.Background(), true); err != nil {
			// the cluster of the user is worth more than a new addon
			fmt.Fprintln(os.Stderr, "tabernetes: updating the addons:", err)
			status("the addons are not up to date: " + err.Error())
		}
	}
	close(booted)
	status("ready")
	current = c
	return c, nil
}

// newCluster creates the PKI, the tokens and the kubeconfigs of a new
// cluster in dataDir.
func newCluster() *cluster {
	status("generating PKI")
	ca := newPKI("ca", "tabernetes-ca")
	ca.issue("apiserver", "kube-apiserver", nil, true,
		[]string{"localhost", "kubernetes", "kubernetes.default", "kubernetes.default.svc", "kubernetes.default.svc.cluster.local"},
		[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP(advertiseIP), net.ParseIP(kubernetesIP)})
	ca.issue("apiserver-kubelet-client", "kube-apiserver-kubelet-client", []string{"system:masters"}, false, nil, nil)
	// Aggregated API servers and the delegated authentication of the
	// controller-manager and scheduler expect a request header CA.
	newPKI("front-proxy-ca", "tabernetes-front-proxy-ca").issue("front-proxy-client", "front-proxy-client", nil, false, nil, nil)
	saKey := newKey()
	must(os.WriteFile(p("sa.key"), keyPEM(saKey), 0o600))
	pub, err := x509.MarshalPKIXPublicKey(&saKey.PublicKey)
	must(err)
	must(os.WriteFile(p("sa.pub"), pemBlock("PUBLIC KEY", pub), 0o644))

	c := &cluster{ca: ca, adminToken: randToken(32)}
	cmToken, schedToken := randToken(32), randToken(32)
	must(os.WriteFile(p("tokens.csv"), []byte(strings.Join([]string{
		c.adminToken + ",kubernetes-admin,kubernetes-admin,system:masters",
		cmToken + ",system:kube-controller-manager,system:kube-controller-manager",
		schedToken + ",system:kube-scheduler,system:kube-scheduler",
	}, "\n")+"\n"), 0o600))
	writeKubeconfig("admin", c.adminToken)
	writeKubeconfig("controller-manager", cmToken)
	writeKubeconfig("scheduler", schedToken)
	return c
}

func apiserverArgs() []string {
	return []string{
		"--etcd-servers=http://127.0.0.1:2379",
		"--bind-address=127.0.0.1",
		"--advertise-address=" + advertiseIP,
		"--secure-port=6443",
		"--cert-dir=" + p("certs"),
		"--tls-cert-file=" + p("apiserver.crt"),
		"--tls-private-key-file=" + p("apiserver.key"),
		"--client-ca-file=" + p("ca.crt"),
		"--kubelet-client-certificate=" + p("apiserver-kubelet-client.crt"),
		"--kubelet-client-key=" + p("apiserver-kubelet-client.key"),
		"--kubelet-preferred-address-types=InternalIP,Hostname",
		"--requestheader-client-ca-file=" + p("front-proxy-ca.crt"),
		"--requestheader-allowed-names=front-proxy-client",
		"--requestheader-username-headers=X-Remote-User",
		"--requestheader-group-headers=X-Remote-Group",
		"--requestheader-extra-headers-prefix=X-Remote-Extra-",
		"--proxy-client-cert-file=" + p("front-proxy-client.crt"),
		"--proxy-client-key-file=" + p("front-proxy-client.key"),
		"--service-account-issuer=https://kubernetes.default.svc.cluster.local",
		"--service-account-key-file=" + p("sa.pub"),
		"--service-account-signing-key-file=" + p("sa.key"),
		"--token-auth-file=" + p("tokens.csv"),
		"--authorization-mode=Node,RBAC",
		"--enable-bootstrap-token-auth=true",
		"--allow-privileged=true",
		// Priority and fairness protects a shared apiserver from noisy
		// clients. The browser has one user; without it the boot is about a
		// second faster.
		"--enable-priority-and-fairness=false",
		"--service-cluster-ip-range=" + serviceCIDR,
		"--profiling=false",
		"--v=1",
	}
}

func startControllers() {
	status("starting kube-controller-manager")
	run("kube-controller-manager", cmapp.NewControllerManagerCommand(), []string{
		"--kubeconfig=" + p("controller-manager.kubeconfig"),
		"--authentication-kubeconfig=" + p("controller-manager.kubeconfig"),
		"--authorization-kubeconfig=" + p("controller-manager.kubeconfig"),
		"--bind-address=127.0.0.1",
		"--secure-port=10257",
		"--cert-dir=" + p("certs"),
		"--cluster-signing-cert-file=" + p("ca.crt"),
		"--cluster-signing-key-file=" + p("ca.key"),
		"--root-ca-file=" + p("ca.crt"),
		"--service-account-private-key-file=" + p("sa.key"),
		"--use-service-account-credentials=true",
		"--controllers=*,bootstrapsigner,tokencleaner",
		"--allocate-node-cidrs=true",
		"--cluster-cidr=" + clusterCIDR,
		"--service-cluster-ip-range=" + serviceCIDR,
		"--leader-elect=false",
		"--profiling=false",
		"--v=1",
	})

	status("starting kube-scheduler")
	run("kube-scheduler", schedapp.NewSchedulerCommand(), []string{
		"--kubeconfig=" + p("scheduler.kubeconfig"),
		"--authentication-kubeconfig=" + p("scheduler.kubeconfig"),
		"--authorization-kubeconfig=" + p("scheduler.kubeconfig"),
		"--bind-address=127.0.0.1",
		"--secure-port=10259",
		"--leader-elect=false",
		"--profiling=false",
		"--v=1",
	})
}

// waitReady polls url until it answers 200. It gives up when the component
// exits or after bootTimeout, with the last failure as the error.
func waitReady(c *cluster, url string, exited <-chan error) error {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(c.ca.caPEM)
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool},
		DialContext:     (&net.Dialer{}).DialContext, // force Go networking instead of fetch()
	}}
	deadline := time.After(bootTimeout)
	for {
		req, _ := http.NewRequest(http.MethodGet, url+"?verbose", nil)
		req.Header.Set("Authorization", "Bearer "+c.adminToken)
		resp, err := hc.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			var failed []string
			for _, l := range strings.Split(string(body), "\n") {
				if strings.HasPrefix(l, "[-]") {
					failed = append(failed, l)
				}
			}
			err = fmt.Errorf("status %d: %s", resp.StatusCode, strings.Join(failed, ", "))
		}
		select {
		case exitErr := <-exited:
			return exitErr
		case <-deadline:
			return fmt.Errorf("no answer after %v: %w", bootTimeout, err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// promise wraps fn into a JS Promise.
func promise(fn func() (any, error)) js.Value {
	return js.Global().Get("Promise").New(js.FuncOf(func(this js.Value, a []js.Value) any {
		resolve, reject := a[0], a[1]
		go func() {
			v, err := fn()
			if err != nil {
				reject.Invoke(js.Global().Get("Error").New(err.Error()))
				return
			}
			resolve.Invoke(v)
		}()
		return nil
	}))
}

func main() {
	// The allocation profiler takes a stack trace now and then. When that
	// happens in runtime.beforeIdle, the js/wasm runtime cannot unwind the
	// stack and dies with "invalid runtime symbol table". Nobody profiles
	// the browser tab, so turn it off.
	runtime.MemProfileRate = 0
	// wasm32 ends at 4 GiB. With GOGC=100 the heap may grow to twice the live
	// heap first; the limit makes the GC work harder before it gets there.
	debug.SetMemoryLimit(3 << 30)
	api := js.Global().Get("Object").New()
	js.Global().Set("tabernetes", api)
	if cb := js.Global().Get("tabernetesStatus"); cb.Type() == js.TypeFunction {
		status = func(s string) { cb.Invoke(s) }
	}

	// tabernetesRole "tab": the control plane runs in a SharedWorker, this
	// tab joins it (tabs.go). Otherwise this instance boots the control plane,
	// in a tab of its own or in the worker.
	if r := js.Global().Get("tabernetesRole"); r.Type() == js.TypeString {
		role = r.String()
	}
	var c *cluster
	ready := promise(func() (any, error) {
		var err error
		if role == "tab" {
			c, err = follow(js.Global().Get("tabernetesCluster"))
		} else {
			c, err = boot()
			// a tab that runs the control plane itself is a node too; the
			// SharedWorker has no document and is none
			if err == nil && js.Global().Get("document").Truthy() {
				registerTab()
			}
		}
		return nil, err
	})
	api.Set("ready", ready)

	// shell(src: string, write: (s: string) => void, builtins: {[name]: (args, {write, stdin}) => Promise}): Promise<number>
	sh := newShell()
	api.Set("shell", js.FuncOf(func(this js.Value, a []js.Value) any {
		src, out, builtins := a[0].String(), a[1], js.Undefined()
		if len(a) > 2 {
			builtins = a[2]
		}
		return promise(func() (any, error) { return sh.run(src, out, builtins), nil })
	}))
	// incomplete(src: string): boolean, true while src needs more lines
	api.Set("incomplete", js.FuncOf(func(this js.Value, a []js.Value) any { return incomplete(a[0].String()) }))
	// commands(): {tools: string[], builtins: string[]} for the help text
	api.Set("commands", js.FuncOf(func(js.Value, []js.Value) any { return commands() }))
	// rawMode(): boolean, true while a full screen program (vi) wants every key
	api.Set("rawMode", js.FuncOf(func(js.Value, []js.Value) any { return tty.raw.Load() }))
	// input(data: string) passes keys to a raw mode program
	api.Set("input", js.FuncOf(func(this js.Value, a []js.Value) any { tty.input(a[0].String()); return nil }))
	// resize(cols: number, rows: number) tells programs the terminal size
	api.Set("resize", js.FuncOf(func(this js.Value, a []js.Value) any { tty.resize(a[0].Int(), a[1].Int()); return nil }))
	// complete(line: string, builtins: string[]): Promise<{word, items, noSpace}>, tab completion
	api.Set("complete", js.FuncOf(func(this js.Value, a []js.Value) any {
		line := a[0].String()
		var builtins []string
		if len(a) > 1 && a[1].Type() == js.TypeObject {
			for i := range a[1].Length() {
				builtins = append(builtins, a[1].Index(i).String())
			}
		}
		return promise(func() (any, error) {
			c := sh.complete(line, builtins)
			items := make([]any, len(c.items))
			for i, it := range c.items {
				items[i] = it
			}
			return map[string]any{"word": c.word, "items": items, "noSpace": c.noSpace}, nil
		})
	}))
	// cwd(): string, the working directory of the shell
	api.Set("cwd", js.FuncOf(func(js.Value, []js.Value) any { return sh.dir() }))
	// cancel() stops the running command line (Ctrl-C)
	api.Set("cancel", js.FuncOf(func(js.Value, []js.Value) any { sh.stop(); return nil }))

	// proxy(port, method, path, headers, body): Promise<{status, statusText, headers, body}>, see proxy.go
	api.Set("proxy", js.FuncOf(func(this js.Value, a []js.Value) any { return proxyJS(a) }))
	// tabernetes.dial(port, onData, onClose) for the VNC viewer, see dial.go.
	api.Set("dial", js.FuncOf(func(this js.Value, a []js.Value) any { return dialJS(a) }))

	// prepareJoin(open: (label: string) => Promise<RTCDataChannel>): Promise<{name, ip, token, ca, caHash}>
	api.Set("prepareJoin", js.FuncOf(func(this js.Value, a []js.Value) any {
		open := a[0]
		return promise(func() (any, error) {
			if c == nil {
				return nil, fmt.Errorf("cluster is not ready yet")
			}
			if role == "tab" {
				return nil, fmt.Errorf("the control plane in the shared worker prepares joins")
			}
			return c.prepareJoin(open)
		})
	}))
	// attachChannel(dc: RTCDataChannel) for channels opened by a node agent.
	api.Set("attachChannel", js.FuncOf(func(this js.Value, a []js.Value) any {
		attachChannel(a[0])
		return nil
	}))
	// attachPort(port: MessagePort, label: string) for connections that the
	// other side of a tab and the shared worker opened (tabs.go).
	api.Set("attachPort", js.FuncOf(func(this js.Value, a []js.Value) any {
		attach(newPortConn(a[0]), a[1].String())
		return nil
	}))
	// prepareTab(open: (label: string) => Promise<MessagePort>): Promise<{name, ip, token, files}>
	// adds a tab to the control plane in the shared worker.
	api.Set("prepareTab", js.FuncOf(func(this js.Value, a []js.Value) any {
		open := a[0]
		return promise(func() (any, error) {
			if c == nil || role == "tab" {
				return nil, fmt.Errorf("no control plane here")
			}
			return c.prepareTab(open)
		})
	}))
	// dropNode(name: string) removes a node of a closed tab (tabs.go).
	api.Set("dropNode", js.FuncOf(func(this js.Value, a []js.Value) any {
		name := a[0].String()
		go dropNode(name)
		return nil
	}))

	select {}
}
