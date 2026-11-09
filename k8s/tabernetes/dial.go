package main

import (
	"fmt"
	"net"
	"sync"
	"syscall/js"
)

// dialJS backs tabernetes.dial(port, onData, onClose): a TCP connection from
// the page to localhost:<port> of the in-process network, where kubectl
// port-forward listens. It returns a Promise of {send(Uint8Array), close()}.
// onData gets each chunk as a Uint8Array, onClose is called once at the end.
// The VNC viewer (web/src/vnc.js) speaks RFB over it.
func dialJS(a []js.Value) js.Value {
	port, onData, onClose := a[0].Int(), a[1], a[2]
	return promise(func() (any, error) {
		c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return nil, fmt.Errorf("nothing answers on localhost:%d: %v", port, err)
		}
		// send must not block the JS event loop, and the writes must keep
		// their order: one goroutine writes what send queues.
		out := make(chan []byte, 1024)
		var mu sync.Mutex
		closed := false
		shutdown := func() {
			mu.Lock()
			defer mu.Unlock()
			if !closed {
				closed = true
				c.Close()
				close(out)
			}
		}
		go func() {
			for b := range out {
				if _, err := c.Write(b); err != nil {
					c.Close() // the reader ends and calls shutdown
				}
			}
		}()
		go func() {
			buf := make([]byte, 64<<10)
			for {
				n, err := c.Read(buf)
				if n > 0 {
					arr := js.Global().Get("Uint8Array").New(n)
					js.CopyBytesToJS(arr, buf[:n])
					onData.Invoke(arr)
				}
				if err != nil {
					shutdown()
					onClose.Invoke()
					return
				}
			}
		}()
		// The funcs stay alive with the page: the viewer may call close twice.
		send := js.FuncOf(func(this js.Value, a []js.Value) any {
			b := make([]byte, a[0].Length())
			js.CopyBytesToGo(b, a[0])
			mu.Lock()
			defer mu.Unlock()
			if closed {
				return nil
			}
			select {
			case out <- b:
			default:
				// 1024 writes queued: the other side reads nothing.
				closed = true
				c.Close()
				close(out)
			}
			return nil
		})
		closeFn := js.FuncOf(func(js.Value, []js.Value) any {
			shutdown()
			return nil
		})
		conn := js.Global().Get("Object").New()
		conn.Set("send", send)
		conn.Set("close", closeFn)
		return conn, nil
	})
}
