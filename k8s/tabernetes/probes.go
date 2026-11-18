package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpchealth "google.golang.org/grpc/health/grpc_health_v1"
	grpcstatus "google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/component-base/version"
	"k8s.io/kubernetes/pkg/probe"
	probehttp "k8s.io/kubernetes/pkg/probe/http"
	utilexec "k8s.io/utils/exec"
)

// Probes of the containers in a VM, as the prober of the kubelet runs them:
// startup first, then liveness and readiness, each with its delay, period,
// timeout and thresholds. httpGet, tcpSocket and grpc connect to the pod
// through the network card of the VM, as the kubelet does from the node;
// exec runs in the container like kubectl exec. A probe goroutine only
// reports a change; the loop of the pod (drive in v86.go) acts on it, in
// probed.

// startProbes starts the probes of c for its run run.
func (tp *tabPod) startProbes(v *v86Pod, c *tabCtr, run int) {
	s := c.spec
	if c.kind == ctrEphemeral || s.StartupProbe == nil && s.LivenessProbe == nil && s.ReadinessProbe == nil {
		return
	}
	ctx, cancel := context.WithCancel(tp.ctx)
	tp.mu.Lock()
	if c.probes != nil {
		c.probes()
	}
	c.probes = cancel
	tp.mu.Unlock()
	post := func(kind string, ok bool) {
		tp.post(podEvent{kind: "probe", gen: v.gen, ctr: c, run: run, msg: kind, ok: ok})
	}
	go func() {
		if p := s.StartupProbe; p != nil {
			ok := false
			tp.probeLoop(ctx, v, c, "Startup", p, func(r bool) bool {
				ok = r
				post("startup", r)
				return true
			})
			if !ok {
				return
			}
		}
		if p := s.LivenessProbe; p != nil {
			go tp.probeLoop(ctx, v, c, "Liveness", p, func(r bool) bool {
				if !r {
					post("liveness", false)
				}
				return !r
			})
		}
		if p := s.ReadinessProbe; p != nil {
			// readiness starts as failed: a change to failed is no news
			last := false
			tp.probeLoop(ctx, v, c, "Readiness", p, func(r bool) bool {
				if r != last {
					last = r
					post("readiness", r)
				}
				return false
			})
		}
	}()
}

// probeLoop runs p until ctx ends or report returns true. report gets the
// result once the success or failure threshold is reached.
func (tp *tabPod) probeLoop(ctx context.Context, v *v86Pod, c *tabCtr, name string, p *corev1.Probe, report func(ok bool) bool) {
	sec := func(s, def int32) time.Duration {
		if s <= 0 {
			s = def
		}
		return time.Duration(s) * time.Second
	}
	period, timeout := sec(p.PeriodSeconds, 10), sec(p.TimeoutSeconds, 1)
	successes, failures := max(p.SuccessThreshold, 1), max(p.FailureThreshold, 1)
	if d := time.Duration(p.InitialDelaySeconds) * time.Second; d > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
	}
	var ok, failed int32
	for {
		pctx, cancel := context.WithTimeout(ctx, timeout)
		good, out := tp.probe(pctx, v, c, p, timeout)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if good {
			ok, failed = ok+1, 0
			if ok >= successes && report(true) {
				return
			}
		} else {
			ok, failed = 0, failed+1
			c.event(corev1.EventTypeWarning, "Unhealthy", name+" probe failed: "+out)
			if failed >= failures && report(false) {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(period):
		}
	}
}

// probe runs p once and returns the result with the output, as the kubelet
// tells it in the event of a failed probe.
func (tp *tabPod) probe(ctx context.Context, v *v86Pod, c *tabCtr, p *corev1.Probe, timeout time.Duration) (bool, string) {
	dialPort := func(ctx context.Context, port int) (net.Conn, error) {
		type res struct {
			c   net.Conn
			err error
		}
		ch := make(chan res, 1)
		go func() {
			conn, err := v.dial(int32(port))
			ch <- res{conn, err}
		}()
		select {
		case r := <-ch:
			return r.c, r.err
		case <-ctx.Done():
			go func() {
				if r := <-ch; r.c != nil {
					r.c.Close()
				}
			}()
			return nil, fmt.Errorf("dial tcp %s: i/o timeout", net.JoinHostPort(tp.ip, strconv.Itoa(port)))
		}
	}
	switch {
	case p.Exec != nil:
		var out bytes.Buffer
		err := v.exec(ctx, c, p.Exec.Command, nil, &out, &out)
		if ctx.Err() == context.DeadlineExceeded {
			return false, fmt.Sprintf("command %q timed out", strings.Join(p.Exec.Command, " "))
		}
		if _, ok := errors.AsType[utilexec.CodeExitError](err); ok || err == nil {
			return err == nil, out.String()
		}
		return false, err.Error()
	case p.HTTPGet != nil:
		req, err := probehttp.NewRequestForHTTPGetAction(p.HTTPGet, &c.spec, tp.ip, "probe")
		if err != nil {
			return false, err.Error()
		}
		port, _ := strconv.Atoi(req.URL.Port())
		client := &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DisableKeepAlives: true,
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialPort(ctx, port)
				},
			},
			CheckRedirect: probehttp.RedirectChecker(false),
		}
		res, out, err := probehttp.DoHTTPProbe(req.WithContext(ctx), client)
		if err != nil {
			return false, err.Error()
		}
		// a redirect to another host is a warning, and counts as success
		return res != probe.Failure, out
	case p.TCPSocket != nil:
		port, err := probe.ResolveContainerPort(p.TCPSocket.Port, &c.spec)
		if err != nil {
			return false, err.Error()
		}
		conn, err := dialPort(ctx, port)
		if err != nil {
			return false, err.Error()
		}
		conn.Close()
		return true, ""
	case p.GRPC != nil:
		return tp.grpcProbe(ctx, p.GRPC, timeout, dialPort)
	}
	return false, "the probe has no handler"
}

// grpcProbe asks the health service of the container, with the messages of
// the gRPC prober of the kubelet (k8s.io/kubernetes/pkg/probe/grpc).
func (tp *tabPod) grpcProbe(ctx context.Context, g *corev1.GRPCAction, timeout time.Duration, dialPort func(context.Context, int) (net.Conn, error)) (bool, string) {
	addr := net.JoinHostPort(tp.ip, strconv.Itoa(int(g.Port)))
	conn, err := grpc.NewClient("passthrough:///"+addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUserAgent(fmt.Sprintf("kube-probe/%s.%s", version.Get().Major, version.Get().Minor)),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return dialPort(ctx, int(g.Port)) }),
	)
	if err != nil {
		return false, fmt.Sprintf("error: failed to connect service at %q: %+v", addr, err)
	}
	defer conn.Close()
	service := ""
	if g.Service != nil {
		service = *g.Service
	}
	resp, err := grpchealth.NewHealthClient(conn).Check(ctx, &grpchealth.HealthCheckRequest{Service: service})
	if err != nil {
		if s, ok := grpcstatus.FromError(err); ok {
			switch s.Code() {
			case codes.Unimplemented:
				return false, "error: this server does not implement the grpc health protocol (grpc.health.v1.Health): " + s.Message()
			case codes.DeadlineExceeded:
				return false, fmt.Sprintf("timeout: health rpc did not complete within %v", timeout)
			case codes.Unavailable:
				return false, fmt.Sprintf("timeout: failed to connect service %q within %v: %+v", addr, timeout, err)
			}
		}
		return false, fmt.Sprintf("error: health rpc probe failed: %+v", err)
	}
	if resp.GetStatus() != grpchealth.HealthCheckResponse_SERVING {
		return false, fmt.Sprintf("service unhealthy (responded with %q)", resp.GetStatus().String())
	}
	return true, "service healthy"
}

// probed acts on a result of a probe in the loop of the pod. advance goes
// on with the next init container after a sidecar started.
func (tp *tabPod) probed(v *v86Pod, c *tabCtr, e podEvent, advance func(*tabCtr)) {
	tp.mu.Lock()
	current := e.run == c.run && c.status.State.Running != nil
	if !current {
		tp.mu.Unlock()
		return
	}
	switch e.msg {
	case "startup":
		if e.ok {
			c.startupOK = true
			c.ready = c.spec.ReadinessProbe == nil
		}
	case "readiness":
		c.ready = e.ok && c.startupOK
	}
	tp.mu.Unlock()
	switch {
	case e.msg == "startup" && e.ok:
		tp.update()
		if c.sidecar {
			advance(c)
		}
	case e.msg == "readiness":
		tp.update()
	default:
		// a failed startup or liveness probe: the container is killed
		// and started again by its restart policy
		c.event(corev1.EventTypeNormal, "Killing", fmt.Sprintf("Container %s failed %s probe, will be restarted", c.spec.Name, e.msg))
		tp.killCtr(v, c, e.run, probeGrace(tp.pod, c, e.msg))
	}
}

// killCtr stops c with SIGTERM, and with SIGKILL after grace if the run is
// still there.
func (tp *tabPod) killCtr(v *v86Pod, c *tabCtr, run int, grace time.Duration) {
	v.kill(c, 15)
	time.AfterFunc(grace, func() {
		tp.mu.Lock()
		alive := c.run == run && c.status.State.Running != nil
		tp.mu.Unlock()
		if alive {
			v.kill(c, 9)
		}
	})
}

// probeGrace is the time a container gets to stop after a failed probe: the
// one of the probe, else the one of the pod.
func probeGrace(pod *corev1.Pod, c *tabCtr, kind string) time.Duration {
	var p *corev1.Probe
	switch kind {
	case "startup":
		p = c.spec.StartupProbe
	case "liveness":
		p = c.spec.LivenessProbe
	}
	if p != nil && p.TerminationGracePeriodSeconds != nil {
		return time.Duration(*p.TerminationGracePeriodSeconds) * time.Second
	}
	if pod.Spec.TerminationGracePeriodSeconds != nil {
		return time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second
	}
	return 30 * time.Second
}
