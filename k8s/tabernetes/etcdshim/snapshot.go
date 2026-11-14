package etcdshim

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
	"google.golang.org/protobuf/proto"
)

// A snapshot is the newest revision of every key plus the leases: enough
// for kube-apiserver to start again where it was. The history is not in
// it. The restored store starts compacted at the revision of the snapshot,
// so a client that watches from an older revision lists again, as it does
// after a compaction of a real etcd.
//
// Format: "tbs1", then uvarints: revision, next lease ID, number of leases,
// per lease ID and TTL, number of keys, per key the length and the
// marshalled mvccpb.KeyValue.
var magic = []byte("tbs1")

// Options of ServeWith.
type Options struct {
	// Snapshot to start from, nil for an empty store.
	Snapshot []byte
	// Save gets a snapshot every Interval while the store changes.
	Save     func([]byte)
	Interval time.Duration
	// Ready, if set, holds the saves back until it is closed: a snapshot of a
	// cluster that is still booting would restore half a cluster.
	Ready <-chan struct{}
	// Volatile are key prefixes whose changes alone are no reason to save:
	// heartbeats, which a restored cluster writes again anyway. The keys are
	// in the snapshot like all others.
	Volatile []string
}

// ServeWith starts the shim like Serve, from a snapshot and saving one now
// and then.
func ServeWith(l net.Listener, o Options) error {
	s := newStore()
	s.volatile = o.Volatile
	if o.Snapshot != nil {
		if err := s.restore(o.Snapshot); err != nil {
			return err
		}
	}
	if o.Save != nil {
		if o.Interval <= 0 {
			o.Interval = 2 * time.Second
		}
		go s.saveLoop(o.Save, o.Interval, o.Ready)
	}
	return serve(l, s)
}

func (s *store) saveLoop(save func([]byte), every time.Duration, ready <-chan struct{}) {
	if ready != nil {
		<-ready
	}
	// The first snapshot goes out right away: what is there now was never
	// saved, or was restored from a snapshot of another time.
	saved := int64(-1)
	var cache map[*mvccpb.KeyValue][]byte
	for {
		s.mu.Lock()
		var st *snapState
		if s.durable != saved {
			saved = s.durable
			st = s.snapState()
		}
		s.mu.Unlock()
		if st != nil {
			var b []byte
			b, cache = st.encode(cache)
			save(b)
		}
		time.Sleep(every)
	}
}

// snapState is what a snapshot holds, taken from the store under its lock.
// The values are not copied: a record never changes once it is in the store,
// a write adds a new one.
type snapState struct {
	rev, nextLease int64
	leases         [][2]int64 // ID, TTL
	kvs            []*mvccpb.KeyValue
}

// snapState must be called with mu held. It is cheap: the expensive part,
// encode, runs without the lock, so the apiserver can write meanwhile.
func (s *store) snapState() *snapState {
	st := &snapState{rev: s.rev, nextLease: s.nextLease, kvs: make([]*mvccpb.KeyValue, 0, len(s.sorted))}
	for id, l := range s.leases {
		st.leases = append(st.leases, [2]int64{id, l.ttl})
	}
	sort.Slice(st.leases, func(i, j int) bool { return st.leases[i][0] < st.leases[j][0] })
	for _, k := range s.sorted {
		if kv := s.current(k); kv != nil {
			st.kvs = append(st.kvs, kv)
		}
	}
	return st
}

// encode returns the snapshot and the marshalled records in it. The next
// encode gets these back as old: most records did not change since the last
// snapshot, and marshalling all of them again each second cost the most.
// old may be nil.
func (st *snapState) encode(old map[*mvccpb.KeyValue][]byte) ([]byte, map[*mvccpb.KeyValue][]byte) {
	ms := make([][]byte, 0, len(st.kvs))
	cache := make(map[*mvccpb.KeyValue][]byte, len(st.kvs))
	size := len(magic) + 3*binary.MaxVarintLen64 + len(st.leases)*2*binary.MaxVarintLen64
	for _, kv := range st.kvs {
		m, ok := old[kv]
		if !ok {
			var err error
			if m, err = proto.Marshal(kv); err != nil {
				continue
			}
		}
		cache[kv] = m
		ms = append(ms, m)
		size += binary.MaxVarintLen64 + len(m)
	}
	b := make([]byte, 0, size)
	b = append(b, magic...)
	b = binary.AppendUvarint(b, uint64(st.rev))
	b = binary.AppendUvarint(b, uint64(st.nextLease))
	b = binary.AppendUvarint(b, uint64(len(st.leases)))
	for _, l := range st.leases {
		b = binary.AppendUvarint(b, uint64(l[0]))
		b = binary.AppendUvarint(b, uint64(l[1]))
	}
	b = binary.AppendUvarint(b, uint64(len(ms)))
	for _, m := range ms {
		b = binary.AppendUvarint(b, uint64(len(m)))
		b = append(b, m...)
	}
	return b, cache
}

// snapshot returns the current state of the store.
func (s *store) snapshot() []byte {
	s.mu.Lock()
	st := s.snapState()
	s.mu.Unlock()
	b, _ := st.encode(nil)
	return b
}

// Without returns the snapshot without the given keys.
func Without(snapshot []byte, keys ...string) ([]byte, error) {
	s := emptyStore()
	if err := s.restore(snapshot); err != nil {
		return nil, err
	}
	s.mu.Lock()
	st := s.snapState()
	s.mu.Unlock()
	kvs := st.kvs[:0]
	for _, kv := range st.kvs {
		if !slices.Contains(keys, string(kv.Key)) {
			kvs = append(kvs, kv)
		}
	}
	st.kvs = kvs
	b, _ := st.encode(nil)
	return b, nil
}

var errSnapshot = errors.New("etcdshim: broken snapshot")

// Check tells whether ServeWith can start from the snapshot.
func Check(snapshot []byte) error {
	return emptyStore().restore(snapshot)
}

func (s *store) restore(b []byte) error {
	if len(b) < len(magic) || string(b[:len(magic)]) != string(magic) {
		return errSnapshot
	}
	b = b[len(magic):]
	next := func() uint64 {
		v, n := binary.Uvarint(b)
		if n <= 0 {
			b = nil
			return 0
		}
		b = b[n:]
		return v
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rev := int64(next())
	s.nextLease = int64(next())
	now := time.Now()
	for n := next(); n > 0 && b != nil; n-- {
		id, ttl := int64(next()), int64(next())
		// The clock stood still while nobody had the page open: every lease
		// gets its full TTL again.
		s.leases[id] = &lease{id: id, ttl: ttl, expiry: now.Add(time.Duration(ttl) * time.Second), keys: map[string]struct{}{}}
	}
	for n := next(); n > 0 && b != nil; n-- {
		size := next()
		if b == nil || uint64(len(b)) < size {
			return errSnapshot
		}
		kv := &mvccpb.KeyValue{}
		if err := proto.Unmarshal(b[:size], kv); err != nil {
			return fmt.Errorf("%w: %v", errSnapshot, err)
		}
		b = b[size:]
		k := string(kv.Key)
		s.keys[k] = []record{{rev: kv.ModRevision, kv: kv}}
		s.sorted = append(s.sorted, k)
		if kv.Lease != 0 {
			if l := s.leases[kv.Lease]; l != nil {
				l.keys[k] = struct{}{}
			}
		}
	}
	if b == nil {
		return errSnapshot
	}
	sort.Strings(s.sorted)
	s.rev = rev
	s.compactRev = rev
	return nil
}
