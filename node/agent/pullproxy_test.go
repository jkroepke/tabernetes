package main

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPullProxy(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("layer", 100000))
	}))
	defer srv.Close()

	inside, err := newOutsideDialer()
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.FileConn(inside)
	inside.Close()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxy := servePullProxy(ctx, c.(*net.UnixConn))
	if proxy == "" {
		t.Fatal("no proxy")
	}
	pu, _ := url.Parse(proxy)
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(pu)
	tr.TLSClientConfig = &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs, ServerName: "example.com"}
	cl := &http.Client{Transport: tr}
	for i := 0; i < 5; i++ {
		resp, err := cl.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || len(b) != 500000 {
			t.Fatalf("got %d bytes, %v", len(b), err)
		}
	}

	// A failed dial outside comes back as an error.
	if _, err := dialOutside(c.(*net.UnixConn), "127.0.0.1:1"); err == nil {
		t.Fatal("dial to a closed port worked")
	}

	// Plain HTTP requests are refused.
	pc, err := net.Dial("tcp", pu.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	_, _ = io.WriteString(pc, "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
	b := make([]byte, 64)
	n, _ := pc.Read(b)
	if !strings.HasPrefix(string(b[:n]), "HTTP/1.1 405") {
		t.Fatalf("got %q", b[:n])
	}
}
