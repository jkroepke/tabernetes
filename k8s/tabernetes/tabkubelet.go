package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/cri-streaming/pkg/streaming/portforward"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
)

// The kubelet API of the tab node, as far as kubectl uses it: logs, exec,
// port-forward and, for container images, attach. The apiserver reaches it on
// tabNodeIP:10250 in the in-process network, or from the shared worker
// through a MessagePort (tabs.go). It does not check the client: only the
// apiserver gets there.

const (
	streamIdleTimeout     = 4 * time.Hour
	streamCreationTimeout = 30 * time.Second
)

func (n *tabNode) serveKubelet() (func(), error) {
	n.ca.issue("tab-kubelet", tabNodeName, nil, true, []string{tabNodeName}, []net.IP{net.ParseIP(tabNodeIP)})
	cert, err := tls.LoadX509KeyPair(p("tab-kubelet.crt"), p("tab-kubelet.key"))
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", tabNodeIP+":10250")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("GET /containerLogs/{ns}/{pod}/{container}", n.serveLogs)
	mux.HandleFunc("/exec/{ns}/{pod}/{container}", n.serveExec)
	mux.HandleFunc("/exec/{ns}/{pod}/{uid}/{container}", n.serveExec)
	mux.HandleFunc("/portForward/{ns}/{pod}", n.servePortForward)
	mux.HandleFunc("/portForward/{ns}/{pod}/{uid}", n.servePortForward)
	mux.HandleFunc("/attach/{ns}/{pod}/{container}", n.serveAttach)
	mux.HandleFunc("/attach/{ns}/{pod}/{uid}/{container}", n.serveAttach)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{cert}}))
	return func() {
		srv.Close()
		l.Close()
	}, nil
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
	tp.mu.Lock()
	logs := tp.logs
	waiting := tp.status.State.Waiting
	tp.mu.Unlock()
	lines, next, more := logs.since(0)
	if waiting != nil && next == 0 {
		// as the kubelet says it while the image is pulled
		http.Error(w, fmt.Sprintf("container %q in pod %q is waiting to start: %s", tp.container.Name, tp.name, waiting.Reason), http.StatusBadRequest)
		return
	}
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
			bw.WriteString(l.text)
			if !l.partial {
				bw.WriteByte('\n')
			}
		}
		if !flush() || !follow {
			return
		}
		select {
		case <-logs.done:
			follow = false
		case <-more:
		case <-tp.ctx.Done():
			return
		case <-r.Context().Done():
			return
		}
		lines, next, more = logs.since(next)
	}
}

// executor runs kubectl exec in the workload of a pod.
type executor struct{ tp *tabPod }

func (e executor) ExecInContainer(ctx context.Context, _, _, _ string, cmd []string, in io.Reader, out, errOut io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize, _ time.Duration) error {
	w := e.tp.running()
	if w == nil {
		return errors.New("container not found (" + e.tp.container.Name + " is not running)")
	}
	if out == nil {
		out = nopCloser{io.Discard}
	}
	if errOut == nil {
		errOut = out // a tty has no stderr
	}
	ctx, cancel := e.podContext(ctx)
	defer cancel()
	if s, ok := w.(streamer); ok {
		return s.stream(ctx, false, cmd, in, out, errOut, tty, resize)
	}
	if in == nil {
		in = eofReader{}
	}
	if tty {
		// The workload reads lines: the tty does the editing, as in
		// canonical mode.
		o := &crlfWriter{w: out}
		in, out, errOut = lineDiscipline(in, o), nopCloser{o}, nopCloser{o}
	}
	return w.exec(ctx, cmd, in, out, errOut)
}

// crlfWriter turns \n into \r\n, as a tty does on output (onlcr).
type crlfWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (c *crlfWriter) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.w.Write(bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))); err != nil {
		return 0, err
	}
	return len(b), nil
}

// lineDiscipline is the canonical mode of a tty for a workload that has no
// tty of its own: it echoes the keys, edits the line and hands it on with
// Enter. Ctrl-C sends \x03 and ends the input, as does Ctrl-D on an empty
// line.
func lineDiscipline(in io.Reader, echo io.Writer) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		var line []rune
		esc := 0 // in an escape sequence (arrow keys): 1 after ESC, 2 after ESC [
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			for _, r := range string(buf[:n]) {
				switch {
				case esc == 1:
					esc = 0
					if r == '[' || r == 'O' {
						esc = 2
					}
				case esc == 2:
					if r >= 0x40 && r <= 0x7e {
						esc = 0
					}
				case r == 0x1b:
					esc = 1
				case r == '\r' || r == '\n':
					io.WriteString(echo, "\n")
					if _, err := io.WriteString(pw, string(line)+"\n"); err != nil {
						return
					}
					line = nil
				case r == 0x7f || r == '\b':
					if len(line) > 0 {
						line = line[:len(line)-1]
						io.WriteString(echo, "\b \b")
					}
				case r == 0x15: // Ctrl-U
					io.WriteString(echo, strings.Repeat("\b \b", len(line)))
					line = nil
				case r == 0x03:
					io.WriteString(echo, "^C\n")
					io.WriteString(pw, "\x03\n")
					return
				case r == 0x04:
					if len(line) == 0 {
						return
					}
				case r < 0x20:
				default:
					line = append(line, r)
					io.WriteString(echo, string(r))
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return pr
}

// podContext ends with the pod.
func (e executor) podContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-e.tp.ctx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// AttachContainer connects kubectl attach to the main process of a container
// image (v86). The other workloads have none.
func (e executor) AttachContainer(ctx context.Context, _, _, _ string, in io.Reader, out, errOut io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	w := e.tp.running()
	if w == nil {
		return errors.New("container not found (" + e.tp.container.Name + " is not running)")
	}
	s, ok := w.(streamer)
	if !ok {
		return errors.New("this workload has no process to attach to: use kubectl exec or kubectl logs")
	}
	if out == nil {
		out = nopCloser{io.Discard}
	}
	if errOut == nil {
		errOut = out
	}
	ctx, cancel := e.podContext(ctx)
	defer cancel()
	return s.stream(ctx, true, nil, in, out, errOut, tty, resize)
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

func (n *tabNode) serveAttach(w http.ResponseWriter, r *http.Request) {
	tp := n.pod(w, r)
	if tp == nil {
		return
	}
	opts, err := remotecommand.NewOptions(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	remotecommand.ServeAttach(w, r, executor{tp}, tp.name, string(tp.uid), tp.container.Name, opts,
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
