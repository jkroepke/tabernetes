package etcdshim

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

const (
	clusterID = 0x1
	memberID  = 0x1
)

type Server struct {
	pb.UnimplementedKVServer
	pb.UnimplementedWatchServer
	pb.UnimplementedLeaseServer
	pb.UnimplementedMaintenanceServer
	pb.UnimplementedClusterServer
	s   *store
	url string
}

// Serve starts the shim on the given listener.
func Serve(l net.Listener) error {
	srv := &Server{s: newStore(), url: "http://" + l.Addr().String()}
	// The etcd client pings every 30s. gRPC allows one ping per 5 minutes by
	// default and answers more with GOAWAY "too_many_pings". Same as etcd.
	g := grpc.NewServer(grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
		MinTime:             5 * time.Second,
		PermitWithoutStream: true,
	}))
	pb.RegisterKVServer(g, srv)
	pb.RegisterWatchServer(g, srv)
	pb.RegisterLeaseServer(g, srv)
	pb.RegisterMaintenanceServer(g, srv)
	pb.RegisterClusterServer(g, srv)
	return g.Serve(l)
}

func (srv *Server) header() *pb.ResponseHeader {
	return &pb.ResponseHeader{ClusterId: clusterID, MemberId: memberID, Revision: srv.s.rev, RaftTerm: 1}
}

// ---- KV ----

func (srv *Server) rangeLocked(r *pb.RangeRequest) (*pb.RangeResponse, error) {
	s := srv.s
	rev := r.Revision
	if rev <= 0 {
		rev = s.rev
	}
	if rev > s.rev {
		return nil, rpctypes.ErrGRPCFutureRev
	}
	if rev < s.compactRev {
		return nil, rpctypes.ErrGRPCCompacted
	}
	resp := &pb.RangeResponse{Header: srv.header()}
	for _, k := range s.rangeKeys(r.Key, r.RangeEnd) {
		kv := s.getAt(k, rev)
		if kv == nil {
			continue
		}
		if r.MinModRevision > 0 && kv.ModRevision < r.MinModRevision ||
			r.MaxModRevision > 0 && kv.ModRevision > r.MaxModRevision ||
			r.MinCreateRevision > 0 && kv.CreateRevision < r.MinCreateRevision ||
			r.MaxCreateRevision > 0 && kv.CreateRevision > r.MaxCreateRevision {
			continue
		}
		resp.Count++
		if r.CountOnly {
			continue
		}
		if r.Limit > 0 && int64(len(resp.Kvs)) >= r.Limit {
			resp.More = true
			continue
		}
		if r.KeysOnly {
			c := *kv
			c.Value = nil
			kv = &c
		}
		resp.Kvs = append(resp.Kvs, kv)
	}
	if r.SortOrder == pb.RangeRequest_DESCEND {
		for i, j := 0, len(resp.Kvs)-1; i < j; i, j = i+1, j-1 {
			resp.Kvs[i], resp.Kvs[j] = resp.Kvs[j], resp.Kvs[i]
		}
	}
	return resp, nil
}

func (srv *Server) Range(ctx context.Context, r *pb.RangeRequest) (*pb.RangeResponse, error) {
	srv.s.mu.Lock()
	defer srv.s.mu.Unlock()
	return srv.rangeLocked(r)
}

func (srv *Server) RangeStream(r *pb.RangeRequest, stream grpc.ServerStreamingServer[pb.RangeStreamResponse]) error {
	resp, err := srv.Range(stream.Context(), r)
	if err != nil {
		return err
	}
	return stream.Send(&pb.RangeStreamResponse{RangeResponse: resp})
}

func (srv *Server) putLocked(rev int64, r *pb.PutRequest) (*pb.PutResponse, error) {
	s := srv.s
	val, leaseID := r.Value, r.Lease
	cur := s.current(string(r.Key))
	if r.IgnoreValue || r.IgnoreLease {
		if cur == nil {
			return nil, rpctypes.ErrGRPCKeyNotFound
		}
		if r.IgnoreValue {
			val = cur.Value
		}
		if r.IgnoreLease {
			leaseID = cur.Lease
		}
	}
	if leaseID != 0 && s.leases[leaseID] == nil {
		return nil, rpctypes.ErrGRPCLeaseNotFound
	}
	prev := s.put(rev, r.Key, val, leaseID)
	resp := &pb.PutResponse{}
	if r.PrevKv {
		resp.PrevKv = prev
	}
	return resp, nil
}

func (srv *Server) deleteLocked(rev int64, r *pb.DeleteRangeRequest) *pb.DeleteRangeResponse {
	s := srv.s
	resp := &pb.DeleteRangeResponse{}
	for _, k := range s.rangeKeys(r.Key, r.RangeEnd) {
		if prev := s.del(rev, k); prev != nil {
			resp.Deleted++
			if r.PrevKv {
				resp.PrevKvs = append(resp.PrevKvs, prev)
			}
		}
	}
	return resp
}

func (srv *Server) Put(ctx context.Context, r *pb.PutRequest) (*pb.PutResponse, error) {
	s := srv.s
	s.mu.Lock()
	defer s.mu.Unlock()
	resp, err := srv.putLocked(s.rev+1, r)
	if err != nil {
		return nil, err
	}
	s.rev++
	s.wake()
	resp.Header = srv.header()
	return resp, nil
}

func (srv *Server) DeleteRange(ctx context.Context, r *pb.DeleteRangeRequest) (*pb.DeleteRangeResponse, error) {
	s := srv.s
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := srv.deleteLocked(s.rev+1, r)
	if resp.Deleted > 0 {
		s.rev++
		s.wake()
	}
	resp.Header = srv.header()
	return resp, nil
}

func compareOK(c *pb.Compare, kv *mvccpb.KeyValue) bool {
	var res int
	switch c.Target {
	case pb.Compare_VALUE:
		var v []byte
		if kv != nil {
			v = kv.Value
		}
		res = bytes.Compare(v, c.GetValue())
	default:
		var have, want int64
		if kv != nil {
			switch c.Target {
			case pb.Compare_VERSION:
				have = kv.Version
			case pb.Compare_CREATE:
				have = kv.CreateRevision
			case pb.Compare_MOD:
				have = kv.ModRevision
			case pb.Compare_LEASE:
				have = kv.Lease
			}
		}
		switch c.Target {
		case pb.Compare_VERSION:
			want = c.GetVersion()
		case pb.Compare_CREATE:
			want = c.GetCreateRevision()
		case pb.Compare_MOD:
			want = c.GetModRevision()
		case pb.Compare_LEASE:
			want = c.GetLease()
		}
		switch {
		case have < want:
			res = -1
		case have > want:
			res = 1
		}
	}
	switch c.Result {
	case pb.Compare_EQUAL:
		return res == 0
	case pb.Compare_NOT_EQUAL:
		return res != 0
	case pb.Compare_GREATER:
		return res > 0
	case pb.Compare_LESS:
		return res < 0
	}
	return false
}

func (srv *Server) evalCompares(cmps []*pb.Compare) bool {
	for _, c := range cmps {
		keys := []string{string(c.Key)}
		if len(c.RangeEnd) > 0 {
			keys = srv.s.rangeKeys(c.Key, c.RangeEnd)
		}
		for _, k := range keys {
			if !compareOK(c, srv.s.current(k)) {
				return false
			}
		}
	}
	return true
}

func (srv *Server) txnLocked(rev int64, r *pb.TxnRequest, wrote *bool) (*pb.TxnResponse, error) {
	ok := srv.evalCompares(r.Compare)
	ops := r.Failure
	if ok {
		ops = r.Success
	}
	resp := &pb.TxnResponse{Succeeded: ok}
	for _, op := range ops {
		switch o := op.Request.(type) {
		case *pb.RequestOp_RequestRange:
			rr, err := srv.rangeLocked(o.RequestRange)
			if err != nil {
				return nil, err
			}
			rr.Header = nil
			resp.Responses = append(resp.Responses, &pb.ResponseOp{Response: &pb.ResponseOp_ResponseRange{ResponseRange: rr}})
		case *pb.RequestOp_RequestPut:
			pr, err := srv.putLocked(rev, o.RequestPut)
			if err != nil {
				return nil, err
			}
			*wrote = true
			resp.Responses = append(resp.Responses, &pb.ResponseOp{Response: &pb.ResponseOp_ResponsePut{ResponsePut: pr}})
		case *pb.RequestOp_RequestDeleteRange:
			dr := srv.deleteLocked(rev, o.RequestDeleteRange)
			if dr.Deleted > 0 {
				*wrote = true
			}
			resp.Responses = append(resp.Responses, &pb.ResponseOp{Response: &pb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: dr}})
		case *pb.RequestOp_RequestTxn:
			tr, err := srv.txnLocked(rev, o.RequestTxn, wrote)
			if err != nil {
				return nil, err
			}
			resp.Responses = append(resp.Responses, &pb.ResponseOp{Response: &pb.ResponseOp_ResponseTxn{ResponseTxn: tr}})
		}
	}
	return resp, nil
}

func (srv *Server) Txn(ctx context.Context, r *pb.TxnRequest) (*pb.TxnResponse, error) {
	s := srv.s
	s.mu.Lock()
	defer s.mu.Unlock()
	wrote := false
	resp, err := srv.txnLocked(s.rev+1, r, &wrote)
	if err != nil {
		return nil, err
	}
	if wrote {
		s.rev++
		s.wake()
	}
	resp.Header = srv.header()
	return resp, nil
}

func (srv *Server) Compact(ctx context.Context, r *pb.CompactionRequest) (*pb.CompactionResponse, error) {
	s := srv.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Revision > s.rev {
		return nil, rpctypes.ErrGRPCFutureRev
	}
	if r.Revision <= s.compactRev {
		return nil, rpctypes.ErrGRPCCompacted
	}
	s.compact(r.Revision)
	s.wake()
	return &pb.CompactionResponse{Header: srv.header()}, nil
}

// ---- Watch ----

type watcher struct {
	id       int64
	key, end []byte
	next     int64
	prevKV   bool
	progress bool
	noPut    bool
	noDelete bool
}

func (srv *Server) Watch(stream grpc.BidiStreamingServer[pb.WatchRequest, pb.WatchResponse]) error {
	s := srv.s
	ctx := stream.Context()
	var (
		mu              sync.Mutex
		watchers        = map[int64]*watcher{}
		pending         []*pb.WatchResponse // control responses (created/canceled)
		progressPending bool
		nextID          int64
		kick            = make(chan struct{}, 1)
	)
	poke := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	recvErr := make(chan error, 1)
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			mu.Lock()
			switch r := req.RequestUnion.(type) {
			case *pb.WatchRequest_CreateRequest:
				c := r.CreateRequest
				id := c.WatchId
				if id == 0 {
					id = nextID
				}
				nextID = id + 1
				s.mu.Lock()
				start := c.StartRevision
				if start <= 0 {
					start = s.rev + 1
				}
				hdr := srv.header()
				compacted := start <= s.compactRev
				s.mu.Unlock()
				pending = append(pending, &pb.WatchResponse{Header: hdr, WatchId: id, Created: true})
				if compacted {
					pending = append(pending, &pb.WatchResponse{Header: hdr, WatchId: id, Canceled: true, CompactRevision: s.compactRev, CancelReason: rpctypes.ErrCompacted.Error()})
					break
				}
				w := &watcher{id: id, key: c.Key, end: c.RangeEnd, next: start, prevKV: c.PrevKv, progress: c.ProgressNotify}
				for _, f := range c.Filters {
					switch f {
					case pb.WatchCreateRequest_NOPUT:
						w.noPut = true
					case pb.WatchCreateRequest_NODELETE:
						w.noDelete = true
					}
				}
				watchers[id] = w
			case *pb.WatchRequest_CancelRequest:
				id := r.CancelRequest.WatchId
				if _, ok := watchers[id]; ok {
					delete(watchers, id)
					pending = append(pending, &pb.WatchResponse{Header: srv.header(), WatchId: id, Canceled: true})
				}
			case *pb.WatchRequest_ProgressRequest:
				progressPending = true
			}
			mu.Unlock()
			poke()
		}
	}()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	periodic := false
	for {
		mu.Lock()
		out := pending
		pending = nil
		s.mu.Lock()
		rev := s.rev
		notify := s.notify
		var compactedWatchers []int64
		for _, w := range watchers {
			if w.next <= s.compactRev {
				compactedWatchers = append(compactedWatchers, w.id)
				out = append(out, &pb.WatchResponse{Header: srv.header(), WatchId: w.id, Canceled: true, CompactRevision: s.compactRev, CancelReason: rpctypes.ErrCompacted.Error()})
				continue
			}
			var evs []*mvccpb.Event
			for _, e := range s.eventsSince(w.next) {
				if !inRange(e.kv.Key, w.key, w.end) {
					continue
				}
				if e.typ == mvccpb.PUT && w.noPut || e.typ == mvccpb.DELETE && w.noDelete {
					continue
				}
				ev := &mvccpb.Event{Type: e.typ, Kv: e.kv}
				if w.prevKV {
					ev.PrevKv = e.prevKV
				}
				evs = append(evs, ev)
			}
			w.next = rev + 1
			if len(evs) > 0 {
				out = append(out, &pb.WatchResponse{Header: srv.header(), WatchId: w.id, Events: evs})
			} else if periodic && w.progress {
				out = append(out, &pb.WatchResponse{Header: srv.header(), WatchId: w.id})
			}
		}
		s.mu.Unlock()
		for _, id := range compactedWatchers {
			delete(watchers, id)
		}
		if progressPending {
			progressPending = false
			out = append(out, &pb.WatchResponse{Header: &pb.ResponseHeader{ClusterId: clusterID, MemberId: memberID, Revision: rev, RaftTerm: 1}, WatchId: -1})
		}
		periodic = false
		mu.Unlock()

		for _, r := range out {
			if err := stream.Send(r); err != nil {
				return err
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case err := <-recvErr:
			if err == io.EOF {
				return nil
			}
			return err
		case <-notify:
		case <-kick:
		case <-ticker.C:
			periodic = true
		}
	}
}

// ---- Lease ----

func (srv *Server) LeaseGrant(ctx context.Context, r *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error) {
	s := srv.s
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.ID
	if id == 0 {
		s.nextLease++
		id = s.nextLease
	}
	if s.leases[id] != nil {
		return nil, rpctypes.ErrGRPCLeaseExist
	}
	ttl := r.TTL
	if ttl < 5 {
		ttl = 5
	}
	s.leases[id] = &lease{id: id, ttl: ttl, expiry: time.Now().Add(time.Duration(ttl) * time.Second), keys: map[string]struct{}{}}
	return &pb.LeaseGrantResponse{Header: srv.header(), ID: id, TTL: ttl}, nil
}

func (srv *Server) LeaseRevoke(ctx context.Context, r *pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error) {
	s := srv.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leases[r.ID] == nil {
		return nil, rpctypes.ErrGRPCLeaseNotFound
	}
	if s.revokeLocked(r.ID) {
		s.wake()
	}
	return &pb.LeaseRevokeResponse{Header: srv.header()}, nil
}

func (srv *Server) LeaseKeepAlive(stream grpc.BidiStreamingServer[pb.LeaseKeepAliveRequest, pb.LeaseKeepAliveResponse]) error {
	s := srv.s
	for {
		req, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		s.mu.Lock()
		resp := &pb.LeaseKeepAliveResponse{Header: srv.header(), ID: req.ID}
		if l := s.leases[req.ID]; l != nil {
			l.expiry = time.Now().Add(time.Duration(l.ttl) * time.Second)
			resp.TTL = l.ttl
		}
		s.mu.Unlock()
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

func (srv *Server) LeaseTimeToLive(ctx context.Context, r *pb.LeaseTimeToLiveRequest) (*pb.LeaseTimeToLiveResponse, error) {
	s := srv.s
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.leases[r.ID]
	if l == nil {
		return &pb.LeaseTimeToLiveResponse{Header: srv.header(), ID: r.ID, TTL: -1}, nil
	}
	resp := &pb.LeaseTimeToLiveResponse{Header: srv.header(), ID: r.ID, TTL: int64(time.Until(l.expiry).Seconds()), GrantedTTL: l.ttl}
	if r.Keys {
		for k := range l.keys {
			resp.Keys = append(resp.Keys, []byte(k))
		}
	}
	return resp, nil
}

func (srv *Server) LeaseLeases(ctx context.Context, r *pb.LeaseLeasesRequest) (*pb.LeaseLeasesResponse, error) {
	s := srv.s
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &pb.LeaseLeasesResponse{Header: srv.header()}
	for id := range s.leases {
		resp.Leases = append(resp.Leases, &pb.LeaseStatus{ID: id})
	}
	return resp, nil
}

// ---- Maintenance / Cluster ----

func (srv *Server) Status(ctx context.Context, r *pb.StatusRequest) (*pb.StatusResponse, error) {
	s := srv.s
	s.mu.Lock()
	defer s.mu.Unlock()
	return &pb.StatusResponse{
		Header: srv.header(), Version: "3.7.0", StorageVersion: "3.7.0", DbSize: int64(len(s.log)) * 1024, DbSizeInUse: int64(len(s.log)) * 1024,
		Leader: memberID, RaftIndex: uint64(s.rev), RaftTerm: 1, RaftAppliedIndex: uint64(s.rev),
	}, nil
}

func (srv *Server) Alarm(ctx context.Context, r *pb.AlarmRequest) (*pb.AlarmResponse, error) {
	srv.s.mu.Lock()
	defer srv.s.mu.Unlock()
	return &pb.AlarmResponse{Header: srv.header()}, nil
}

func (srv *Server) Defragment(ctx context.Context, r *pb.DefragmentRequest) (*pb.DefragmentResponse, error) {
	srv.s.mu.Lock()
	defer srv.s.mu.Unlock()
	return &pb.DefragmentResponse{Header: srv.header()}, nil
}

func (srv *Server) MemberList(ctx context.Context, r *pb.MemberListRequest) (*pb.MemberListResponse, error) {
	srv.s.mu.Lock()
	defer srv.s.mu.Unlock()
	return &pb.MemberListResponse{Header: srv.header(), Members: []*pb.Member{{ID: memberID, Name: "wasm", PeerURLs: []string{srv.url}, ClientURLs: []string{srv.url}}}}, nil
}
