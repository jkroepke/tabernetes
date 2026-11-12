package main

import (
	"context"
	"fmt"
	"os"

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
// The shell, kubectl and the kubelet of every tab run in the worker, too: a
// session per tab (session.go). A tab holds a Web Lock while it is open.
// When the worker gets that lock, the tab is gone, and so are its session
// and its node.

var tabClient kubernetes.Interface

// adminClient is the client of the worker for the nodes of the tabs.
func adminClient() (kubernetes.Interface, error) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
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

// dropNode removes a node of a tab that was closed: the node of the tab
// itself, or a node from join, whose WebRTC connection ended with the tab.
// The pods on the node go with it, as the pod garbage collector would do a
// minute later; a Deployment starts them on another node.
func dropNode(name string) {
	sessionsMu.Lock()
	s := sessions[name]
	droppingTabs[name] = true
	sessionsMu.Unlock()
	defer func() {
		sessionsMu.Lock()
		delete(droppingTabs, name)
		sessionsMu.Unlock()
	}()
	if s != nil {
		s.close()
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
