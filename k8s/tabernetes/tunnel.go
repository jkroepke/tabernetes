package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall/js"
	"time"
)

// dcConn adapts a browser RTCDataChannel to an io.ReadWriteCloser.
type dcConn struct {
	dc     js.Value
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
	funcs  []js.Func
}

const (
	dcChunk       = 16 * 1024
	dcHighWater   = 4 * 1024 * 1024
	uint8ArrayCtr = "Uint8Array"
)

func newDCConn(dc js.Value) *dcConn {
	c := &dcConn{dc: dc}
	c.cond = sync.NewCond(&c.mu)
	dc.Set("binaryType", "arraybuffer")
	onMsg := js.FuncOf(func(this js.Value, a []js.Value) any {
		data := a[0].Get("data")
		var b []byte
		if data.Type() == js.TypeString {
			b = []byte(data.String())
		} else {
			arr := js.Global().Get(uint8ArrayCtr).New(data)
			b = make([]byte, arr.Get("length").Int())
			js.CopyBytesToGo(b, arr)
		}
		c.mu.Lock()
		c.buf = append(c.buf, b...)
		c.cond.Broadcast()
		c.mu.Unlock()
		return nil
	})
	onClose := js.FuncOf(func(js.Value, []js.Value) any {
		c.mu.Lock()
		c.closed = true
		c.cond.Broadcast()
		c.mu.Unlock()
		return nil
	})
	c.funcs = []js.Func{onMsg, onClose}
	dc.Set("onmessage", onMsg)
	dc.Set("onclose", onClose)
	return c
}

func (c *dcConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.buf) == 0 && !c.closed {
		c.cond.Wait()
	}
	if len(c.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	if len(c.buf) == 0 {
		c.buf = nil
	}
	return n, nil
}

func (c *dcConn) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if closed || c.dc.Get("readyState").String() != "open" {
			return written, io.ErrClosedPipe
		}
		for c.dc.Get("bufferedAmount").Int() > dcHighWater {
			time.Sleep(5 * time.Millisecond)
		}
		n := min(len(p), dcChunk)
		arr := js.Global().Get(uint8ArrayCtr).New(n)
		js.CopyBytesToJS(arr, p[:n])
		c.dc.Call("send", arr)
		p = p[n:]
		written += n
	}
	return written, nil
}

func (c *dcConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.cond.Broadcast()
	c.mu.Unlock()
	c.dc.Call("close")
	return nil
}

func splice(a, b io.ReadWriteCloser) {
	var once sync.Once
	closeBoth := func() { a.Close(); b.Close() }
	go func() { io.Copy(a, b); once.Do(closeBoth) }()
	go func() { io.Copy(b, a); once.Do(closeBoth) }()
}

// portConn adapts a MessagePort to an io.ReadWriteCloser. It carries the
// connections between the tabs and the control plane in the SharedWorker
// (tabs.go): every message is an ArrayBuffer of data, null ends the
// connection. The buffers are transferred, not copied.
type portConn struct {
	port   js.Value
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
	onMsg  js.Func
}

func newPortConn(port js.Value) *portConn {
	c := &portConn{port: port}
	c.cond = sync.NewCond(&c.mu)
	c.onMsg = js.FuncOf(func(this js.Value, a []js.Value) any {
		data := a[0].Get("data")
		c.mu.Lock()
		defer c.mu.Unlock()
		if data.Type() != js.TypeObject {
			c.closed = true
		} else {
			arr := js.Global().Get(uint8ArrayCtr).New(data)
			n := len(c.buf)
			c.buf = append(c.buf, make([]byte, arr.Get("length").Int())...)
			js.CopyBytesToGo(c.buf[n:], arr)
		}
		c.cond.Broadcast()
		return nil
	})
	port.Set("onmessage", c.onMsg) // starts the port
	return c
}

func (c *portConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.buf) == 0 && !c.closed {
		c.cond.Wait()
	}
	if len(c.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	if len(c.buf) == 0 {
		c.buf = nil
	}
	return n, nil
}

func (c *portConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return 0, io.ErrClosedPipe
	}
	arr := js.Global().Get(uint8ArrayCtr).New(len(p))
	js.CopyBytesToJS(arr, p)
	buf := arr.Get("buffer")
	c.port.Call("postMessage", buf, []any{buf})
	return len(p), nil
}

func (c *portConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed && c.onMsg.IsUndefined() {
		return nil
	}
	c.closed = true
	c.cond.Broadcast()
	c.port.Call("postMessage", js.Null())
	c.port.Call("close")
	// No more messages arrive on a closed port.
	c.port.Set("onmessage", js.Null())
	c.onMsg.Release()
	c.onMsg = js.Func{}
	return nil
}

// newJSConn wraps what JS hands over for a connection: an RTCDataChannel
// (join) or a MessagePort (tabs.go).
func newJSConn(v js.Value) io.ReadWriteCloser {
	if v.Get("bufferedAmount").Type() == js.TypeNumber {
		return newDCConn(v)
	}
	return newPortConn(v)
}

// attachChannel is called from JS for every data channel the node agent opens.
func attachChannel(dc js.Value) {
	attach(newDCConn(dc), dc.Get("label").String())
}

// attach connects a connection that the other side opened to its target:
// "api" tunnels to the kube-apiserver, "kubelet" to the kubelet API of this
// tab (tabkubelet.go).
func attach(conn io.ReadWriteCloser, label string) {
	go func() {
		var target string
		switch label {
		case "api":
			target = "127.0.0.1:6443"
		case "kubelet", "kubelet:10250":
			target = tabNodeIP + ":10250"
		default:
			fmt.Fprintln(os.Stderr, "tabernetes: unknown connection", label)
			conn.Close()
			return
		}
		tcp, err := net.DialTimeout("tcp", target, 10*time.Second)
		if err != nil {
			fmt.Fprintln(os.Stderr, "tabernetes: tunnel dial:", err)
			conn.Close()
			return
		}
		splice(conn, tcp)
	}()
}

type joinedNode struct {
	name string
	ip   string
	open js.Value // JS function(label) => Promise<RTCDataChannel or MessagePort>
	ln   net.Listener
}

var (
	nodesMu  sync.Mutex
	nodes    = map[string]*joinedNode{}
	nextNode = 2
)

// prepareJoin creates a bootstrap token and allocates an IP for a new node.
// The returned listener on <ip>:10250 forwards apiserver->kubelet connections
// through a data channel created by the JS side.
func (c *cluster) prepareJoin(open js.Value) (map[string]any, error) {
	nodesMu.Lock()
	n := nextNode
	nextNode++
	nodesMu.Unlock()
	if n > 250 {
		return nil, errors.New("too many nodes")
	}
	name := fmt.Sprintf("node-%d", n-1)
	ip := fmt.Sprintf("10.0.0.%d", n)

	id, secret := randToken(6), randToken(16)
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: bootstrap-token-%s
  namespace: kube-system
type: bootstrap.kubernetes.io/token
stringData:
  token-id: %s
  token-secret: %s
  expiration: %s
  usage-bootstrap-authentication: "true"
  usage-bootstrap-signing: "true"
  auth-extra-groups: system:bootstrappers:tabernetes:default-node-token
`, id, id, secret, time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339))
	if code := runKubectl(context.Background(), []string{"apply", "-f", "-"}, strings.NewReader(manifest), os.Stderr, os.Stderr); code != 0 {
		return nil, fmt.Errorf("creating bootstrap token failed (exit %d)", code)
	}

	ln, err := net.Listen("tcp", ip+":10250")
	if err != nil {
		return nil, err
	}
	jn := &joinedNode{name: name, ip: ip, open: open, ln: ln}
	nodesMu.Lock()
	nodes[name] = jn
	nodesMu.Unlock()
	go jn.serve()

	return map[string]any{
		"name":   name,
		"ip":     ip,
		"token":  id + "." + secret,
		"ca":     string(c.ca.caPEM),
		"caHash": c.ca.caHash(),
	}, nil
}

func (jn *joinedNode) serve() {
	for {
		tcp, err := jn.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			dc, err := awaitPromise(ctx, jn.open.Invoke("kubelet:10250"))
			if err != nil {
				fmt.Fprintln(os.Stderr, "tabernetes: opening kubelet channel:", err)
				tcp.Close()
				return
			}
			splice(newJSConn(dc), tcp)
		}()
	}
}

func awaitPromise(ctx context.Context, p js.Value) (js.Value, error) {
	type result struct {
		v   js.Value
		err error
	}
	ch := make(chan result, 1)
	var then, catch js.Func
	then = js.FuncOf(func(_ js.Value, a []js.Value) any {
		ch <- result{v: a[0]}
		then.Release()
		catch.Release()
		return nil
	})
	catch = js.FuncOf(func(_ js.Value, a []js.Value) any {
		ch <- result{err: errors.New(a[0].Call("toString").String())}
		then.Release()
		catch.Release()
		return nil
	})
	p.Call("then", then, catch)
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return js.Undefined(), ctx.Err()
	}
}
