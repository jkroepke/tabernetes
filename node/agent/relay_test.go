package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNtfy is enough of an ntfy server for the relay: POST /<topic> and a
// streamed GET /<topic>/json.
type fakeNtfy struct {
	mu   sync.Mutex
	subs map[string][]chan string
	msgs map[string][]string
}

func (f *fakeNtfy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodPost && len(parts) == 1:
		b, _ := io.ReadAll(r.Body)
		line, _ := json.Marshal(map[string]string{"event": "message", "topic": parts[0], "message": string(b)})
		f.mu.Lock()
		f.msgs[parts[0]] = append(f.msgs[parts[0]], string(line))
		for _, c := range f.subs[parts[0]] {
			c <- string(line)
		}
		f.mu.Unlock()
	case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "json":
		c := make(chan string, 16)
		f.mu.Lock()
		f.subs[parts[0]] = append(f.subs[parts[0]], c)
		old := f.msgs[parts[0]]
		f.mu.Unlock()
		fmt.Fprintln(w, `{"event":"open"}`)
		for _, l := range old { // since=30s
			fmt.Fprintln(w, l)
		}
		w.(http.Flusher).Flush()
		for {
			select {
			case l := <-c:
				fmt.Fprintln(w, l)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	default:
		http.NotFound(w, r)
	}
}

func TestRelay(t *testing.T) {
	f := &fakeNtfy{subs: map[string][]chan string{}, msgs: map[string][]string{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	key := "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8" // 32 bytes
	node, err := newRelay(srv.URL, "tabernetes-node", key)
	if err != nil {
		t.Fatal(err)
	}
	tab, _ := newRelay(srv.URL, "tabernetes-reply", key)
	other, _ := newRelay(srv.URL, "tabernetes-reply", "Hx4dHBsaGRgXFhUUExIREA8ODQwLCgkIBwYFBAMCAQA")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// An offer sent before the node subscribed still arrives (since=30s).
	send := func(r *relay, m relayMsg) {
		msg, err := r.seal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.publish(ctx, "tabernetes-node", msg); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UnixMilli()
	send(tab, relayMsg{SDP: "offer-1", Reply: "tabernetes-r1", Time: now})
	send(other, relayMsg{SDP: "wrong key", Reply: "tabernetes-r2", Time: now})
	send(tab, relayMsg{SDP: "too old", Reply: "tabernetes-r3", Time: now - 3*time.Minute.Milliseconds()})
	send(tab, relayMsg{SDP: "replay", Reply: "tabernetes-r1", Time: now})

	answered := make(chan string, 8)
	go node.serve(ctx, func(sdp string) (string, error) {
		answered <- sdp
		return "answer to " + sdp, nil
	})
	select {
	case got := <-answered:
		if got != "offer-1" {
			t.Fatalf("answered %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no offer")
	}
	select {
	case got := <-answered:
		t.Fatalf("answered %q too", got)
	case <-time.After(300 * time.Millisecond):
	}
	// The answer is on the reply topic, sealed with the key.
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		msgs := f.msgs["tabernetes-r1"]
		f.mu.Unlock()
		if len(msgs) > 0 {
			var ev struct{ Message string }
			json.Unmarshal([]byte(msgs[0]), &ev)
			var m relayMsg
			if err := tab.open(ev.Message, &m); err != nil || m.SDP != "answer to offer-1" {
				t.Fatalf("answer %+v, %v", m, err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no answer")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
