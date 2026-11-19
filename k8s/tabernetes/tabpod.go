package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
)

// A pod on the tab node. A container image runs in v86 (v86.go): the VM is
// the sandbox of the pod, and its init starts the containers on command, as
// the kubelet asks containerd: the init containers one after the other (a
// sidecar, an init container with restartPolicy Always, keeps running), then
// the containers, and later the ephemeral containers of kubectl debug. The
// kubelet side here pulls the strings: restarts with back-off, probes,
// statuses and events, with the messages of the kubelet.
//
// The language model of webllm.go is a workload of its own: a pod with one
// container.

// workload is what runs in place of a container that is no image: the
// language model of webllm.go.
type workload interface {
	// start loads the workload and returns once it runs. It reports the
	// download to pull, and writes to log (kubectl logs) once that is done.
	start(ctx context.Context, log io.Writer, pull *imagePull) error
	// exec runs a command of the workload (kubectl exec).
	exec(ctx context.Context, cmd []string, in io.Reader, out, errOut io.Writer) error
	dialer
	stop()
	// imageID tells kubectl describe what runs.
	imageID() string
	// runtime names what runs it, for the container ID.
	runtime() string
}

// dialer is what runs a pod: port-forward and the cluster network connect
// to its ports.
type dialer interface {
	// dial connects to a port of the pod (kubectl port-forward).
	dial(port int32) (net.Conn, error)
}

// startError is a container that did not start, as runc reports it.
type startError struct {
	code int
	msg  string
}

func (e *startError) Error() string { return e.msg }

const (
	backoffStart = 10 * time.Second
	backoffMax   = 5 * time.Minute
)

type ctrKind int

const (
	ctrInit ctrKind = iota
	ctrRegular
	ctrEphemeral
)

// tabCtr is a container of a tab pod.
type tabCtr struct {
	tp      *tabPod
	spec    corev1.Container
	kind    ctrKind
	sidecar bool // an init container with restartPolicy Always
	index   int  // in the VM (init.c), -1 before it is there
	ref     *corev1.ObjectReference

	// under tp.mu
	status    corev1.ContainerStatus
	logs      *logBuffer
	prev      *logBuffer // of the run before: kubectl logs --previous
	backoff   time.Duration
	run       int // counts the runs; the probes of an old run are ignored
	runAt     time.Time
	probes    context.CancelFunc
	startupOK bool
	ready     bool
	done      bool // terminated for good
	pull      *imagePull
	pending   bool // a start is under way
}

// tabPod is a pod on the tab node.
type tabPod struct {
	n               *tabNode
	uid             types.UID
	namespace, name string
	restartPolicy   corev1.RestartPolicy
	ref             *corev1.ObjectReference
	ip              string
	hostname        string
	pod             *corev1.Pod     // as it was bound, for env, volumes and the downward API
	spec            *corev1.PodSpec // for DNS and /etc/hosts (clusternet.go)
	ctrs            []*tabCtr       // init containers, containers, ephemeral containers
	ignored         []string        // what the pod asks for and the tab node does not have
	ctx             context.Context
	cancel          context.CancelFunc

	mu      sync.Mutex
	w       dialer // what runs the pod, once a container started
	phase   corev1.PodPhase
	reason  string // of the pod, when the tab cannot run it at all
	message string
	started metav1.Time
	push    sync.Mutex // one status update at a time

	// events of the VM and the probes for the loop of the pod (runVM)
	evMu   sync.Mutex
	evs    []podEvent
	notify chan struct{}
}

func (n *tabNode) newPod(pod *corev1.Pod) *tabPod {
	ctx, cancel := context.WithCancel(context.Background())
	tp := &tabPod{
		n:             n,
		uid:           pod.UID,
		namespace:     pod.Namespace,
		name:          pod.Name,
		restartPolicy: pod.Spec.RestartPolicy,
		ref:           &corev1.ObjectReference{Kind: "Pod", APIVersion: "v1", Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID},
		ip:            n.allocateIP(),
		hostname:      pod.Name,
		pod:           pod.DeepCopy(),
		spec:          pod.Spec.DeepCopy(),
		ctx:           ctx,
		cancel:        cancel,
		phase:         corev1.PodPending,
		started:       metav1.Now(),
		notify:        make(chan struct{}, 1),
	}
	if pod.Spec.HostNetwork {
		tp.ip = n.ip
	}
	if pod.Spec.Hostname != "" {
		tp.hostname = pod.Spec.Hostname
	}
	for _, c := range pod.Spec.InitContainers {
		tc := tp.addCtr(c, ctrInit)
		tc.sidecar = c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
	}
	for _, c := range pod.Spec.Containers {
		tp.addCtr(c, ctrRegular)
	}
	for _, e := range pod.Spec.EphemeralContainers {
		tp.addCtr(corev1.Container(e.EphemeralContainerCommon), ctrEphemeral)
	}
	webllm := false
	for _, c := range tp.ctrs {
		if _, ok := webllmModel(c.spec.Image); ok {
			webllm = true
		}
	}
	if webllm && (len(pod.Spec.Containers) != 1 || len(tp.ctrs) != 1) {
		tp.phase, tp.reason, tp.message = corev1.PodFailed, "UnsupportedPod", "a pod with the language model of "+webllmRepo+" has exactly one container"
	}
	tp.ignored = unsupportedVolumes(pod)
	return tp
}

func (tp *tabPod) addCtr(c corev1.Container, kind ctrKind) *tabCtr {
	field := map[ctrKind]string{ctrInit: "spec.initContainers", ctrRegular: "spec.containers", ctrEphemeral: "spec.ephemeralContainers"}[kind]
	ref := *tp.ref
	ref.FieldPath = field + "{" + c.Name + "}"
	tc := &tabCtr{tp: tp, spec: c, kind: kind, index: -1, ref: &ref, logs: newLogBuffer()}
	waiting := "ContainerCreating"
	if kind == ctrRegular && len(tp.spec.InitContainers) > 0 || kind == ctrInit {
		waiting = "PodInitializing"
	}
	tc.status = corev1.ContainerStatus{
		Name:    c.Name,
		Image:   c.Image,
		State:   corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waiting}},
		Started: ptr.To(false),
	}
	tp.ctrs = append(tp.ctrs, tc)
	return tc
}

// container finds a container by name; "" is the first container.
func (tp *tabPod) container(name string) *tabCtr {
	for _, c := range tp.ctrs {
		if c.kind == ctrRegular && name == "" || c.spec.Name == name {
			return c
		}
	}
	return nil
}

// main is the first container, the one of a pod with one container.
func (tp *tabPod) main() *tabCtr { return tp.container("") }

func (tp *tabPod) event(typ, reason, msg string) {
	tp.n.event(tp.ref, typ, reason, msg)
}

func (c *tabCtr) event(typ, reason, msg string) {
	c.tp.n.event(c.ref, typ, reason, msg)
}

// run runs the pod until it is done or deleted.
func (tp *tabPod) run() {
	if tp.phase == corev1.PodFailed {
		tp.update()
		tp.event(corev1.EventTypeWarning, tp.reason, tp.message)
		return
	}
	// The status manager of the kubelet writes the status on its own while
	// the pod starts. Here too: the VM need not wait for two requests to
	// the API server. Every update writes the state of the moment, so a
	// late one does not undo a newer one (push orders them).
	go tp.update()
	if len(tp.ignored) > 0 {
		tp.event(corev1.EventTypeWarning, "Unsupported", "the tab node has no "+strings.Join(tp.ignored, ", ")+"; the containers start without")
	}
	if model, ok := webllmModel(tp.main().spec.Image); ok {
		tp.runWorkload(model)
		return
	}
	if !tp.n.hasVM() {
		c := tp.main()
		msg := fmt.Sprintf("the tab node runs no containers, only images that run in the browser: "+webllmRepo+"<model> (not %q)", c.spec.Image)
		c.event(corev1.EventTypeWarning, "Failed", msg)
		tp.setWaiting(c, "ErrImagePull", msg)
		return
	}
	tp.runVM()
}

// runWorkload runs the language model and restarts it after a failure, with
// the same back-off as the kubelet.
func (tp *tabPod) runWorkload(model string) {
	ctx := tp.ctx
	c := tp.main()
	backoff := backoffStart
	for ctx.Err() == nil {
		tp.mu.Lock()
		logs := tp.newRun(c)
		tp.mu.Unlock()
		w, err := newWebLLM(tp, model)
		if err != nil {
			c.event(corev1.EventTypeWarning, "Failed", err.Error())
			tp.setWaiting(c, "ErrImagePull", err.Error())
			logs.finish()
			return
		}
		pull := &imagePull{c: c, t0: time.Now()}
		err = w.start(ctx, logs, pull)
		if ctx.Err() != nil {
			w.stop()
			logs.finish()
			return
		}
		if err != nil {
			w.stop()
			if !pull.wasPulled() {
				logs.finish()
				// the download failed: the kubelet calls that a failed pull
				tp.pullFailed(c, err)
				if !tp.sleep(backoff) {
					return
				}
				tp.pullBackOff(c, err)
				backoff = min(backoff*2, backoffMax)
				continue
			}
			fmt.Fprintln(logs, "error:", err)
			c.event(corev1.EventTypeWarning, "Failed", err.Error())
			restart := tp.restarts(c, 1)
			if !restart {
				tp.mu.Lock()
				tp.phase = corev1.PodFailed
				tp.mu.Unlock()
			}
			tp.setTerminated(c, 1, "Error", err.Error(), restart)
			if !restart || !tp.backOff(c, &backoff) {
				return
			}
			continue
		}
		pull.done(0)
		c.event(corev1.EventTypeNormal, "Started", "Started container "+c.spec.Name)
		tp.mu.Lock()
		tp.w = w
		tp.mu.Unlock()
		tp.mu.Lock()
		tp.phase = corev1.PodRunning
		tp.mu.Unlock()
		tp.setRunning(c, w.imageID(), w.runtime())
		return
	}
}

// pullFailed tells that the image of c did not come, as the kubelet does.
func (tp *tabPod) pullFailed(c *tabCtr, err error) {
	if _, ok := errors.AsType[errNeverPull](err); ok {
		c.event(corev1.EventTypeWarning, "ErrImageNeverPull", err.Error())
		tp.setWaiting(c, "ErrImageNeverPull", err.Error())
		return
	}
	c.event(corev1.EventTypeWarning, "Failed", fmt.Sprintf("Failed to pull image %q: %v", c.spec.Image, err))
	c.event(corev1.EventTypeWarning, "Failed", "Error: ErrImagePull")
	tp.setWaiting(c, "ErrImagePull", err.Error())
}

// pullBackOff is the state after a failed pull, while the kubelet waits.
func (tp *tabPod) pullBackOff(c *tabCtr, err error) {
	if _, ok := errors.AsType[errNeverPull](err); ok {
		return
	}
	c.event(corev1.EventTypeNormal, "BackOff", fmt.Sprintf("Back-off pulling image %q", c.spec.Image))
	tp.setWaiting(c, "ImagePullBackOff", fmt.Sprintf("Back-off pulling image %q: ErrImagePull: %v", c.spec.Image, err))
}

// sleep waits for d; false if the pod is gone meanwhile.
func (tp *tabPod) sleep(d time.Duration) bool {
	select {
	case <-tp.ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// newRun starts a new log for a run of c: the last one stays for kubectl
// logs --previous. tp.mu is held.
func (tp *tabPod) newRun(c *tabCtr) *logBuffer {
	select {
	case <-c.logs.done:
		c.prev = c.logs
		c.logs = newLogBuffer()
	default:
	}
	c.run++
	return c.logs
}

// restarts tells whether c starts again after it exited with code.
func (tp *tabPod) restarts(c *tabCtr, code int) bool {
	switch {
	case c.kind == ctrEphemeral:
		return false
	case c.sidecar:
		return true
	case c.kind == ctrInit:
		// an init container that failed runs again, unless the pod never
		// restarts
		return code != 0 && tp.restartPolicy != corev1.RestartPolicyNever
	}
	switch tp.restartPolicy {
	case corev1.RestartPolicyNever:
		return false
	case corev1.RestartPolicyOnFailure:
		return code != 0
	}
	return true
}

// backOff waits before c starts again, as CrashLoopBackOff. It returns
// false if the pod is gone meanwhile.
func (tp *tabPod) backOff(c *tabCtr, backoff *time.Duration) bool {
	tp.crashLoop(c, *backoff)
	if !tp.sleep(*backoff) {
		return false
	}
	*backoff = min(*backoff*2, backoffMax)
	return true
}

// crashLoop is the state of c while it waits for a restart.
func (tp *tabPod) crashLoop(c *tabCtr, d time.Duration) {
	msg := fmt.Sprintf("back-off %s restarting failed container=%s pod=%s_%s(%s)", d, c.spec.Name, tp.name, tp.namespace, tp.uid)
	c.event(corev1.EventTypeWarning, "BackOff", "Back-off restarting failed container "+c.spec.Name+" in pod "+tp.name+"_"+tp.namespace+"("+string(tp.uid)+")")
	tp.setWaiting(c, "CrashLoopBackOff", msg)
}

// errNeverPull is the error of a container whose image is not present, with
// the pull policy Never.
type errNeverPull struct{ error }

// imagePull tells the events of a container what the kubelet would say
// about an image pull.
type imagePull struct {
	c       *tabCtr
	t0      time.Time
	mu      sync.Mutex
	pulling bool // the download started
	pulled  bool // the download is done, the container is created
}

// fetching says that the image downloads, not that the browser has it.
func (p *imagePull) fetching() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pulling || p.pulled {
		return
	}
	p.pulling = true
	p.c.event(corev1.EventTypeNormal, "Pulling", fmt.Sprintf("Pulling image %q", p.c.spec.Image))
}

// done says that the download is complete, with its size if known. Without
// a download, the image was already there.
func (p *imagePull) done(size int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pulled {
		return
	}
	p.pulled = true
	image := p.c.spec.Image
	if p.pulling {
		d := time.Since(p.t0).Truncate(time.Millisecond)
		p.c.event(corev1.EventTypeNormal, "Pulled", fmt.Sprintf("Successfully pulled image %q in %v (%v including waiting). Image size: %d bytes.", image, d, d, size))
	} else {
		p.c.event(corev1.EventTypeNormal, "Pulled", fmt.Sprintf("Container image %q already present on machine and can be accessed by the pod", image))
	}
	p.c.event(corev1.EventTypeNormal, "Created", "Created container: "+p.c.spec.Name)
}

// wasPulled reports whether the download was done.
func (p *imagePull) wasPulled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pulled
}

// stop ends the pod. With a reason, it tells the events of the containers
// that run.
func (tp *tabPod) stop(reason, msg string) {
	tp.cancel()
	tp.mu.Lock()
	w := tp.w
	tp.w = nil
	var running []*tabCtr
	for _, c := range tp.ctrs {
		if c.status.State.Running != nil {
			running = append(running, c)
		}
		if c.probes != nil {
			c.probes()
		}
	}
	tp.mu.Unlock()
	if reason != "" {
		for _, c := range running {
			c.event(corev1.EventTypeNormal, reason, msg+c.spec.Name)
		}
	}
	if s, ok := w.(interface{ stop() }); ok {
		s.stop()
	}
	for _, c := range tp.ctrs {
		c.logs.finish()
	}
}

// running is what runs the pod, or nil before a container started.
func (tp *tabPod) running() dialer {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return tp.w
}

func (tp *tabPod) setWaiting(c *tabCtr, reason, msg string) {
	tp.mu.Lock()
	c.status.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}
	c.status.Ready, c.status.Started = false, ptr.To(false)
	c.ready, c.startupOK = false, false
	tp.mu.Unlock()
	tp.update()
}

// setTerminated records that c ended. Unless it restarts, it is done.
func (tp *tabPod) setTerminated(c *tabCtr, code int, reason, msg string, restart bool) {
	tp.mu.Lock()
	logs := c.logs
	now := metav1.Now()
	t := &corev1.ContainerStateTerminated{ExitCode: int32(code), Reason: reason, Message: msg, StartedAt: now, FinishedAt: now, ContainerID: c.status.ContainerID}
	if r := c.status.State.Running; r != nil {
		t.StartedAt = r.StartedAt
	}
	c.status.State = corev1.ContainerState{Terminated: t}
	c.status.Ready, c.status.Started = false, ptr.To(false)
	c.ready, c.startupOK = false, false
	if c.probes != nil {
		c.probes()
		c.probes = nil
	}
	if restart {
		c.status.LastTerminationState = corev1.ContainerState{Terminated: t}
		c.status.RestartCount++
	} else {
		c.done = true
	}
	tp.mu.Unlock()
	logs.finish()
	tp.update()
}

func (tp *tabPod) setRunning(c *tabCtr, imageID, runtime string) {
	tp.mu.Lock()
	c.runAt = time.Now()
	c.status.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}
	// without a startup probe the container counts as started at once,
	// without a readiness probe as ready once started (probes.go)
	c.startupOK = c.spec.StartupProbe == nil
	c.ready = c.startupOK && c.spec.ReadinessProbe == nil
	c.status.Started = ptr.To(c.startupOK)
	c.status.Ready = c.ready
	if imageID != "" {
		c.status.ImageID = imageID
	}
	c.status.ContainerID = runtime + "://" + containerID(tp.uid, c.spec.Name, c.run)
	tp.mu.Unlock()
	tp.update()
}

// containerID is unique per run of a container, as with containerd.
func containerID(uid types.UID, name string, run int) string {
	h := fnv.New32a()
	h.Write([]byte(name))
	return fmt.Sprintf("%.32s%08x%04x", strings.ReplaceAll(string(uid), "-", ""), h.Sum32(), run)
}

// update writes the status of the pod.
func (tp *tabPod) update() {
	tp.push.Lock()
	defer tp.push.Unlock()
	pods := tp.n.cs.CoreV1().Pods(tp.namespace)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pod, err := pods.Get(context.Background(), tp.name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if pod.UID != tp.uid {
			return nil
		}
		s := &pod.Status
		tp.mu.Lock()
		phase, reason, message := tp.phase, tp.reason, tp.message
		var inits, regular, ephemeral []corev1.ContainerStatus
		initialized, ready := true, true
		for _, c := range tp.ctrs {
			cs := *c.status.DeepCopy()
			cs.Ready = c.ready
			cs.Started = ptr.To(c.startupOK && cs.State.Running != nil)
			switch c.kind {
			case ctrInit:
				inits = append(inits, cs)
				if c.sidecar {
					initialized = initialized && *cs.Started
					ready = ready && cs.Ready
				} else {
					initialized = initialized && cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0
				}
			case ctrRegular:
				regular = append(regular, cs)
				ready = ready && cs.Ready
			case ctrEphemeral:
				cs.Ready, cs.Started = false, nil
				ephemeral = append(ephemeral, cs)
			}
		}
		tp.mu.Unlock()

		s.Phase, s.Reason, s.Message = phase, reason, message
		s.HostIP, s.HostIPs = tp.n.ip, []corev1.HostIP{{IP: tp.n.ip}}
		s.PodIP, s.PodIPs = tp.ip, []corev1.PodIP{{IP: tp.ip}}
		s.StartTime = &tp.started
		if phase != corev1.PodFailed || reason == "" {
			s.InitContainerStatuses, s.ContainerStatuses, s.EphemeralContainerStatuses = inits, regular, ephemeral
		}
		cond := func(b bool) corev1.ConditionStatus {
			if b {
				return corev1.ConditionTrue
			}
			return corev1.ConditionFalse
		}
		ready = ready && initialized && phase == corev1.PodRunning
		for _, c := range []struct {
			t corev1.PodConditionType
			s corev1.ConditionStatus
		}{
			{corev1.PodReadyToStartContainers, corev1.ConditionTrue},
			{corev1.PodInitialized, cond(initialized)},
			{corev1.ContainersReady, cond(ready)},
			{corev1.PodReady, cond(ready)},
			{corev1.PodScheduled, corev1.ConditionTrue},
		} {
			setPodCondition(s, c.t, c.s)
		}
		_, err = pods.UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
		return err
	})
	if err != nil && !apierrors.IsNotFound(err) {
		fmt.Fprintln(os.Stderr, "tab node: pod status:", err)
	}
}

func setPodCondition(s *corev1.PodStatus, t corev1.PodConditionType, status corev1.ConditionStatus) {
	for i := range s.Conditions {
		if s.Conditions[i].Type == t {
			if s.Conditions[i].Status != status {
				s.Conditions[i].Status = status
				s.Conditions[i].LastTransitionTime = metav1.Now()
			}
			return
		}
	}
	s.Conditions = append(s.Conditions, corev1.PodCondition{Type: t, Status: status, LastTransitionTime: metav1.Now()})
}
