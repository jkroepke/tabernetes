//go:build js

package probe

import "net"

func ProbeDialer() *net.Dialer { return &net.Dialer{} }
