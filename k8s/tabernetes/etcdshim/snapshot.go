package etcdshim

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
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
}

// ServeWith starts the shim like Serve, from a snapshot and saving one now
// and then.
func ServeWith(l net.Listener, o Options) error {
	s := newStore()
	if o.Snapshot != nil {
		if err := s.restore(o.Snapshot); err != nil {
			return err
		}
	}
	if o.Save != nil {
		if o.Interval <= 0 {
			o.Interval = 2 * time.Second
		}
		go s.saveLoop(o.Save, o.Interval)
	}
	return serve(l, s)
}

func (s *store) saveLoop(save func([]byte), every time.Duration) {
	s.mu.Lock()
	saved := s.rev
	s.mu.Unlock()
	for range time.Tick(every) {
		s.mu.Lock()
		if s.rev == saved {
			s.mu.Unlock()
			continue
		}
		saved = s.rev
		b := s.snapshotLocked()
		s.mu.Unlock()
		save(b)
	}
}

// Snapshot returns the current state of the store.
func (s *store) snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *store) snapshotLocked() []byte {
	b := append([]byte(nil), magic...)
	b = binary.AppendUvarint(b, uint64(s.rev))
	b = binary.AppendUvarint(b, uint64(s.nextLease))
	ids := make([]int64, 0, len(s.leases))
	for id := range s.leases {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	b = binary.AppendUvarint(b, uint64(len(ids)))
	for _, id := range ids {
		b = binary.AppendUvarint(b, uint64(id))
		b = binary.AppendUvarint(b, uint64(s.leases[id].ttl))
	}
	var kvs [][]byte
	for _, k := range s.sorted {
		kv := s.current(k)
		if kv == nil {
			continue
		}
		m, err := proto.Marshal(kv)
		if err != nil {
			continue
		}
		kvs = append(kvs, m)
	}
	b = binary.AppendUvarint(b, uint64(len(kvs)))
	for _, m := range kvs {
		b = binary.AppendUvarint(b, uint64(len(m)))
		b = append(b, m...)
	}
	return b
}

var errSnapshot = errors.New("etcdshim: broken snapshot")

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
