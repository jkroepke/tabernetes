package main

import (
	"io"
	"net"
	"sync"
	"syscall/js"
	"time"
)

// vmConn is a TCP connection of the network card of a pod VM (the network
// adapter of v86 ends TCP in JavaScript, web/src/vmworker.js), as a
// net.Conn. It is a connection into the pod (port-forward, exec, a
// connection from another pod) or one that the pod opened (podnet.go).
// It closes each direction on its own: CloseWrite sends a FIN to the pod,
// and Read returns io.EOF once the pod sent its FIN.
type vmConn struct {
	h     js.Value // write(data), end(), close() of vm.js
	funcs []js.Func

	mu     sync.Mutex
	cond   *sync.Cond
	queue  [][]byte
	eof    bool // the pod closed its side, or the connection is gone
	closed bool

	connected chan bool
	endOnce   sync.Once
	closeOnce sync.Once
	local     net.Addr
	remote    net.Addr
}

func newVMConn(local, remote net.Addr) *vmConn {
	c := &vmConn{connected: make(chan bool, 1), local: local, remote: remote}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *vmConn) fn(f func(a []js.Value)) js.Func {
	jf := js.FuncOf(func(_ js.Value, a []js.Value) any { f(a); return nil })
	c.funcs = append(c.funcs, jf)
	return jf
}

// callbacks returns the handlers of vm.js: connect(), data(bytes), end()
// (the pod sent a FIN) and close() (the connection is gone).
func (c *vmConn) callbacks() (onConnect, onData, onEnd, onClose js.Func) {
	onConnect = c.fn(func([]js.Value) {
		select {
		case c.connected <- true:
		default:
		}
	})
	onData = c.fn(func(a []js.Value) {
		b := make([]byte, a[0].Length())
		js.CopyBytesToGo(b, a[0])
		c.mu.Lock()
		if !c.eof && !c.closed {
			c.queue = append(c.queue, b)
			c.cond.Signal()
		}
		c.mu.Unlock()
	})
	onEnd = c.fn(func([]js.Value) { c.setEOF() })
	onClose = c.fn(func([]js.Value) {
		select {
		case c.connected <- false:
		default:
		}
		c.setEOF()
	})
	return
}

func (c *vmConn) setEOF() {
	c.mu.Lock()
	c.eof = true
	c.cond.Broadcast()
	c.mu.Unlock()
}

func (c *vmConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.queue) == 0 && !c.eof && !c.closed {
		c.cond.Wait()
	}
	if c.closed {
		return 0, net.ErrClosed
	}
	if len(c.queue) == 0 {
		return 0, io.EOF
	}
	n := copy(b, c.queue[0])
	if n == len(c.queue[0]) {
		c.queue[0] = nil
		c.queue = c.queue[1:]
	} else {
		c.queue[0] = c.queue[0][n:]
	}
	return n, nil
}

func (c *vmConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	arr := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(arr, b)
	c.h.Call("write", arr)
	return len(b), nil
}

// CloseWrite sends a FIN to the pod; it can still send.
func (c *vmConn) CloseWrite() error {
	c.endOnce.Do(func() { c.h.Call("end") })
	return nil
}

func (c *vmConn) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.queue = nil
		c.cond.Broadcast()
		c.mu.Unlock()
		c.h.Call("close")
		c.release()
	})
	return nil
}

// release frees the handlers; the worker may still have sent messages
// that are not delivered yet.
func (c *vmConn) release() {
	funcs := c.funcs
	c.funcs = nil
	time.AfterFunc(time.Minute, func() {
		for _, f := range funcs {
			f.Release()
		}
	})
}

func (c *vmConn) LocalAddr() net.Addr                { return c.local }
func (c *vmConn) RemoteAddr() net.Addr               { return c.remote }
func (c *vmConn) SetDeadline(t time.Time) error      { return nil }
func (c *vmConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *vmConn) SetWriteDeadline(t time.Time) error { return nil }

// pipeTCP copies between a and b until both directions ended, and passes a
// FIN on (CloseWrite) where the connection has one.
func pipeTCP(a, b net.Conn) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}
