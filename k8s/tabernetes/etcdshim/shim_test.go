package etcdshim

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestWatch(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go Serve(l)
	c, err := clientv3.New(clientv3.Config{Endpoints: []string{l.Addr().String()}, DialTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := c.Put(ctx, "/a/x", "1")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("put rev %d", r.Header.Revision)
	w1 := c.Watch(ctx, "/a/", clientv3.WithPrefix(), clientv3.WithRev(r.Header.Revision+1), clientv3.WithPrevKV(), clientv3.WithProgressNotify())
	w2 := c.Watch(ctx, "/b/", clientv3.WithPrefix(), clientv3.WithPrevKV())
	time.Sleep(100 * time.Millisecond)
	c.Put(ctx, "/a/y", "2")
	c.Put(ctx, "/b/y", "2")
	for _, w := range []clientv3.WatchChan{w1, w2} {
		select {
		case resp := <-w:
			t.Logf("got %v err=%v", resp.Events, resp.Err())
			if len(resp.Events) != 1 {
				t.Fatalf("expected one event, got %d", len(resp.Events))
			}
		case <-ctx.Done():
			t.Fatal("timeout")
		}
	}
	if err := c.RequestProgress(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case resp := <-w1:
		t.Logf("progress: %v rev=%d", resp.IsProgressNotify(), resp.Header.Revision)
	case <-ctx.Done():
		t.Fatal("timeout")
	}
	tx, err := c.Txn(ctx).If(clientv3.Compare(clientv3.ModRevision("/a/x"), "=", r.Header.Revision)).Then(clientv3.OpPut("/a/x", "3")).Else(clientv3.OpGet("/a/x")).Commit()
	t.Logf("txn ok=%v err=%v", tx.Succeeded, err)
}

// A txn is all or nothing: one that fails writes nothing, also when it
// fails after a put.
func TestTxnAtomic(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go Serve(l)
	c, err := clientv3.New(clientv3.Config{Endpoints: []string{l.Addr().String()}, DialTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = c.Txn(ctx).Then(clientv3.OpPut("/x", "1"), clientv3.OpPut("/y", "2", clientv3.WithLease(999))).Commit()
	if err == nil {
		t.Fatal("a put with an unknown lease worked")
	}
	r1, err := c.Put(ctx, "/z", "3")
	if err != nil {
		t.Fatal(err)
	}
	if r, err := c.Get(ctx, "/", clientv3.WithPrefix()); err != nil || len(r.Kvs) != 1 {
		t.Fatalf("keys after the failed txn: %v, %v", r.Kvs, err)
	}
	// The compares of a nested txn see the store before the txn, as in etcd.
	tx, err := c.Txn(ctx).If(clientv3.Compare(clientv3.Version("/z"), "=", 1)).Then(
		clientv3.OpPut("/z", "4"),
		clientv3.OpTxn([]clientv3.Cmp{clientv3.Compare(clientv3.Version("/z"), "=", 1)}, []clientv3.Op{clientv3.OpPut("/n", "ok")}, nil),
	).Commit()
	if err != nil || !tx.Succeeded || tx.Header.Revision != r1.Header.Revision+1 {
		t.Fatalf("nested txn: %+v, %v", tx, err)
	}
	if r, _ := c.Get(ctx, "/n"); len(r.Kvs) != 1 {
		t.Fatal("the nested txn took the wrong branch")
	}
}

func TestSnapshot(t *testing.T) {
	s := newStore()
	s.mu.Lock()
	s.leases[7] = &lease{id: 7, ttl: 60, expiry: time.Now(), keys: map[string]struct{}{}}
	s.rev++
	s.put(s.rev, []byte("/a"), []byte("1"), 0)
	s.rev++
	s.put(s.rev, []byte("/b"), []byte("2"), 7)
	s.rev++
	s.put(s.rev, []byte("/a"), []byte("3"), 0)
	s.rev++
	s.del(s.rev, "/b")
	s.rev++
	s.put(s.rev, []byte("/c"), []byte("4"), 7)
	s.mu.Unlock()

	r := newStore()
	if err := r.restore(s.snapshot()); err != nil {
		t.Fatal(err)
	}
	if r.rev != s.rev || r.compactRev != s.rev {
		t.Fatalf("rev %d compact %d, want %d", r.rev, r.compactRev, s.rev)
	}
	if kv := r.current("/a"); kv == nil || string(kv.Value) != "3" || kv.Version != 2 || kv.CreateRevision != 2 {
		t.Fatalf("/a = %v", kv)
	}
	if r.current("/b") != nil {
		t.Fatal("/b was deleted")
	}
	if l := r.leases[7]; l == nil || len(l.keys) != 1 || time.Until(l.expiry) < 50*time.Second {
		t.Fatalf("lease 7 = %+v", l)
	}
	if got := r.rangeKeys([]byte("/"), []byte("0")); len(got) != 2 {
		t.Fatalf("keys %v", got)
	}
	// A snapshot from the records of the last one is the same as a new one.
	s.mu.Lock()
	_, cache := s.snapState().encode(nil)
	s.rev++
	s.put(s.rev, []byte("/a"), []byte("5"), 0)
	cached, _ := s.snapState().encode(cache)
	s.mu.Unlock()
	if !bytes.Equal(cached, s.snapshot()) {
		t.Fatal("the snapshot from the cache differs")
	}
	if err := r.restore([]byte("tbs1\x05")); err == nil {
		t.Fatal("a cut snapshot restored")
	}
	w, err := Without(s.snapshot(), "/c")
	if err != nil {
		t.Fatal(err)
	}
	r = newStore()
	if err := r.restore(w); err != nil {
		t.Fatal(err)
	}
	if r.current("/c") != nil || r.current("/a") == nil || r.rev != s.rev {
		t.Fatalf("Without: /c %v, /a %v, rev %d", r.current("/c"), r.current("/a"), r.rev)
	}
}

func TestSaveLoop(t *testing.T) {
	s := newStore()
	s.volatile = []string{"/registry/leases/"}
	saves := make(chan []byte, 16)
	ready := make(chan struct{})
	go s.saveLoop(func(b []byte) { saves <- b }, 10*time.Millisecond, ready)
	put := func(key string) {
		s.mu.Lock()
		s.rev++
		s.put(s.rev, []byte(key), []byte("x"), 0)
		s.mu.Unlock()
	}
	saved := func(want bool, what string) {
		t.Helper()
		select {
		case <-saves:
			if !want {
				t.Fatalf("saved %s", what)
			}
		case <-time.After(100 * time.Millisecond):
			if want {
				t.Fatalf("did not save %s", what)
			}
		}
	}
	put("/registry/pods/default/a")
	saved(false, "before the cluster was ready")
	close(ready)
	saved(true, "once the cluster was ready")
	saved(false, "again without a change")
	put("/registry/leases/kube-node-lease/tab-1")
	saved(false, "for a heartbeat")
	put("/registry/pods/default/b")
	saved(true, "a new pod")
	s.mu.Lock()
	s.rev++
	s.del(s.rev, "/registry/pods/default/a")
	s.mu.Unlock()
	saved(true, "a deleted pod")
}
