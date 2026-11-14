package factory

import (
	"go.etcd.io/etcd/client/v3/kubernetes"

	"k8s.io/apiserver/pkg/storage/storagebackend"
)

// InProcessClient, if set, makes the etcd clients of the storage instead of
// a gRPC connection: tabernetes runs its etcd in the same process, and gRPC
// over the in-memory network (HTTP/2 framing, protobuf both ways, one
// connection per resource) was ~0.5 s of the boot.
var InProcessClient func(c storagebackend.TransportConfig) (*kubernetes.Client, error)
