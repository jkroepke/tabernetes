package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall/js"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/component-base/version"
	"k8s.io/utils/ptr"
)

// The tab node: the browser tab joins the cluster as the node "tab-1", or
// tab-<n> next to other tabs (tabs.go). It has no container runtime. Its
// kubelet (this file, tabkubelet.go) runs the pods whose image names
// something that runs in the browser: webllm:<model> loads a language model
// onto the GPU with WebGPU (webllm.go). The page provides the GPU side as globalThis.tabernetesGPU (web/src/gpu.js).
//
// The node has the taint tabernetes.dev/tab, so that ordinary pods stay away,
// and kubernetes.io/os=browser, so that the DaemonSets (which select linux)
// do, too.

// The name and the address of this tab as a node. With the control plane in
// a SharedWorker (tabs.go), every tab of the page is a node: tab-1, tab-2 and
// so on, with 10.0.1.<n>. Joined nodes have 10.0.0.2 to .250 (tunnel.go).
var (
	tabNodeName = "tab-1"
	tabNodeIP   = "10.0.1.1"
	// leaseByCluster is set when the control plane renews the lease of this
	// node for as long as the tab holds its Web Lock (tabs.go). Timers of a
	// hidden tab may fire only once a minute, too late for a lease.
	leaseByCluster bool
)

const (
	tabTaint       = "tabernetes.dev/tab"
	webgpuResource = "tabernetes.dev/webgpu"
	// The kubelet reports the node status every 10 s. The lease is the
	// heartbeat the node lifecycle controller looks at.
	tabHeartbeat     = 10 * time.Second
	tabLeaseDuration = 40
)

// gpuInfo is what the page knows about the GPU (gpu.js adapter()).
type gpuInfo struct {
	description, vendor, architecture string
	f16                               bool
}

// gpuAdapter asks the page for a WebGPU adapter. It returns nil without one.
func gpuAdapter(ctx context.Context) (*gpuInfo, error) {
	gpu := js.Global().Get("tabernetesGPU")
	if gpu.Type() != js.TypeObject {
		return nil, errors.New("this page has no GPU support")
	}
	v, err := awaitPromise(ctx, gpu.Call("adapter"))
	if err != nil || v.Type() != js.TypeObject {
		return nil, err
	}
	return &gpuInfo{
		description:  v.Get("description").String(),
		vendor:       v.Get("vendor").String(),
		architecture: v.Get("architecture").String(),
		f16:          v.Get("f16").Truthy(),
	}, nil
}

func (g *gpuInfo) String() string {
	for _, s := range []string{g.description, strings.TrimSpace(g.vendor + " " + g.architecture)} {
		if s != "" {
			return s
		}
	}
	return "unknown GPU"
}

type tabNode struct {
	cs      kubernetes.Interface
	gpu     *gpuInfo
	rec     record.EventRecorder
	ca      *pki
	podCIDR *net.IPNet

	mu     sync.Mutex
	pods   map[types.UID]*tabPod
	nextIP int
}

var (
	tabOnce sync.Once
	tab     *tabNode
	tabErr  error
)

// startTabNode registers the tab as a node, once.
func startTabNode(ctx context.Context) (*tabNode, error) {
	tabOnce.Do(func() { tab, tabErr = newTabNode(ctx) })
	if tabErr != nil {
		// let the next call try again, for example after a reload of the GPU
		err := tabErr
		tabOnce = sync.Once{}
		return nil, err
	}
	return tab, nil
}

func newTabNode(ctx context.Context) (*tabNode, error) {
	if current == nil {
		return nil, errors.New("the cluster is not ready yet")
	}
	// Without WebGPU, the tab is a node without the resource
	// tabernetes.dev/webgpu: it runs nothing, but it is there.
	gpu, err := gpuAdapter(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tab node: no GPU:", err)
		gpu = nil
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", p("admin.kubeconfig"))
	if err != nil {
		return nil, err
	}
	cfg.UserAgent = "kubelet/" + version.Get().GitVersion + " (tab)"
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	b := record.NewBroadcaster(record.WithContext(context.Background()))
	b.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: cs.CoreV1().Events("")})
	n := &tabNode{
		cs:   cs,
		gpu:  gpu,
		rec:  b.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "kubelet", Host: tabNodeName}),
		ca:   current.ca,
		pods: map[types.UID]*tabPod{},
	}
	if err := n.serveKubelet(); err != nil {
		return nil, err
	}
	if err := n.register(ctx); err != nil {
		return nil, err
	}
	go n.heartbeat()
	n.watchPods()
	return n, nil
}

func (n *tabNode) node() *corev1.Node {
	cpus := js.Global().Get("navigator").Get("hardwareConcurrency").Int()
	mem := 4
	if m := js.Global().Get("navigator").Get("deviceMemory"); m.Type() == js.TypeNumber && m.Int() > 0 {
		mem = m.Int() // Chrome rounds and caps at 8
	}
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(int64(max(cpus, 1)), resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(int64(mem)<<30, resource.BinarySI),
		corev1.ResourcePods:   *resource.NewQuantity(8, resource.DecimalSI),
		webgpuResource:        *resource.NewQuantity(0, resource.DecimalSI),
	}
	runtime := "webgpu://none"
	if n.gpu != nil {
		capacity[webgpuResource] = *resource.NewQuantity(1, resource.DecimalSI)
		runtime = "webgpu://" + n.gpu.String()
	}
	now := metav1.Now()
	cond := func(t corev1.NodeConditionType, s corev1.ConditionStatus, reason, msg string) corev1.NodeCondition {
		return corev1.NodeCondition{Type: t, Status: s, Reason: reason, Message: msg, LastHeartbeatTime: now, LastTransitionTime: now}
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: tabNodeName,
			Labels: map[string]string{
				"kubernetes.io/hostname":           tabNodeName,
				"kubernetes.io/os":                 "browser",
				"kubernetes.io/arch":               "wasm",
				"node.kubernetes.io/instance-type": "browser-tab",
				webgpuResource:                     strconv.FormatBool(n.gpu != nil),
			},
		},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{{Key: tabTaint, Value: "browser", Effect: corev1.TaintEffectNoSchedule}},
		},
		Status: corev1.NodeStatus{
			Capacity:    capacity,
			Allocatable: capacity,
			Conditions: []corev1.NodeCondition{
				cond(corev1.NodeMemoryPressure, corev1.ConditionFalse, "KubeletHasSufficientMemory", "kubelet has sufficient memory available"),
				cond(corev1.NodeDiskPressure, corev1.ConditionFalse, "KubeletHasNoDiskPressure", "kubelet has no disk pressure"),
				cond(corev1.NodePIDPressure, corev1.ConditionFalse, "KubeletHasSufficientPID", "kubelet has sufficient PID available"),
				cond(corev1.NodeReady, corev1.ConditionTrue, "KubeletReady", "the tab is open"),
			},
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: tabNodeIP},
				{Type: corev1.NodeHostName, Address: tabNodeName},
			},
			DaemonEndpoints: corev1.NodeDaemonEndpoints{KubeletEndpoint: corev1.DaemonEndpoint{Port: 10250}},
			NodeInfo: corev1.NodeSystemInfo{
				MachineID:               randToken(32),
				SystemUUID:              randToken(32),
				BootID:                  randToken(32),
				KernelVersion:           "-",
				OSImage:                 browserName(),
				ContainerRuntimeVersion: runtime,
				KubeletVersion:          version.Get().GitVersion,
				OperatingSystem:         "browser",
				Architecture:            "wasm",
			},
		},
	}
}

// browserName is "Google Chrome 142", or the user agent where the browser
// does not tell its brands.
func browserName() string {
	nav := js.Global().Get("navigator")
	if d := nav.Get("userAgentData"); d.Type() == js.TypeObject {
		brands := d.Get("brands")
		for i := range brands.Length() {
			b := brands.Index(i)
			name := b.Get("brand").String()
			if !strings.Contains(name, "Brand") && name != "Chromium" {
				return name + " " + b.Get("version").String()
			}
		}
	}
	return nav.Get("userAgent").String()
}

func (n *tabNode) register(ctx context.Context) error {
	want := n.node()
	got, err := n.cs.CoreV1().Nodes().Create(ctx, want, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		got, err = n.cs.CoreV1().Nodes().Get(ctx, tabNodeName, metav1.GetOptions{})
	}
	if err != nil {
		return fmt.Errorf("registering node %s: %w", tabNodeName, err)
	}
	if err := n.updateStatus(ctx); err != nil {
		return err
	}
	// The node IPAM controller gives the node its pod CIDR right after the
	// registration.
	for i := 0; got.Spec.PodCIDR == ""; i++ {
		if i == 100 {
			return errors.New("the node got no pod CIDR")
		}
		time.Sleep(100 * time.Millisecond)
		if got, err = n.cs.CoreV1().Nodes().Get(ctx, tabNodeName, metav1.GetOptions{}); err != nil {
			return err
		}
	}
	_, n.podCIDR, err = net.ParseCIDR(got.Spec.PodCIDR)
	return err
}

func (n *tabNode) updateStatus(ctx context.Context) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := n.cs.CoreV1().Nodes().Get(ctx, tabNodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		fresh := n.node().Status
		fresh.NodeInfo = node.Status.NodeInfo
		if fresh.NodeInfo.MachineID == "" {
			fresh.NodeInfo = n.node().Status.NodeInfo
		}
		// keep the transition times of conditions that did not change
		for i, c := range fresh.Conditions {
			for _, old := range node.Status.Conditions {
				if old.Type == c.Type && old.Status == c.Status {
					fresh.Conditions[i].LastTransitionTime = old.LastTransitionTime
				}
			}
		}
		node.Status = fresh
		_, err = n.cs.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
		return err
	})
}

// heartbeat renews the node lease and the status for as long as the tab is
// open. A hidden tab gets fewer timers, the whole cluster slows down with it.
func (n *tabNode) heartbeat() {
	ctx := context.Background()
	for i := 0; ; i++ {
		if !leaseByCluster {
			if err := renewLease(ctx, n.cs, tabNodeName); err != nil {
				fmt.Fprintln(os.Stderr, "tab node: lease:", err)
			}
		}
		if i%6 == 5 {
			if err := n.updateStatus(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "tab node: status:", err)
			}
		}
		time.Sleep(tabHeartbeat)
	}
}

// renewLease renews the lease of a tab node, or creates it.
func renewLease(ctx context.Context, cs kubernetes.Interface, name string) error {
	leases := cs.CoordinationV1().Leases(corev1.NamespaceNodeLease)
	now := metav1.NewMicroTime(time.Now())
	l, err := leases.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		node, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		_, err = leases.Create(ctx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       corev1.NamespaceNodeLease,
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: name, UID: node.UID}},
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       ptr.To(name),
				LeaseDurationSeconds: ptr.To(int32(tabLeaseDuration)),
				RenewTime:            &now,
			},
		}, metav1.CreateOptions{})
		return err
	case err != nil:
		return err
	}
	l.Spec.RenewTime = &now
	_, err = leases.Update(ctx, l, metav1.UpdateOptions{})
	return err
}

// watchPods runs the pods the scheduler binds to the tab.
func (n *tabNode) watchPods() {
	f := informers.NewSharedInformerFactoryWithOptions(n.cs, 0, informers.WithTweakListOptions(func(o *metav1.ListOptions) {
		o.FieldSelector = "spec.nodeName=" + tabNodeName
	}))
	inf := f.Core().V1().Pods().Informer()
	inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { n.sync(o.(*corev1.Pod)) },
		UpdateFunc: func(_, o any) { n.sync(o.(*corev1.Pod)) },
		DeleteFunc: func(o any) {
			if d, ok := o.(cache.DeletedFinalStateUnknown); ok {
				o = d.Obj
			}
			if pod, ok := o.(*corev1.Pod); ok {
				n.forget(pod.UID)
			}
		},
	})
	f.Start(make(chan struct{}))
}

func (n *tabNode) sync(pod *corev1.Pod) {
	n.mu.Lock()
	tp := n.pods[pod.UID]
	if tp == nil && pod.DeletionTimestamp == nil && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		tp = n.newPod(pod)
		n.pods[pod.UID] = tp
		go tp.run()
	}
	n.mu.Unlock()
	if pod.DeletionTimestamp != nil {
		go n.terminate(pod, tp)
	}
}

// terminate stops the pod and removes it, as the kubelet does once the
// containers are gone.
func (n *tabNode) terminate(pod *corev1.Pod, tp *tabPod) {
	if tp != nil {
		tp.stop("Killing", "Stopping container "+tp.container.Name)
	}
	err := n.cs.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: ptr.To(int64(0)),
		Preconditions:      &metav1.Preconditions{UID: &pod.UID},
	})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		fmt.Fprintln(os.Stderr, "tab node: deleting pod:", err)
	}
}

func (n *tabNode) forget(uid types.UID) {
	n.mu.Lock()
	tp := n.pods[uid]
	delete(n.pods, uid)
	n.mu.Unlock()
	if tp != nil {
		tp.stop("", "")
	}
}

func (n *tabNode) podByName(namespace, name string) *tabPod {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, tp := range n.pods {
		if tp.namespace == namespace && tp.name == name {
			return tp
		}
	}
	return nil
}

// allocateIP hands out the next address of the pod CIDR. n.mu is held.
func (n *tabNode) allocateIP() string {
	ip := n.podCIDR.IP.To4()
	n.nextIP++
	return net.IPv4(ip[0], ip[1], ip[2], byte(2+(n.nextIP-1)%250)).String()
}

// logBuffer keeps the output of a container for kubectl logs. Line numbers
// count from the start, also after old lines were dropped.
type logBuffer struct {
	mu      sync.Mutex
	lines   []logLine
	dropped int
	part    []byte
	wake    chan struct{}
}

type logLine struct {
	t    time.Time
	text string
}

func newLogBuffer() *logBuffer { return &logBuffer{wake: make(chan struct{})} }

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.part = append(b.part, p...)
	for {
		i := bytes.IndexByte(b.part, '\n')
		if i < 0 {
			break
		}
		b.lines = append(b.lines, logLine{time.Now(), string(b.part[:i])})
		b.part = b.part[i+1:]
	}
	if len(b.lines) > 5000 {
		b.dropped += len(b.lines) - 4000
		b.lines = append([]logLine(nil), b.lines[len(b.lines)-4000:]...)
	}
	close(b.wake)
	b.wake = make(chan struct{})
	return len(p), nil
}

// since returns the lines from number i on, the number after the last one,
// and a channel that is closed when more arrive.
func (b *logBuffer) since(i int) ([]logLine, int, <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	j := min(max(i-b.dropped, 0), len(b.lines))
	return append([]logLine(nil), b.lines[j:]...), b.dropped + len(b.lines), b.wake
}

var _ io.Writer = (*logBuffer)(nil)
