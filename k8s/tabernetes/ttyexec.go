package main

import (
	"context"
	"io"
	"strings"
	"sync/atomic"

	"github.com/moby/term"
	"mvdan.cc/sh/v3/interp"
)

// kubectl exec -t, attach -t, run -t and debug -t on the terminal. kubectl
// asks the terminal package (moby/term, js variant in _overlay/) whether
// stdin is a terminal, switches it to raw mode and polls its size. Here the
// terminal of the page answers: kubectl then sends every key to the
// container and the container gets a tty, as on a real terminal.

func init() {
	term.TerminalStreams = func() (io.ReadCloser, io.Writer, io.Writer) {
		in := ttyIn.Load()
		if in == nil {
			in = &termReader{t: newTerminal(), done: closedChan}
		}
		return in, ttyOut{in.t}, ttyOut{in.t}
	}
	term.TerminalSize = func() (uint16, uint16) {
		t := newTerminal()
		if in := ttyIn.Load(); in != nil {
			t = in.t
		}
		cols, rows := t.size()
		return uint16(cols), uint16(rows)
	}
	term.TerminalRaw = func() func() {
		in := ttyIn.Load()
		if in == nil {
			// the command line was canceled before kubectl got here
			return func() {}
		}
		return in.t.enterLine()
	}
}

var closedChan = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

// The hooks above and the editor of kubectl edit (vi.go) are process wide:
// they cannot tell which tab runs the kubectl that calls them. So only one
// such kubectl runs at a time; another tab waits for it.
var (
	ttyOwner = make(chan struct{}, 1)
	// ttyIn is the stdin of the kubectl -t that owns the terminal, if any.
	ttyIn atomic.Pointer[termReader]
	// editTerm is the terminal of the kubectl edit that runs, if any.
	editTerm atomic.Pointer[terminal]
)

// ownTTY waits until no other kubectl uses the terminal hooks. The returned
// func lets the next one have them.
func ownTTY(ctx context.Context, w io.Writer) (func(), error) {
	select {
	case ttyOwner <- struct{}{}:
	default:
		io.WriteString(w, "waiting for kubectl in another tab to end...\n")
		select {
		case ttyOwner <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return func() { <-ttyOwner }, nil
}

// termReader is stdin on the terminal in raw mode: the keys as the browser
// sends them, until the kubectl command ends.
type termReader struct {
	t    *terminal
	done <-chan struct{}
	buf  []byte
}

func (r *termReader) Fd() uintptr  { return term.TerminalFd }
func (r *termReader) Close() error { return nil }

func (r *termReader) Read(b []byte) (int, error) {
	for len(r.buf) == 0 {
		select {
		case s := <-r.t.keys:
			if s != resizeKey {
				r.buf = []byte(s)
			}
		case <-r.done:
			return 0, io.EOF
		}
	}
	n := copy(b, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// ttyOut is stdout on the terminal.
type ttyOut struct{ t *terminal }

func (ttyOut) Fd() uintptr                   { return term.TerminalFd }
func (o ttyOut) Write(b []byte) (int, error) { return o.t.Write(b) }

// wantsTTY tells whether kubectl is asked for a tty (-t), before "--".
func wantsTTY(args []string) bool {
	for _, a := range args {
		switch {
		case a == "--":
			return false
		case a == "-t" || a == "--tty" || a == "--tty=true":
			return true
		case len(a) > 2 && a[0] == '-' && a[1] != '-' && strings.Trim(a[1:], "it") == "" && strings.Contains(a, "t"):
			return true // -it, -ti
		}
	}
	return false
}

// kubectlTTY runs kubectl with the terminal as stdin.
func kubectlTTY(ctx context.Context, hc interp.HandlerContext, args []string) error {
	t := termFrom(ctx)
	release, err := ownTTY(ctx, hc.Stderr)
	if err != nil {
		return interp.ExitStatus(130)
	}
	defer release()
	done := make(chan struct{})
	in := &termReader{t: t, done: done}
	ttyIn.Store(in)
	defer func() {
		ttyIn.CompareAndSwap(in, nil)
		close(done)
	}()
	code := make(chan int, 1)
	// stderr goes straight to the terminal: kubectl attach says "If you
	// don't see a command prompt, try pressing enter." in raw mode.
	go func() { code <- runKubectl(ctx, args, in, hc.Stdout, ctxWriter{ctx, ttyOut{t}}) }()
	select {
	case c := <-code:
		return exitCode(c)
	case <-ctx.Done():
		return interp.ExitStatus(130)
	}
}
