package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"k8s.io/cri-streaming/pkg/streaming/portforward"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
)

// The kubelet API of the tab node, as far as kubectl uses it: logs, exec and
// port-forward. The apiserver reaches it on tabNodeIP:10250 in the in-process
// network, or from the shared worker through a MessagePort (tabs.go). It does
// not check the client: only the apiserver gets there.

const (
	streamIdleTimeout     = 4 * time.Hour
	streamCreationTimeout = 30 * time.Second
)

func (n *tabNode) serveKubelet() error {
	n.ca.issue("tab-kubelet", tabNodeName, nil, true, []string{tabNodeName}, []net.IP{net.ParseIP(tabNodeIP)})
	cert, err := tls.LoadX509KeyPair(p("tab-kubelet.crt"), p("tab-kubelet.key"))
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", tabNodeIP+":10250")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("GET /containerLogs/{ns}/{pod}/{container}", n.serveLogs)
	mux.HandleFunc("/exec/{ns}/{pod}/{container}", n.serveExec)
	mux.HandleFunc("/exec/{ns}/{pod}/{uid}/{container}", n.serveExec)
	mux.HandleFunc("/portForward/{ns}/{pod}", n.servePortForward)
	mux.HandleFunc("/portForward/{ns}/{pod}/{uid}", n.servePortForward)
	mux.HandleFunc("/attach/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "the tab node has no container to attach to: use kubectl exec", http.StatusBadRequest)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{cert}}))
	return nil
}

// pod finds the pod of a request, also by UID if the request names one.
func (n *tabNode) pod(w http.ResponseWriter, r *http.Request) *tabPod {
	tp := n.podByName(r.PathValue("ns"), r.PathValue("pod"))
	if tp == nil || (r.PathValue("uid") != "" && r.PathValue("uid") != string(tp.uid)) {
		http.Error(w, "pod does not exist", http.StatusNotFound)
		return nil
	}
	if c := r.PathValue("container"); c != "" && c != tp.container.Name {
		http.Error(w, fmt.Sprintf("container %s is not valid for pod %s", c, tp.name), http.StatusNotFound)
		return nil
	}
	return tp
}

func (n *tabNode) serveLogs(w http.ResponseWriter, r *http.Request) {
	tp := n.pod(w, r)
	if tp == nil {
		return
	}
	q := r.URL.Query()
	if q.Get("previous") == "true" {
		http.Error(w, "previous terminated container "+tp.container.Name+" not found: the tab keeps the log of one run", http.StatusBadRequest)
		return
	}
	follow, timestamps := q.Get("follow") == "true", q.Get("timestamps") == "true"
	lines, next, more := tp.logs.since(0)
	if s := q.Get("tailLines"); s != "" {
		if t, err := strconv.Atoi(s); err == nil && t >= 0 && t < len(lines) {
			lines = lines[len(lines)-t:]
		}
	}
	if s := q.Get("sinceSeconds"); s != "" {
		if secs, err := strconv.Atoi(s); err == nil {
			cut := time.Now().Add(-time.Duration(secs) * time.Second)
			for len(lines) > 0 && lines[0].t.Before(cut) {
				lines = lines[1:]
			}
		}
	}
	w.Header().Set("Content-Type", "text/plain")
	bw := bufio.NewWriter(w)
	flush := func() bool {
		if err := bw.Flush(); err != nil {
			return false
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return true
	}
	for {
		for _, l := range lines {
			if timestamps {
				bw.WriteString(l.t.UTC().Format(time.RFC3339Nano) + " ")
			}
			bw.WriteString(l.text + "\n")
		}
		if !flush() || !follow {
			return
		}
		select {
		case <-more:
		case <-tp.ctx.Done():
			return
		case <-r.Context().Done():
			return
		}
		lines, next, more = tp.logs.since(next)
	}
}

// executor runs kubectl exec in the workload of a pod.
type executor struct{ tp *tabPod }

func (e executor) ExecInContainer(ctx context.Context, _, _, _ string, cmd []string, in io.Reader, out, errOut io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize, _ time.Duration) error {
	w := e.tp.running()
	if w == nil {
		return errors.New("container not found (" + e.tp.container.Name + " is not running)")
	}
	if in == nil {
		in = eofReader{}
	}
	if out == nil {
		out = nopCloser{io.Discard}
	}
	if errOut == nil {
		errOut = out // a tty has no stderr
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		// the pod goes away: end the command
		select {
		case <-e.tp.ctx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	return w.exec(ctx, cmd, in, out, errOut)
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

func (n *tabNode) serveExec(w http.ResponseWriter, r *http.Request) {
	tp := n.pod(w, r)
	if tp == nil {
		return
	}
	opts, err := remotecommand.NewOptions(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	remotecommand.ServeExec(w, r, executor{tp}, tp.name, string(tp.uid), tp.container.Name, r.URL.Query()["command"], opts,
		streamIdleTimeout, streamCreationTimeout, remotecommand.SupportedStreamingProtocols)
}

// forwarder connects kubectl port-forward to a port of the workload.
type forwarder struct{ tp *tabPod }

func (f forwarder) PortForward(ctx context.Context, _, _ string, port int32, stream io.ReadWriteCloser) error {
	wl := f.tp.running()
	if wl == nil {
		return errors.New("the container is not running")
	}
	c, err := wl.dial(port)
	if err != nil {
		return err
	}
	defer c.Close()
	// Done when one side is: the other one gets closed.
	done := make(chan struct{}, 2)
	go func() { io.Copy(c, stream); done <- struct{}{} }()
	go func() { io.Copy(stream, c); done <- struct{}{} }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

func (n *tabNode) servePortForward(w http.ResponseWriter, r *http.Request) {
	tp := n.pod(w, r)
	if tp == nil {
		return
	}
	opts, err := portforward.NewV4Options(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	portforward.ServePortForward(w, r, forwarder{tp}, tp.name, string(tp.uid), opts,
		streamIdleTimeout, streamCreationTimeout, portforward.SupportedProtocols)
}
