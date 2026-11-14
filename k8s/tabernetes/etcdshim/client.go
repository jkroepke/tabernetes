package etcdshim

import (
	"context"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/kubernetes"
	"go.etcd.io/etcd/server/v3/proxy/grpcproxy/adapter"
	"google.golang.org/protobuf/proto"
)

// Client returns an etcd client that calls the shim directly, like
// v3client of an embedded etcd: no gRPC, no HTTP/2 framing, no protobuf
// encoding. kube-apiserver made one gRPC connection per resource, and the
// gRPC traffic was ~0.5 s of CPU of a boot in the browser.
//
// The answers hold the records of the store itself; they never change once
// they are in the store, and the client only reads them. Writes are copied,
// so a buffer of the caller cannot change a record later.
func (srv *Server) Client() *kubernetes.Client {
	c := clientv3.NewCtxClient(context.Background(), clientv3.WithEndpoints(srv.url))
	c.KV = clientv3.NewKVFromKVClient(adapter.KvServerToKvClient(copyWrites{srv}), c)
	c.Lease = clientv3.NewLeaseFromLeaseClient(adapter.LeaseServerToLeaseClient(srv), c, time.Second)
	c.Watcher = clientv3.NewWatchFromWatchClient(adapter.WatchServerToWatchClient(srv), c)
	c.Maintenance = clientv3.NewMaintenanceFromMaintenanceClient(adapter.MaintenanceServerToMaintenanceClient(srv), c)
	c.Cluster = clientv3.NewClusterFromClusterClient(adapter.ClusterServerToClusterClient(srv), c)
	kc := &kubernetes.Client{Client: c}
	kc.Kubernetes = kc
	return kc
}

// copyWrites copies the requests that put values into the store.
type copyWrites struct{ *Server }

func (w copyWrites) Put(ctx context.Context, r *pb.PutRequest) (*pb.PutResponse, error) {
	return w.Server.Put(ctx, proto.Clone(r).(*pb.PutRequest))
}

func (w copyWrites) Txn(ctx context.Context, r *pb.TxnRequest) (*pb.TxnResponse, error) {
	return w.Server.Txn(ctx, proto.Clone(r).(*pb.TxnRequest))
}
