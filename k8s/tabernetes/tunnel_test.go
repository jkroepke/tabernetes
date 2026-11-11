package main

import (
	"io"
	"syscall/js"
	"testing"
)

func TestDCConnClose(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remote bool
	}{
		{name: "local"},
		{name: "remote", remote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dc := js.Global().Get("Object").New()
			dc.Set("readyState", "open")
			closes := 0
			closeChannel := js.FuncOf(func(js.Value, []js.Value) any {
				closes++
				dc.Set("readyState", "closed")
				if onClose := dc.Get("onclose"); onClose.Type() == js.TypeFunction {
					onClose.Invoke()
				}
				return nil
			})
			defer closeChannel.Release()
			dc.Set("close", closeChannel)
			c := newDCConn(dc)
			if tc.remote {
				dc.Get("onclose").Invoke()
			}
			c.Close()
			c.Close()
			if closes != 1 {
				t.Fatalf("closed the data channel %d times", closes)
			}
			if !dc.Get("onmessage").IsNull() || !dc.Get("onclose").IsNull() {
				t.Fatal("closed data channel still has callbacks")
			}
			if n, err := c.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Fatalf("read after close: %d, %v", n, err)
			}
		})
	}
}
