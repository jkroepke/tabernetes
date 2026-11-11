// Package etcdshim is a tiny, in-memory, single-node implementation of
// the etcd v3 gRPC API. A snapshot (snapshot.go) can carry it over a reload. It implements just enough of the API for
// kube-apiserver to run against it inside a WebAssembly sandbox.
package etcdshim

import (
	"bytes"
	"sort"
	"sync"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

type record struct {
	rev     int64
	kv      *mvccpb.KeyValue // nil == tombstone
	deleted bool
}

type event struct {
	rev    int64
	typ    mvccpb.Event_EventType
	kv     *mvccpb.KeyValue
	prevKV *mvccpb.KeyValue
}

type lease struct {
	id     int64
	ttl    int64
	expiry time.Time
	keys   map[string]struct{}
}

type store struct {
	mu         sync.Mutex
	rev        int64
	compactRev int64
	keys       map[string][]record
	sorted     []string // all keys ever written, sorted
	log        []event
	notify     chan struct{}
	leases     map[int64]*lease
	nextLease  int64
}

func newStore() *store {
	s := &store{
		rev:       1,
		keys:      map[string][]record{},
		notify:    make(chan struct{}),
		leases:    map[int64]*lease{},
		nextLease: 0x1000,
	}
	go s.expireLoop()
	return s
}

// wake must be called with mu held after a write.
func (s *store) wake() {
	close(s.notify)
	s.notify = make(chan struct{})
}

func (s *store) getAt(key string, rev int64) *mvccpb.KeyValue {
	recs := s.keys[key]
	i := sort.Search(len(recs), func(i int) bool { return recs[i].rev > rev }) - 1
	if i < 0 || recs[i].deleted {
		return nil
	}
	return recs[i].kv
}

func (s *store) current(key string) *mvccpb.KeyValue { return s.getAt(key, s.rev) }

// inRange reports whether key is within [start, end) using etcd semantics.
func inRange(key, start, end []byte) bool {
	if len(end) == 0 {
		return bytes.Equal(key, start)
	}
	if bytes.Compare(key, start) < 0 {
		return false
	}
	if len(end) == 1 && end[0] == 0 {
		return true
	}
	return bytes.Compare(key, end) < 0
}

func (s *store) rangeKeys(start, end []byte) []string {
	if len(end) == 0 {
		if _, ok := s.keys[string(start)]; ok {
			return []string{string(start)}
		}
		return nil
	}
	i := sort.SearchStrings(s.sorted, string(start))
	var out []string
	for ; i < len(s.sorted); i++ {
		k := s.sorted[i]
		if !inRange([]byte(k), start, end) {
			break
		}
		out = append(out, k)
	}
	return out
}

func (s *store) addKey(key string) {
	if _, ok := s.keys[key]; ok {
		return
	}
	i := sort.SearchStrings(s.sorted, key)
	s.sorted = append(s.sorted, "")
	copy(s.sorted[i+1:], s.sorted[i:])
	s.sorted[i] = key
}

// put writes key at revision rev (caller bumped s.rev).
func (s *store) put(rev int64, key, value []byte, leaseID int64) (prev *mvccpb.KeyValue) {
	k := string(key)
	prev = s.current(k)
	kv := &mvccpb.KeyValue{Key: key, Value: value, ModRevision: rev, Lease: leaseID}
	if prev != nil {
		kv.CreateRevision = prev.CreateRevision
		kv.Version = prev.Version + 1
		if prev.Lease != 0 && prev.Lease != leaseID {
			if l := s.leases[prev.Lease]; l != nil {
				delete(l.keys, k)
			}
		}
	} else {
		kv.CreateRevision = rev
		kv.Version = 1
	}
	if leaseID != 0 {
		if l := s.leases[leaseID]; l != nil {
			l.keys[k] = struct{}{}
		}
	}
	s.addKey(k)
	s.keys[k] = append(s.keys[k], record{rev: rev, kv: kv})
	s.log = append(s.log, event{rev: rev, typ: mvccpb.PUT, kv: kv, prevKV: prev})
	return prev
}

func (s *store) del(rev int64, key string) *mvccpb.KeyValue {
	prev := s.current(key)
	if prev == nil {
		return nil
	}
	if prev.Lease != 0 {
		if l := s.leases[prev.Lease]; l != nil {
			delete(l.keys, key)
		}
	}
	s.keys[key] = append(s.keys[key], record{rev: rev, deleted: true})
	s.log = append(s.log, event{rev: rev, typ: mvccpb.DELETE, kv: &mvccpb.KeyValue{Key: []byte(key), ModRevision: rev}, prevKV: prev})
	return prev
}

func (s *store) compact(rev int64) {
	if rev <= s.compactRev {
		return
	}
	s.compactRev = rev
	i := sort.Search(len(s.log), func(i int) bool { return s.log[i].rev > rev })
	s.log = append([]event(nil), s.log[i:]...)
	for k, recs := range s.keys {
		// keep the newest record <= rev plus everything after it
		j := sort.Search(len(recs), func(i int) bool { return recs[i].rev > rev }) - 1
		if j <= 0 {
			continue
		}
		recs = append([]record(nil), recs[j:]...)
		if len(recs) == 1 && recs[0].deleted {
			delete(s.keys, k)
			continue
		}
		s.keys[k] = recs
	}
	if len(s.sorted) != len(s.keys) {
		s.sorted = s.sorted[:0]
		for k := range s.keys {
			s.sorted = append(s.sorted, k)
		}
		sort.Strings(s.sorted)
	}
}

func (s *store) eventsSince(rev int64) []event {
	i := sort.Search(len(s.log), func(i int) bool { return s.log[i].rev >= rev })
	return s.log[i:]
}

func (s *store) expireLoop() {
	for range time.Tick(time.Second) {
		now := time.Now()
		s.mu.Lock()
		changed := false
		for id, l := range s.leases {
			if now.After(l.expiry) {
				if s.revokeLocked(id) {
					changed = true
				}
			}
		}
		if changed {
			s.wake()
		}
		s.mu.Unlock()
	}
}

func (s *store) revokeLocked(id int64) bool {
	l := s.leases[id]
	if l == nil {
		return false
	}
	delete(s.leases, id)
	if len(l.keys) == 0 {
		return false
	}
	s.rev++
	keys := make([]string, 0, len(l.keys))
	for k := range l.keys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s.del(s.rev, k)
	}
	return true
}
