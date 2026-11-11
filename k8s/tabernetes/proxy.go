package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall/js"
	"time"
)

// proxyClient sends the requests of the proxy command (web/src/proxy.js) to
// localhost in the in-process network, where kubectl port-forward listens.
// Redirects go back to the browser tab, which keeps them inside the proxy.
var proxyClient = &http.Client{
	Transport:     localClient.Transport,
	Timeout:       proxyTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// proxyTimeout is below the timeout of the service worker (sw.js). A model in
// a tab may need minutes for an answer on the chat page of llm.
const proxyTimeout = 290 * time.Second

const proxyMaxBody = 64 << 20

// hopHeaders are not forwarded. Without Accept-Encoding the transport asks
// for gzip itself and unpacks it: a service worker response must not be
// compressed.
var hopHeaders = map[string]bool{
	"accept-encoding": true, "connection": true, "content-length": true, "host": true,
	"keep-alive": true, "te": true, "trailer": true, "transfer-encoding": true, "upgrade": true,
}

// proxyJS backs tabernetes.proxy(port, method, path, headers, body): it returns
// a Promise of {status, statusText, headers: [name, value][], body: Uint8Array}.
func proxyJS(a []js.Value) js.Value {
	port, method, path := a[0].Int(), a[1].String(), a[2].String()
	header := http.Header{}
	for i := range a[3].Length() {
		kv := a[3].Index(i)
		if k := kv.Index(0).String(); !hopHeaders[strings.ToLower(k)] {
			header.Add(k, kv.Index(1).String())
		}
	}
	var body []byte
	if len(a) > 4 && a[4].Truthy() {
		body = make([]byte, a[4].Length())
		js.CopyBytesToGo(body, a[4])
	}
	return promise(func() (any, error) {
		req, err := http.NewRequest(method, fmt.Sprintf("http://localhost:%d%s", port, path), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header = header
		resp, err := proxyClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("nothing answers on localhost:%d (kubectl port-forward ... %d:<pod port> &): %v", port, port, err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, proxyMaxBody+1))
		if err != nil {
			return nil, err
		}
		if len(b) > proxyMaxBody {
			return nil, fmt.Errorf("response larger than %d MiB", proxyMaxBody>>20)
		}
		headers := []any{}
		for k, vs := range resp.Header {
			if hopHeaders[strings.ToLower(k)] {
				continue
			}
			for _, v := range vs {
				headers = append(headers, []any{k, v})
			}
		}
		arr := js.Global().Get("Uint8Array").New(len(b))
		js.CopyBytesToJS(arr, b)
		statusText, _ := strings.CutPrefix(resp.Status, fmt.Sprintf("%d ", resp.StatusCode))
		return map[string]any{
			"status":     resp.StatusCode,
			"statusText": statusText,
			"headers":    headers,
			"body":       arr,
		}, nil
	})
}
