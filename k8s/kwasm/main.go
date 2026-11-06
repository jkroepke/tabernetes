// kwasm runs a Kubernetes control plane (kube-apiserver, kube-controller-manager,
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
	"strings"
	"syscall/js"
	"time"

	"github.com/spf13/cobra"

	logsapi "k8s.io/component-base/logs/api/v1"

	"k8s.io/kubernetes/cmd/kube-apiserver/app"
	cmapp "k8s.io/kubernetes/cmd/kube-controller-manager/app"
	schedapp "k8s.io/kubernetes/cmd/kube-scheduler/app"
	"k8s.io/kubernetes/cmd/kwasm/etcdshim"
)

const (
	dataDir      = "/tmp/kwasm"
	advertiseIP  = "10.0.0.1"
	serviceCIDR  = "10.96.0.0/16"
	clusterCIDR  = "10.244.0.0/16"
	kubernetesIP = "10.96.0.1"
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "kwasm:", err)
		panic(err)
	}
}

var status = func(string) {}

func run(name string, cmd *cobra.Command, args []string) {
	cmd.SetArgs(args)
	go func() {
		if err := cmd.Execute(); err != nil {
			status(name + " failed: " + err.Error())
			fmt.Fprintln(os.Stderr, name, "exited:", err)
		}
	}()
}

type cluster struct {
	ca         *pki
	adminToken string
}

func boot() *cluster {
	// All components share one process and therefore one klog instance.
	logsapi.ReapplyHandling = logsapi.ReapplyHandlingIgnoreUnchanged
	must(os.MkdirAll(p("certs"), 0o755))
	must(os.MkdirAll("/root", 0o755))
	os.Setenv("HOME", "/root")

	status("generating PKI")
	ca := newPKI()
	ca.issue("apiserver", "kube-apiserver", nil, true,
		[]string{"localhost", "kubernetes", "kubernetes.default", "kubernetes.default.svc", "kubernetes.default.svc.cluster.local"},
		[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP(advertiseIP), net.ParseIP(kubernetesIP)})
	ca.issue("apiserver-kubelet-client", "kube-apiserver-kubelet-client", []string{"system:masters"}, false, nil, nil)
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
	must(os.MkdirAll("/root/.kube", 0o755))
	cfg, err := os.ReadFile(p("admin.kubeconfig"))
	must(err)
	must(os.WriteFile("/root/.kube/config", cfg, 0o600))

	status("starting etcd")
	l, err := net.Listen("tcp", "127.0.0.1:2379")
	must(err)
	go func() { must(etcdshim.Serve(l)) }()

	status("starting kube-apiserver")
	run("kube-apiserver", app.NewAPIServerCommand(), []string{
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
		"--service-account-issuer=https://kubernetes.default.svc.cluster.local",
		"--service-account-key-file=" + p("sa.pub"),
		"--service-account-signing-key-file=" + p("sa.key"),
		"--token-auth-file=" + p("tokens.csv"),
		"--authorization-mode=Node,RBAC",
		"--enable-bootstrap-token-auth=true",
		"--allow-privileged=true",
		"--service-cluster-ip-range=" + serviceCIDR,
		"--profiling=false",
		"--v=1",
	})
	waitReady(c, "https://127.0.0.1:6443/readyz")

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

	status("installing addons")
	must(installAddons(context.Background(), c))
	status("ready")
	return c
}

func waitReady(c *cluster, url string) {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(c.ca.caPEM)
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool},
		DialContext:     (&net.Dialer{}).DialContext, // force Go networking instead of fetch()
	}}
	for {
		req, _ := http.NewRequest(http.MethodGet, url+"?verbose", nil)
		req.Header.Set("Authorization", "Bearer "+c.adminToken)
		resp, err := hc.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			var failed []string
			for _, l := range strings.Split(string(body), "\n") {
				if strings.HasPrefix(l, "[-]") {
					failed = append(failed, l)
				}
			}
			err = fmt.Errorf("status %d: %s", resp.StatusCode, strings.Join(failed, ", "))
		}
		time.Sleep(500 * time.Millisecond)
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
	api := js.Global().Get("Object").New()
	js.Global().Set("kwasm", api)
	if cb := js.Global().Get("kwasmStatus"); cb.Type() == js.TypeFunction {
		status = func(s string) { cb.Invoke(s) }
	}

	var c *cluster
	ready := promise(func() (any, error) {
		c = boot()
		return nil, nil
	})
	api.Set("ready", ready)

	// kubectl(args: string[], write: (s: string) => void, stdin?: string): Promise<number>
	api.Set("kubectl", js.FuncOf(func(this js.Value, a []js.Value) any {
		args := make([]string, a[0].Length())
		for i := range args {
			args[i] = a[0].Index(i).String()
		}
		out := a[1]
		stdin := ""
		if len(a) > 2 && a[2].Type() == js.TypeString {
			stdin = a[2].String()
		}
		return promise(func() (any, error) { return runKubectl(args, stdin, out), nil })
	}))
	api.Set("cancel", js.FuncOf(func(js.Value, []js.Value) any { cancelKubectl(); return nil }))

	// prepareJoin(open: (label: string) => Promise<RTCDataChannel>): Promise<{name, ip, token, ca, caHash}>
	api.Set("prepareJoin", js.FuncOf(func(this js.Value, a []js.Value) any {
		open := a[0]
		return promise(func() (any, error) {
			if c == nil {
				return nil, fmt.Errorf("cluster is not ready yet")
			}
			return c.prepareJoin(open)
		})
	}))
	// attachChannel(dc: RTCDataChannel) for channels opened by a node agent.
	api.Set("attachChannel", js.FuncOf(func(this js.Value, a []js.Value) any {
		attachChannel(a[0])
		return nil
	}))

	select {}
}
