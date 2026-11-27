package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall/js"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

// NetworkPolicy for the pods on the tab nodes. Go sees every connection that
// a tab pod opens into the cluster network (clusternet.go) and every DNS
// query, so it can do what the network plugin of a cluster does with the
// policies of networking.k8s.io/v1: a pod that a policy selects for Ingress
// (Egress) takes (opens) only the connections that a rule of such a policy
// allows. A connection that no rule allows is dropped, not refused: the pod
// sees a timeout, as with Calico or Cilium. A retransmitted SYN is checked
// again, so a new policy works on the next attempt; a connection that is
// open stays open, as with conntrack.
//
// What is checked:
//   - TCP between pods, to ClusterIPs (the policy sees the endpoint behind the
//     Service, as after the DNAT of kube-proxy) and to the apiserver;
//   - DNS: a query goes to the pods of the Service kube-dns (UDP 53), so a
//     pod with a default-deny egress policy needs a rule for it, as in a
//     cluster. Without CoreDNS pods the rule is matched against the labels
//     of the selector of kube-dns;
//   - names outside the cluster (HTTP through fetch(), web/src/vmworker.js)
//     stand for "the internet": only an ipBlock of 0.0.0.0/0 matches them,
//     because the real address is not known.
//
// Not checked: traffic from the node (probes, port-forward, exec, the
// terminal), and the pods of the nodes from join (flannel has no policies).

// errDropped is the answer to a connection that a policy does not allow.
var errDropped = errors.New("dropped by a NetworkPolicy")

// npPeer is one end of a connection.
type npPeer struct {
	ip      string
	pod     *corev1.Pod // nil: not a pod (the apiserver, the internet)
	outside bool        // a name outside the cluster (fetch)
}

// npView is what the check needs from the cluster.
type npView interface {
	policies(ns string) []*networkingv1.NetworkPolicy
	nsLabels(ns string) labels.Set
}

// npAllowed tells whether a connection from src to port of dst is allowed:
// by the egress policies of src and the ingress policies of dst.
func npAllowed(v npView, src, dst npPeer, proto corev1.Protocol, port int) bool {
	if src.pod != nil && !npAllows(v, src.pod, networkingv1.PolicyTypeEgress, dst, proto, port, dst.pod) {
		return false
	}
	if dst.pod != nil && !npAllows(v, dst.pod, networkingv1.PolicyTypeIngress, src, proto, port, dst.pod) {
		return false
	}
	return true
}

// npAllows tells whether the policies of pod in one direction let the
// traffic with peer through. target is the pod with the port (for named
// ports), or nil.
func npAllows(v npView, pod *corev1.Pod, dir networkingv1.PolicyType, peer npPeer, proto corev1.Protocol, port int, target *corev1.Pod) bool {
	isolated := false
	for _, p := range v.policies(pod.Namespace) {
		if !npHasType(p, dir) || !selects(&p.Spec.PodSelector, pod.Labels) {
			continue
		}
		isolated = true
		if dir == networkingv1.PolicyTypeIngress {
			for _, r := range p.Spec.Ingress {
				if npPeersMatch(v, p.Namespace, r.From, peer) && npPortsMatch(r.Ports, proto, port, target) {
					return true
				}
			}
		} else {
			for _, r := range p.Spec.Egress {
				if npPeersMatch(v, p.Namespace, r.To, peer) && npPortsMatch(r.Ports, proto, port, target) {
					return true
				}
			}
		}
	}
	return !isolated
}

// npHasType: a policy without policyTypes is for Ingress, and for Egress too
// when it has egress rules (as the API server defaults it).
func npHasType(p *networkingv1.NetworkPolicy, t networkingv1.PolicyType) bool {
	types := p.Spec.PolicyTypes
	if len(types) == 0 {
		types = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
		if len(p.Spec.Egress) > 0 {
			types = append(types, networkingv1.PolicyTypeEgress)
		}
	}
	for _, x := range types {
		if x == t {
			return true
		}
	}
	return false
}

// selects tells whether a label selector matches; {} matches everything.
func selects(s *metav1.LabelSelector, l map[string]string) bool {
	sel, err := metav1.LabelSelectorAsSelector(s)
	return err == nil && sel.Matches(labels.Set(l))
}

// npPeersMatch: no peers means every peer.
func npPeersMatch(v npView, ns string, peers []networkingv1.NetworkPolicyPeer, peer npPeer) bool {
	if len(peers) == 0 {
		return true
	}
	for _, p := range peers {
		if p.IPBlock != nil {
			if ipBlockMatches(p.IPBlock, peer) {
				return true
			}
			continue
		}
		if peer.pod == nil {
			continue
		}
		if p.NamespaceSelector == nil {
			if peer.pod.Namespace != ns {
				continue
			}
		} else if !selects(p.NamespaceSelector, v.nsLabels(peer.pod.Namespace)) {
			continue
		}
		if p.PodSelector == nil || selects(p.PodSelector, peer.pod.Labels) {
			return true
		}
	}
	return false
}

func ipBlockMatches(b *networkingv1.IPBlock, peer npPeer) bool {
	_, n, err := net.ParseCIDR(b.CIDR)
	if err != nil {
		return false
	}
	if peer.outside {
		ones, _ := n.Mask.Size()
		return ones == 0 && n.IP.To4() != nil
	}
	ip := net.ParseIP(peer.ip)
	if ip == nil || !n.Contains(ip) {
		return false
	}
	for _, e := range b.Except {
		if _, x, err := net.ParseCIDR(e); err == nil && x.Contains(ip) {
			return false
		}
	}
	return true
}

// npPortsMatch: no ports means every port. A named port is looked up in the
// containers of target.
func npPortsMatch(ports []networkingv1.NetworkPolicyPort, proto corev1.Protocol, port int, target *corev1.Pod) bool {
	if len(ports) == 0 {
		return true
	}
	for _, p := range ports {
		if orTCP(ptrOr(p.Protocol, "")) != proto {
			continue
		}
		switch {
		case p.Port == nil:
			return true
		case p.Port.Type == 0: // a number
			lo := int(p.Port.IntVal)
			hi := lo
			if p.EndPort != nil {
				hi = int(*p.EndPort)
			}
			if port >= lo && port <= hi {
				return true
			}
		case target != nil:
			for _, c := range target.Spec.Containers {
				for _, cp := range c.Ports {
					if cp.Name == p.Port.StrVal && int(cp.ContainerPort) == port && orTCP(cp.Protocol) == proto {
						return true
					}
				}
			}
		}
	}
	return false
}

func orTCP(p corev1.Protocol) corev1.Protocol {
	if p == "" {
		return corev1.ProtocolTCP
	}
	return p
}

// policies returns the NetworkPolicies of a namespace.
func (c *clusterCache) policies(ns string) []*networkingv1.NetworkPolicy {
	if c.netpols == nil {
		return nil
	}
	l, _ := c.netpols.ByIndex(cache.NamespaceIndex, ns)
	out := make([]*networkingv1.NetworkPolicy, 0, len(l))
	for _, o := range l {
		out = append(out, o.(*networkingv1.NetworkPolicy))
	}
	return out
}

// nsLabels returns the labels of a namespace. The apiserver sets
// kubernetes.io/metadata.name on every namespace.
func (c *clusterCache) nsLabels(ns string) labels.Set {
	if c.nss != nil {
		if o, ok, _ := c.nss.GetByKey(ns); ok {
			return labels.Set(o.(*corev1.Namespace).Labels)
		}
	}
	return labels.Set{corev1.LabelMetadataName: ns}
}

// podByIP returns the pod with an IP, or nil.
func (c *clusterCache) podByIP(ip string) *corev1.Pod {
	if c.pods == nil || ip == "" {
		return nil
	}
	l, _ := c.pods.ByIndex(byPodIP, ip)
	if len(l) > 0 {
		return l[0].(*corev1.Pod)
	}
	// a pod of a tab node whose status with the IP is not written yet
	for _, n := range tabNodes() {
		n.mu.Lock()
		var found *tabPod
		for _, tp := range n.pods {
			if tp.ip == ip && ip != n.ip {
				found = tp
			}
		}
		n.mu.Unlock()
		if found != nil {
			if o, ok, _ := c.pods.GetByKey(found.namespace + "/" + found.name); ok {
				return o.(*corev1.Pod)
			}
			return found.pod
		}
	}
	return nil
}

// checkPolicy returns errDropped when no policy allows the TCP connection
// from the pod src to port of ip.
func checkPolicy(ctx context.Context, src, ip string, port int) error {
	if src == "" {
		return nil // from the node
	}
	c, err := clusterNet(ctx)
	if err != nil {
		return err
	}
	from := c.podByIP(src)
	if from == nil {
		return nil
	}
	if !npAllowed(c, npPeer{ip: src, pod: from}, npPeer{ip: ip, pod: c.podByIP(ip)}, corev1.ProtocolTCP, port) {
		return errDropped
	}
	return nil
}

// dnsAllowed tells whether the pod with the IP src may ask the cluster DNS:
// UDP 53 to a pod of the Service kube-dns. Without such a pod (CoreDNS runs
// on the nodes from join) the rules see a pod with the labels of the selector
// of the Service, in its namespace.
func dnsAllowed(ctx context.Context, src string) bool {
	c, err := clusterNet(ctx)
	if err != nil {
		return true // clusterDNS answers SERVFAIL
	}
	from := c.podByIP(src)
	if from == nil {
		return true
	}
	svc := c.serviceByIP(clusterDNSIP)
	if svc == nil {
		return npAllows(c, from, networkingv1.PolicyTypeEgress, npPeer{ip: clusterDNSIP}, corev1.ProtocolUDP, 53, nil)
	}
	var targets []npPeer
	for _, e := range c.endpoints(svc) {
		if p := c.podByIP(e.ip); p != nil {
			targets = append(targets, npPeer{ip: e.ip, pod: p})
		}
	}
	if len(targets) == 0 {
		virtual := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: svc.Namespace, Labels: svc.Spec.Selector}}
		var ports []corev1.ContainerPort
		for _, p := range svc.Spec.Ports {
			if p.TargetPort.IntValue() != 0 {
				ports = append(ports, corev1.ContainerPort{Name: p.Name, ContainerPort: int32(p.TargetPort.IntValue()), Protocol: p.Protocol})
			}
		}
		virtual.Spec.Containers = []corev1.Container{{Ports: ports}}
		targets = append(targets, npPeer{ip: clusterDNSIP, pod: virtual})
	}
	for _, t := range targets {
		if npAllowed(c, npPeer{ip: src, pod: from}, t, corev1.ProtocolUDP, 53) {
			return true
		}
	}
	return false
}

// The pods reach names outside the cluster through the network adapter of
// v86 (fetch), without Go. So Go tells each VM whether its pod may. The
// informers start 2 s after the first pod of the session started (v86.go),
// or at the first connection or DNS query of a pod, so that the first pod
// does not wait for them; until they synced, the pods may.
var (
	vmsMu      sync.Mutex
	vms        = map[*v86Pod]int{} // the last state sent: 0 none, 1 allowed, 2 denied
	egressTime *time.Timer
)

func addVM(v *v86Pod) {
	vmsMu.Lock()
	vms[v] = 0
	vmsMu.Unlock()
	egressSoon()
}

func removeVM(v *v86Pod) {
	vmsMu.Lock()
	delete(vms, v)
	vmsMu.Unlock()
}

// egressSoon sends the states after a burst of changes.
func egressSoon() {
	vmsMu.Lock()
	defer vmsMu.Unlock()
	if egressTime == nil {
		egressTime = time.AfterFunc(20*time.Millisecond, sendEgress)
	}
}

// egressAllowed tells whether the pod with the IP may reach names outside
// the cluster, if the policies are known.
func egressAllowed(ip string) (allowed, known bool) {
	c := clusterCacheV
	if c == nil {
		return false, false
	}
	p := c.podByIP(ip)
	return p == nil || npAllows(c, p, networkingv1.PolicyTypeEgress, npPeer{outside: true}, corev1.ProtocolTCP, 80, nil), true
}

func sendEgress() {
	vmsMu.Lock()
	egressTime = nil
	l := make([]*v86Pod, 0, len(vms))
	for v := range vms {
		l = append(l, v)
	}
	vmsMu.Unlock()
	if clusterCacheV == nil {
		return // not known yet
	}
	for _, v := range l {
		state := 2
		if ok, _ := egressAllowed(v.tp.ip); ok {
			state = 1
		}
		vmsMu.Lock()
		old, ok := vms[v]
		if ok {
			vms[v] = state
		}
		vmsMu.Unlock()
		if ok && old != state && v.h.Type() == js.TypeObject {
			v.h.Call("egress", state == 1)
		}
	}
}
