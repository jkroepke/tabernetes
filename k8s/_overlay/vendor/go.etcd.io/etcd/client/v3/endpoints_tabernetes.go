package clientv3

// WithEndpoints sets the endpoints of a client from NewCtxClient, which has
// no connection and no resolver. kube-apiserver asks Status of each endpoint
// to learn the version of etcd (RequestWatchProgress).
func WithEndpoints(eps ...string) Option {
	return func(c *Client) {
		c.endpoints = eps
	}
}
