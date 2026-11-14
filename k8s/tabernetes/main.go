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

	utilrand "k8s.io/apimachinery/pkg/util/rand"
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
	// A first visit starts from the cluster that the build booted
	// (seed.go): its objects without its PKI.
	seed := (*savedState)(nil)
	if st == nil {
		seed = loadSeed()
	} else {
		js.Global().Delete("tabernetesSeed")
	}
	makeSeed := js.Global().Get("tabernetesMakeSeed")
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
	switch {
	case st != nil:
		opts.Snapshot = st.Etcd
	case seed != nil:
		opts.Snapshot = seed.Etcd
	}
	if makeSeed.Type() == js.TypeFunction {
		opts.Save = seedSaver(makeSeed)
	}
	go func() { must(etcdshim.ServeWith(l, opts)) }()

	status("starting kube-apiserver")
	apiserver := run("kube-apiserver", app.NewAPIServerCommand(), apiserverArgs())
	if err := waitReady(c, "https://127.0.0.1:6443/readyz", apiserver); err != nil {
		return nil, fmt.Errorf("kube-apiserver did not get ready: %w", err)
	}
	if st != nil {
		// The paired docker nodes stay (join --keep): the tabs dial them again.
		if err := loadPairings(context.Background()); err != nil {
			return nil, fmt.Errorf("reading the paired nodes: %w", err)
		}
		status("removing the nodes of your last visit")
		if err := dropOldNodes(context.Background()); err != nil {
			return nil, fmt.Errorf("removing the old nodes: %w", err)
		}
	}

	// A restored cluster has its addons, maybe changed by the user. It gets
	// them again only when this build brings other ones.
	switch {
	case st == nil && seed != nil:
		// the seed has them
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
	// The controllers start after the addons, which need none of them, and
	// a moment after the cluster is ready: their start takes the CPU for
	// ~1.5 s, and Go in Wasm has no preemption. First the tab that waits
	// for the cluster opens its session and shows the prompt (~0.8 s sooner
	// on the first visit than when they shared the CPU with the addons).
	if makeSeed.Type() == js.TypeFunction {
		// The seed is the cluster before the controllers ran.
		close(booted)
		return c, nil
	}
	startControllers(250 * time.Millisecond)
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

// startControllers starts kube-controller-manager and kube-scheduler after
// a delay. The status lines come at once: later ones would end up below the
// prompt.
func startControllers(after time.Duration) {
	status("starting kube-controller-manager")
	status("starting kube-scheduler")
	time.AfterFunc(after, runControllers)
}

func runControllers() {
	// The scheduler first: it is ready ~0.3 s after its start when it has the
	// thread to itself, but took ~2.5 s while the controller-manager started
	// beside it, and a pod created at the prompt waited for it. The
	// controller-manager waits for it, at most 5 s.
	scheduler := runScheduler()
	if err := waitServing("https://127.0.0.1:10259/readyz", scheduler); err != nil {
		fmt.Fprintln(os.Stderr, "tabernetes: kube-scheduler is not ready:", err)
	}
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
		// All controllers use the client of the controller-manager (the
		// binding tabernetes:kube-controller-manager in addons/bootstrap.yaml),
		// not a service account and a token each: ~40 clients, tokens and TLS
		// connections less while the controllers start, right after the
		// prompt. Measured in headless Chromium (kubectl run 1.5 s after the
		// prompt, 14 rounds): run -> start message ~1.4 s -> 0.3-0.8 s.
		"--use-service-account-credentials=false",
		// The client of the controller-manager waits to stay under 20
		// requests per second (burst 30). With a service account and a
		// token for each of its ~40 controllers, that alone took ~5 s on the
		// first visit, idle time in which no controller ran: no pod CIDR
		// for the node, no default service account, the not-ready taint.
		// kubectl run failed or waited. All of it runs in this process,
		// there is nothing to protect.
		"--kube-api-qps=1000",
		"--kube-api-burst=1000",
		"--controllers=*,bootstrapsigner,tokencleaner",
		"--allocate-node-cidrs=true",
		"--cluster-cidr=" + clusterCIDR,
		"--service-cluster-ip-range=" + serviceCIDR,
		"--leader-elect=false",
		"--profiling=false",
		"--v=1",
	})
}

func runScheduler() <-chan error {
	return run("kube-scheduler", schedapp.NewSchedulerCommand(), []string{
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
	return poll(url, &tls.Config{RootCAs: pool}, c.adminToken, bootTimeout, exited)
}

// waitServing waits for /readyz of a component that serves with a
// self-signed certificate (kube-scheduler). The connection never leaves the
// process, and /readyz needs no login.
func waitServing(url string, exited <-chan error) error {
	return poll(url, &tls.Config{InsecureSkipVerify: true}, "", 5*time.Second, exited)
}

// poll asks url every 100 ms until it answers 200, the component exits or
// timeout passes.
func poll(url string, tlsConfig *tls.Config, token string, timeout time.Duration, exited <-chan error) error {
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: tlsConfig,
		DialContext:     (&net.Dialer{}).DialContext, // force Go networking instead of fetch()
	}}
	defer hc.CloseIdleConnections()
	deadline := time.After(timeout)
	for {
		req, _ := http.NewRequest(http.MethodGet, url+"?verbose", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
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
			return fmt.Errorf("no answer after %v: %w", timeout, err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// promise wraps fn into a JS Promise.
func promise(fn func() (any, error)) js.Value {
	executor := js.FuncOf(func(this js.Value, a []js.Value) any {
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
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

func main() {
	// The build (k8s/snapshot.mjs) saves the heap at this point, after the
	// init of all packages, and the page starts from that. The sleep hands
	// control back to JavaScript, with nothing else left to run. In the page
	// the timer of the sleep is long gone, so main goes on at once.
	if js.Global().Get("tabernetesSnapshot").Truthy() {
		time.Sleep(100 * time.Millisecond)
	}
	// Every start from the snapshot has the random seeds of the build. The
	// names that Kubernetes generates (pods of a ReplicaSet, ...) come from
	// util/rand, seeded with the clock during init: seed it again.
	utilrand.Seed(time.Now().UnixNano())
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

	// This instance boots the control plane, in the SharedWorker
	// (web/src/cluster.js) or in a tab without one. Every tab opens a session
	// on it (openTab, session.go): its shell, kubectl and its node.
	ready := promise(func() (any, error) {
		// The boot allocates a lot of short-lived garbage. Collect less
		// often while it runs: the prompt comes ~0.1-0.25 s sooner, and the
		// memory of the control plane grows ~35 MiB (260 -> 295 MiB).
		gc := debug.SetGCPercent(400)
		defer debug.SetGCPercent(gc)
		_, err := boot()
		return nil, err
	})
	api.Set("ready", ready)

	// openTab(host): Promise<session>, see session.go
	api.Set("openTab", js.FuncOf(func(this js.Value, a []js.Value) any {
		host := a[0]
		return promise(func() (any, error) {
			s, err := openTab(host)
			if err != nil {
				return nil, err
			}
			return s.api(), nil
		})
	}))
	// sessions(): the number of open sessions
	api.Set("sessions", js.FuncOf(func(js.Value, []js.Value) any { return openSessions() }))
	// dropping(): the number of nodes of closed tabs that are still being deleted
	api.Set("dropping", js.FuncOf(func(js.Value, []js.Value) any { return droppingNodes() }))

	// proxy(port, method, path, headers, body): Promise<{status, statusText, headers, body}>, see proxy.go
	api.Set("proxy", js.FuncOf(func(this js.Value, a []js.Value) any { return proxyJS(a) }))
	// tabernetes.dial(port, onData, onClose) for the VNC viewer, see dial.go.
	api.Set("dial", js.FuncOf(func(this js.Value, a []js.Value) any { return dialJS(a) }))

	// prepareJoin(open: (label: string) => Promise<RTCDataChannel>, relay?: string): Promise<{name, ip, token, ca, caHash, pairing?}>
	// With a relay URL, the node is paired (join --keep, paired.go).
	api.Set("prepareJoin", js.FuncOf(func(this js.Value, a []js.Value) any {
		open := a[0]
		relay := ""
		if len(a) > 1 && a[1].Type() == js.TypeString {
			relay = a[1].String()
		}
		return promise(func() (any, error) {
			if current == nil {
				return nil, fmt.Errorf("cluster is not ready yet")
			}
			return current.prepareJoin(open, relay)
		})
	}))
	// prepareKubectl(relay: string): Promise<{name, token, ca, pairing}> pairs
	// a kubectl outside the browser (join --kubectl, paired.go).
	api.Set("prepareKubectl", js.FuncOf(func(this js.Value, a []js.Value) any {
		relay := a[0].String()
		return promise(func() (any, error) {
			if current == nil {
				return nil, fmt.Errorf("cluster is not ready yet")
			}
			return current.prepareKubectl(context.Background(), relay)
		})
	}))
	// pairedNodes(): [{kind, name, ip, relay, topic, key}] of the paired docker nodes.
	api.Set("pairedNodes", js.FuncOf(func(this js.Value, a []js.Value) any {
		list := []any{}
		for _, p := range pairings() {
			list = append(list, p.js())
		}
		return js.ValueOf(list)
	}))
	// restoreNode(name, open) connects the kube-apiserver to the kubelet of a
	// paired node again, through open (like the one of prepareJoin).
	api.Set("restoreNode", js.FuncOf(func(this js.Value, a []js.Value) any {
		if err := restoreNode(a[0].String(), a[1]); err != nil {
			return js.Global().Get("Error").New(err.Error())
		}
		return nil
	}))
	// unjoin(name): Promise removes a paired node, its pairing and its pods.
	api.Set("unjoin", js.FuncOf(func(this js.Value, a []js.Value) any {
		name := a[0].String()
		return promise(func() (any, error) { return nil, unjoin(context.Background(), name) })
	}))
	// attachChannel(dc: RTCDataChannel) for channels opened by a node agent.
	api.Set("attachChannel", js.FuncOf(func(this js.Value, a []js.Value) any {
		attachChannel(a[0])
		return nil
	}))
	// attachPort(port: MessagePort, label: string) for the data channels that
	// a docker node opens: a tab bridges them to the shared worker (tabs.js).
	api.Set("attachPort", js.FuncOf(func(this js.Value, a []js.Value) any {
		attach(newPortConn(a[0]), a[1].String())
		return nil
	}))
	// dropNode(name: string) removes a node of a closed tab, with its
	// session, or a node from join whose connections ended (tabs.go).
	api.Set("dropNode", js.FuncOf(func(this js.Value, a []js.Value) any {
		name := a[0].String()
		go dropNode(name)
		return nil
	}))

	select {}
}
