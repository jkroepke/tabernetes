package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// The terminal is not in the pod network: the shell runs in the wasm, the
// pods run on the tab nodes and on nodes from join. wget 10.244.0.2 still
// reaches a pod, through the port-forward of the apiserver, as kubectl
// port-forward does, only without a local port. A ClusterIP of a Service
// goes to one of its ready endpoints.

var (
	podNetOnce sync.Once
	podNetCfg  *rest.Config
	podNetCS   *kubernetes.Clientset
	podNetErr  error
)

func podNetClient() (*rest.Config, *kubernetes.Clientset, error) {
	podNetOnce.Do(func() {
		podNetCfg, podNetErr = clientcmd.BuildConfigFromFlags("", p("admin.kubeconfig"))
		if podNetErr != nil {
			return
		}
		podNetCfg.UserAgent = "tabernetes/wget"
		podNetCS, podNetErr = kubernetes.NewForConfig(podNetCfg)
	})
	return podNetCfg, podNetCS, podNetErr
}

// inClusterNet tells whether ip is a pod or a Service address of the cluster.
func inClusterNet(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, cidr := range []string{clusterCIDR, serviceCIDR} {
		if _, n, err := net.ParseCIDR(cidr); err == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// dialCluster connects to addr, a pod IP or a ClusterIP with a port.
func dialCluster(ctx context.Context, _ string, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	cfg, cs, err := podNetClient()
	if err != nil {
		return nil, err
	}
	pod, port, err := findPod(ctx, cs, host, port)
	if err != nil {
		return nil, fmt.Errorf("dial tcp %s: %w", addr, err)
	}
	return dialPod(cfg, pod, port)
}

// findPod finds the pod behind ip: the pod itself, or a ready endpoint of the
// Service with that ClusterIP, with the target port of the Service port.
func findPod(ctx context.Context, cs *kubernetes.Clientset, ip string, port int) (*corev1.Pod, int, error) {
	if _, n, _ := net.ParseCIDR(serviceCIDR); n.Contains(net.ParseIP(ip)) {
		svcs, err := cs.CoreV1().Services("").List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, 0, err
		}
		for _, svc := range svcs.Items {
			if svc.Spec.ClusterIP != ip {
				continue
			}
			var sp *corev1.ServicePort
			for i := range svc.Spec.Ports {
				if int(svc.Spec.Ports[i].Port) == port {
					sp = &svc.Spec.Ports[i]
				}
			}
			if sp == nil {
				return nil, 0, errors.New("connect: connection refused (the Service has no such port)")
			}
			eps, err := cs.DiscoveryV1().EndpointSlices(svc.Namespace).List(ctx, metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=" + svc.Name})
			if err != nil {
				return nil, 0, err
			}
			for _, s := range eps.Items {
				target := 0
				for _, p := range s.Ports {
					if p.Port != nil && ptrOr(p.Name, "") == sp.Name {
						target = int(*p.Port)
					}
				}
				for _, e := range s.Endpoints {
					if target == 0 || e.TargetRef == nil || e.TargetRef.Kind != "Pod" || !ptrOr(e.Conditions.Ready, true) {
						continue
					}
					pod, err := cs.CoreV1().Pods(e.TargetRef.Namespace).Get(ctx, e.TargetRef.Name, metav1.GetOptions{})
					if err == nil {
						return pod, target, nil
					}
				}
			}
			return nil, 0, errors.New("connect: connection refused (the Service has no ready pod)")
		}
		return nil, 0, errors.New("connect: no route to host (no Service has this ClusterIP)")
	}
	pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "status.podIP=" + ip})
	if err != nil {
		return nil, 0, err
	}
	for i := range pods.Items {
		if p := &pods.Items[i]; p.Status.Phase == corev1.PodRunning && !p.Spec.HostNetwork {
			return p, port, nil
		}
	}
	return nil, 0, errors.New("connect: no route to host (no running pod has this IP)")
}

func ptrOr[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}

// dialPod opens one port-forward stream to port of pod.
func dialPod(cfg *rest.Config, pod *corev1.Pod, port int) (net.Conn, error) {
	u, err := url.Parse(cfg.Host)
	if err != nil {
		return nil, err
	}
	u.Path = fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", pod.Namespace, pod.Name)
	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return nil, err
	}
	// as kubectl port-forward does: SPDY over a WebSocket, else SPDY
	var dialer httpstream.Dialer = spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, u)
	if ws, err := portforward.NewSPDYOverWebsocketDialer(u, cfg); err == nil {
		dialer = portforward.NewFallbackDialer(ws, dialer, func(err error) bool {
			return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
		})
	}
	sc, _, err := dialer.Dial(portforward.PortForwardProtocolV1Name)
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	headers.Set(corev1.PortHeader, strconv.Itoa(port))
	headers.Set(corev1.PortForwardRequestIDHeader, "0")
	errStream, err := sc.CreateStream(headers)
	if err != nil {
		sc.Close()
		return nil, err
	}
	errStream.Close()
	headers.Set(corev1.StreamType, corev1.StreamTypeData)
	data, err := sc.CreateStream(headers)
	if err != nil {
		sc.Close()
		return nil, err
	}
	c := &podConn{sc: sc, data: data, addr: &net.TCPAddr{IP: net.ParseIP(pod.Status.PodIP), Port: port}, errDone: make(chan struct{})}
	// The kubelet reports a failed connect (nothing listens) on the error
	// stream, and closes the data stream, often before that.
	go func() {
		defer close(c.errDone)
		msg, _ := io.ReadAll(errStream)
		if m := strings.TrimSpace(string(msg)); m != "" {
			// "error forwarding port 80 to pod ...: dial tcp ...": the
			// dial error is what a direct connection would say
			if i := strings.LastIndex(m, "dial tcp "); i >= 0 {
				m = m[i:]
			}
			c.mu.Lock()
			c.remoteErr = errors.New(m)
			c.mu.Unlock()
			data.Reset()
		}
	}()
	return c, nil
}

// podConn is a net.Conn over the data stream of a port-forward.
type podConn struct {
	sc        httpstream.Connection
	data      httpstream.Stream
	addr      net.Addr
	mu        sync.Mutex
	remoteErr error
	errDone   chan struct{}
	closeOnce sync.Once
}

func (c *podConn) Read(b []byte) (int, error) {
	n, err := c.data.Read(b)
	if err != nil {
		select {
		case <-c.errDone:
		case <-time.After(2 * time.Second):
		}
		c.mu.Lock()
		if c.remoteErr != nil {
			err = c.remoteErr
		}
		c.mu.Unlock()
	}
	return n, err
}

func (c *podConn) Write(b []byte) (int, error) { return c.data.Write(b) }

func (c *podConn) Close() error {
	c.closeOnce.Do(func() {
		c.data.Close()
		c.sc.Close()
	})
	return nil
}

func (c *podConn) LocalAddr() net.Addr                { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (c *podConn) RemoteAddr() net.Addr               { return c.addr }
func (c *podConn) SetDeadline(t time.Time) error      { return nil }
func (c *podConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *podConn) SetWriteDeadline(t time.Time) error { return nil }
