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
			in = &termReader{done: closedChan}
		}
		return in, ttyOut{}, ttyOut{}
	}
	term.TerminalSize = func() (uint16, uint16) {
		cols, rows := tty.size()
		return uint16(cols), uint16(rows)
	}
	term.TerminalRaw = func() func() {
		if ttyIn.Load() == nil {
			// the command line was canceled before kubectl got here
			return func() {}
		}
		return tty.enterLine()
	}
}

var closedChan = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

// ttyIn is the stdin of the kubectl that owns the terminal, if any.
var ttyIn atomic.Pointer[termReader]

// termReader is stdin on the terminal in raw mode: the keys as the browser
// sends them, until the kubectl command ends.
type termReader struct {
	done <-chan struct{}
	buf  []byte
}

func (r *termReader) Fd() uintptr  { return term.TerminalFd }
func (r *termReader) Close() error { return nil }

func (r *termReader) Read(b []byte) (int, error) {
	for len(r.buf) == 0 {
		select {
		case s := <-tty.keys:
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
type ttyOut struct{}

func (ttyOut) Fd() uintptr                 { return term.TerminalFd }
func (ttyOut) Write(b []byte) (int, error) { return tty.Write(b) }

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
	done := make(chan struct{})
	in := &termReader{done: done}
	ttyIn.Store(in)
	defer func() {
		ttyIn.CompareAndSwap(in, nil)
		close(done)
	}()
	code := make(chan int, 1)
	// stderr goes straight to the terminal: kubectl attach says "If you
	// don't see a command prompt, try pressing enter." in raw mode.
	go func() { code <- runKubectl(ctx, args, in, hc.Stdout, ctxWriter{ctx, ttyOut{}}) }()
	select {
	case c := <-code:
		return exitCode(c)
	case <-ctx.Done():
		return interp.ExitStatus(130)
	}
}
