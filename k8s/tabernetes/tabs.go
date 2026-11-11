package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall/js"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
)

// Tabernetes in several tabs. Where the browser has a SharedWorker, the
// control plane runs in it (web/src/cluster.js), not in a tab. The worker
// lives as long as one tab of the page is open, so any tab may close.
//
// Every tab runs this binary, too, for its shell, kubectl and its kubelet:
// the tab is the node tab-<n> (tabnode.go). A MessagePort per connection
// links tab and worker: "api" from the tab to the kube-apiserver, "kubelet"
// from the kube-apiserver to the tab. A tab holds a Web Lock while it is
// open. When the worker gets that lock, the tab is gone, and so is its node.

// role is "tab" in a tab with the control plane in a SharedWorker, and
// empty where the tab runs the control plane itself.
var role string

type tabMember struct {
	node *joinedNode
	stop chan struct{}
}

var (
	tabsMu     sync.Mutex
	tabMembers = map[string]*tabMember{}
	nextTab    = 1
	tabClient  kubernetes.Interface
)

// prepareTab adds a tab. open(label) opens a connection to it and returns
// a Promise of a MessagePort. The result tells the tab its name and address
// and has what its kubectl and kubelet need.
func (c *cluster) prepareTab(open js.Value) (map[string]any, error) {
	if _, err := adminClient(); err != nil {
		return nil, err
	}
	tabsMu.Lock()
	n := nextTab
	nextTab++
	tabsMu.Unlock()
	if n > 250 {
		return nil, errors.New("too many tabs")
	}
	name, ip := fmt.Sprintf("tab-%d", n), fmt.Sprintf("10.0.1.%d", n)
	files := map[string]any{}
	for _, f := range []string{"ca.crt", "ca.key", "admin.kubeconfig"} {
		b, err := os.ReadFile(p(f))
		if err != nil {
			return nil, err
		}
		files[f] = string(b)
	}
	ln, err := net.Listen("tcp", ip+":10250")
	if err != nil {
		return nil, err
	}
	m := &tabMember{node: &joinedNode{name: name, ip: ip, open: open, ln: ln}, stop: make(chan struct{})}
	tabsMu.Lock()
	tabMembers[name] = m
	tabsMu.Unlock()
	go m.node.serve()
	go m.renew(name)
	return map[string]any{"name": name, "ip": ip, "token": c.adminToken, "files": files}, nil
}

// adminClient is the client of the worker for the nodes of the tabs.
func adminClient() (kubernetes.Interface, error) {
	tabsMu.Lock()
	defer tabsMu.Unlock()
	if tabClient != nil {
		return tabClient, nil
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", p("admin.kubeconfig"))
	if err != nil {
		return nil, err
	}
	cfg.UserAgent = "tabernetes/tabs"
	tabClient, err = kubernetes.NewForConfig(cfg)
	return tabClient, err
}

// renew renews the lease of the node of a tab for as long as the tab is open.
func (m *tabMember) renew(name string) {
	for {
		select {
		case <-m.stop:
			return
		case <-time.After(tabHeartbeat):
		}
		// Not found: the tab has not registered its node yet, or it has no GPU.
		if err := renewLease(context.Background(), tabClient, name); err != nil && !apierrors.IsNotFound(err) {
			fmt.Fprintln(os.Stderr, "tabs: lease of", name+":", err)
		}
	}
}

// dropNode removes a node of a tab that was closed: the node of the tab
// itself, or a node from join, whose WebRTC connection ended with the tab.
// The pods on the node go with it, as the pod garbage collector would do a
// minute later; a Deployment starts them on another node.
func dropNode(name string) {
	tabsMu.Lock()
	m := tabMembers[name]
	delete(tabMembers, name)
	tabsMu.Unlock()
	if m != nil {
		close(m.stop)
		m.node.ln.Close()
	}
	nodesMu.Lock()
	jn := nodes[name]
	delete(nodes, name)
	nodesMu.Unlock()
	if jn != nil {
		jn.ln.Close()
	}
	cs, err := adminClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tabs:", err)
		return
	}
	ctx := context.Background()
	err = cs.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		fmt.Fprintln(os.Stderr, "tabs: deleting node", name+":", err)
	}
	pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + name})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tabs: pods of", name+":", err)
		return
	}
	for _, pod := range pods.Items {
		err := cs.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To(int64(0))})
		if err != nil && !apierrors.IsNotFound(err) {
			fmt.Fprintln(os.Stderr, "tabs: deleting pod", pod.Name+":", err)
		}
	}
}

// follow sets a tab up with the control plane in the SharedWorker:
// tabernetesCluster is {info, open(label)} from web/src/cluster.js.
func follow(cfg js.Value) (*cluster, error) {
	info, open := cfg.Get("info"), cfg.Get("open")
	tabNodeName, tabNodeIP = info.Get("name").String(), info.Get("ip").String()
	leaseByCluster = true
	status("joining the control plane in the shared worker as " + tabNodeName)
	must(os.MkdirAll(dataDir, 0o755))
	must(os.MkdirAll("/root/.kube", 0o755))
	os.Setenv("HOME", "/root")
	files := info.Get("files")
	for _, f := range []string{"ca.crt", "ca.key", "admin.kubeconfig"} {
		if err := os.WriteFile(p(f), []byte(files.Get(f).String()), 0o600); err != nil {
			return nil, err
		}
	}
	cfgFile, err := os.ReadFile(p("admin.kubeconfig"))
	must(err)
	must(os.WriteFile("/root/.kube/config", cfgFile, 0o600))
	ca, err := loadPKI(p("ca.crt"), p("ca.key"))
	if err != nil {
		return nil, err
	}

	// The kube-apiserver of this tab is a tunnel to the worker.
	l, err := net.Listen("tcp", "127.0.0.1:6443")
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			tcp, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				port, err := awaitPromise(ctx, open.Invoke("api"))
				if err != nil {
					fmt.Fprintln(os.Stderr, "tabernetes: opening the api connection:", err)
					tcp.Close()
					return
				}
				splice(newJSConn(port), tcp)
			}()
		}
	}()
	c := &cluster{ca: ca, adminToken: info.Get("token").String()}
	if err := waitReady(c, "https://127.0.0.1:6443/readyz", nil); err != nil {
		return nil, fmt.Errorf("the control plane does not answer: %w", err)
	}
	current = c

	go registerTab()
	return c, nil
}

// registerTab makes this tab a node. A tab without WebGPU is a node too,
// without tabernetes.dev/webgpu; a failure costs the tab only its node.
// It runs in the background: the shell and kubectl do not need the node,
// and on a new cluster the node waits seconds for its pod CIDR.
func registerTab() {
	status("registering this tab as the node " + tabNodeName)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := startTabNode(ctx); err != nil {
		status("this tab is no node: " + err.Error())
	}
}
