package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"

	"golang.org/x/net/dns/dnsmessage"
)

// The cluster network of the pods on the tab nodes. A pod VM has no
// network of its own: the network adapter of v86 ends TCP in JavaScript.
// vmworker.js hands the connections that a pod opens to a pod IP or a
// ClusterIP to Go (v86Pod.outbound), and its DNS queries (v86Pod.resolve).
// Go plays kube-proxy and CoreDNS for them:
//
//   - a ClusterIP goes to a ready endpoint of the Service, at random, as
//     kube-proxy in iptables mode does;
//   - a pod IP goes to the VM of the pod when it runs on a tab node, else
//     through the port-forward of the apiserver (podnet.go);
//   - DNS answers what the kubernetes plugin of CoreDNS answers: A records
//     of Services, of the pods behind headless Services (hostname.subdomain
//     of StatefulSet pods too) and of pods (1-2-3-4.ns.pod), SRV records of
//     named ports, CNAME of ExternalName Services and PTR records.
//
// The Services and EndpointSlices come from informers, started on the first
// use, so a cluster without pod traffic does not pay for them.

const (
	clusterDomain = "cluster.local"
	clusterDNSIP  = "10.96.0.10" // the kube-dns Service of addons/coredns.yaml
	dnsTTL        = 5
)

type clusterCache struct {
	svcs   cache.Indexer
	slices cache.Indexer
	// for NetworkPolicy (netpol.go)
	netpols cache.Indexer
	pods    cache.Indexer
	nss     cache.Indexer
}

var (
	clusterCacheOnce sync.Once
	clusterCacheV    *clusterCache
	clusterCacheErr  error
	clusterCacheDone = make(chan struct{})
)

const (
	byClusterIP = "clusterIP"
	byService   = "service"
	byPodIP     = "podIP"
)

// clusterNet returns the cache of Services and EndpointSlices once it is
// in sync. The first call starts the informers.
func clusterNet(ctx context.Context) (*clusterCache, error) {
	clusterCacheOnce.Do(func() {
		go func() {
			defer close(clusterCacheDone)
			_, cs, err := podNetClient()
			if err != nil {
				clusterCacheErr = err
				return
			}
			f := informers.NewSharedInformerFactory(cs, 0)
			si := f.Core().V1().Services().Informer()
			si.AddIndexers(cache.Indexers{byClusterIP: func(o any) ([]string, error) {
				var ips []string
				for _, ip := range o.(*corev1.Service).Spec.ClusterIPs {
					if ip != "" && ip != corev1.ClusterIPNone {
						ips = append(ips, ip)
					}
				}
				return ips, nil
			}})
			ei := f.Discovery().V1().EndpointSlices().Informer()
			ei.AddIndexers(cache.Indexers{byService: func(o any) ([]string, error) {
				s := o.(*discoveryv1.EndpointSlice)
				if name := s.Labels[discoveryv1.LabelServiceName]; name != "" {
					return []string{s.Namespace + "/" + name}, nil
				}
				return nil, nil
			}})
			npi := f.Networking().V1().NetworkPolicies().Informer()
			npi.AddIndexers(cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			pi := f.Core().V1().Pods().Informer()
			pi.AddIndexers(cache.Indexers{byPodIP: func(o any) ([]string, error) {
				p := o.(*corev1.Pod)
				if p.Spec.HostNetwork || p.Status.PodIP == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
					return nil, nil
				}
				return []string{p.Status.PodIP}, nil
			}})
			nsi := f.Core().V1().Namespaces().Informer()
			// a change of a policy, or of the labels of a pod or a namespace,
			// can change what the pods may reach outside the cluster
			for _, i := range []cache.SharedIndexInformer{npi, pi, nsi} {
				i.AddEventHandler(cache.ResourceEventHandlerFuncs{
					AddFunc:    func(any) { egressSoon() },
					UpdateFunc: func(any, any) { egressSoon() },
					DeleteFunc: func(any) { egressSoon() },
				})
			}
			f.Start(make(chan struct{}))
			wait, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if !cache.WaitForCacheSync(wait.Done(), si.HasSynced, ei.HasSynced, npi.HasSynced, pi.HasSynced, nsi.HasSynced) {
				clusterCacheErr = errors.New("the Services, EndpointSlices, NetworkPolicies, Pods and Namespaces of the cluster did not sync")
				return
			}
			clusterCacheV = &clusterCache{svcs: si.GetIndexer(), slices: ei.GetIndexer(), netpols: npi.GetIndexer(), pods: pi.GetIndexer(), nss: nsi.GetIndexer()}
			egressSoon()
		}()
	})
	select {
	case <-clusterCacheDone:
		return clusterCacheV, clusterCacheErr
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *clusterCache) service(ns, name string) *corev1.Service {
	o, ok, _ := c.svcs.GetByKey(ns + "/" + name)
	if !ok {
		return nil
	}
	return o.(*corev1.Service)
}

func (c *clusterCache) serviceByIP(ip string) *corev1.Service {
	l, _ := c.svcs.ByIndex(byClusterIP, ip)
	if len(l) == 0 {
		return nil
	}
	return l[0].(*corev1.Service)
}

// endpoint is an address of a Service.
type endpoint struct {
	ip       string
	hostname string // of a pod with hostname and subdomain, for DNS
	ready    bool
	pod      *corev1.ObjectReference
	ports    []discoveryv1.EndpointPort
}

// endpoints returns the IPv4 endpoints of svc.
func (c *clusterCache) endpoints(svc *corev1.Service) []endpoint {
	l, _ := c.slices.ByIndex(byService, svc.Namespace+"/"+svc.Name)
	var out []endpoint
	seen := map[string]bool{}
	for _, o := range l {
		s := o.(*discoveryv1.EndpointSlice)
		if s.AddressType != discoveryv1.AddressTypeIPv4 {
			continue
		}
		for _, e := range s.Endpoints {
			if len(e.Addresses) == 0 || seen[e.Addresses[0]] {
				continue
			}
			seen[e.Addresses[0]] = true
			out = append(out, endpoint{ip: e.Addresses[0], hostname: ptrOr(e.Hostname, ""), ready: ptrOr(e.Conditions.Ready, true), pod: e.TargetRef, ports: s.Ports})
		}
	}
	// a stable order for DNS answers and SRV records
	slices.SortFunc(out, func(a, b endpoint) int { return strings.Compare(a.ip, b.ip) })
	return out
}

// dialFromPod connects a pod (src, its IP) to ip:port, a pod IP or a
// ClusterIP.
func dialFromPod(ctx context.Context, src, ip string, port int) (net.Conn, error) {
	if _, n, _ := net.ParseCIDR(serviceCIDR); n.Contains(net.ParseIP(ip)) {
		c, err := clusterNet(ctx)
		if err != nil {
			return nil, err
		}
		svc := c.serviceByIP(ip)
		if svc == nil {
			return nil, fmt.Errorf("dial tcp %s:%d: no Service has this ClusterIP", ip, port)
		}
		var sp *corev1.ServicePort
		for i := range svc.Spec.Ports {
			if p := &svc.Spec.Ports[i]; int(p.Port) == port && (p.Protocol == "" || p.Protocol == corev1.ProtocolTCP) {
				sp = p
			}
		}
		if sp == nil {
			return nil, fmt.Errorf("dial tcp %s:%d: the Service %s/%s has no such port", ip, port, svc.Namespace, svc.Name)
		}
		var ready []endpoint
		for _, e := range c.endpoints(svc) {
			if e.ready {
				ready = append(ready, e)
			}
		}
		// at random, as kube-proxy in iptables mode; another one if a pod
		// does not answer
		rand.Shuffle(len(ready), func(i, j int) { ready[i], ready[j] = ready[j], ready[i] })
		err = fmt.Errorf("dial tcp %s:%d: the Service %s/%s has no ready endpoint", ip, port, svc.Namespace, svc.Name)
		for _, e := range ready {
			target := 0
			for _, p := range e.ports {
				if p.Port != nil && ptrOr(p.Name, "") == sp.Name && (p.Protocol == nil || *p.Protocol == corev1.ProtocolTCP) {
					target = int(*p.Port)
				}
			}
			if target == 0 {
				continue
			}
			var conn net.Conn
			if conn, err = dialEndpoint(ctx, src, e, target); err == nil {
				return conn, nil
			}
			if errors.Is(err, errDropped) {
				// kube-proxy picked this endpoint, and the packet is gone
				return nil, err
			}
		}
		return nil, err
	}
	return dialEndpoint(ctx, src, endpoint{ip: ip}, port)
}

// dialEndpoint connects src to port of the pod e.
func dialEndpoint(ctx context.Context, src string, e endpoint, port int) (net.Conn, error) {
	if err := checkPolicy(ctx, src, e.ip, port); err != nil {
		return nil, err
	}
	if src == e.ip {
		// a pod that reaches itself through its Service: Linux drops a
		// packet from its own address, so it comes from the gateway, as
		// kube-proxy masquerades hairpin traffic
		src = ""
	}
	if wl := tabWorkloadByIP(e.ip); wl != nil {
		if v, ok := wl.(*v86Pod); ok {
			return v.dialFrom(int32(port), src)
		}
		return wl.dial(int32(port))
	}
	// the endpoint of the Service kubernetes: the apiserver in this process
	if e.ip == advertiseIP {
		return dialLocal(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	}
	cfg, cs, err := podNetClient()
	if err != nil {
		return nil, err
	}
	var pod *corev1.Pod
	if e.pod != nil && e.pod.Kind == "Pod" {
		pod, err = cs.CoreV1().Pods(e.pod.Namespace).Get(ctx, e.pod.Name, metav1.GetOptions{})
	} else {
		pod, _, err = findPod(ctx, cs, e.ip, port)
	}
	if err != nil {
		return nil, err
	}
	if wl := tabWorkload(pod); wl != nil {
		return wl.dial(int32(port))
	}
	return dialPod(cfg, pod, port)
}

// hostsFile is /etc/hosts of the pod, as the kubelet writes it.
func (tp *tabPod) hostsFile() string {
	var b strings.Builder
	b.WriteString("# Kubernetes-managed hosts file.\n127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\nfe00::0\tip6-localnet\nfe00::0\tip6-mcastprefix\nfe00::1\tip6-allnodes\nfe00::2\tip6-allrouters\n")
	if tp.spec != nil && tp.spec.Subdomain != "" {
		fmt.Fprintf(&b, "%s\t%s.%s.%s.svc.%s\t%s\n", tp.ip, tp.hostname, tp.spec.Subdomain, tp.namespace, clusterDomain, tp.hostname)
	} else {
		fmt.Fprintf(&b, "%s\t%s\n", tp.ip, tp.hostname)
	}
	if tp.spec != nil && len(tp.spec.HostAliases) > 0 {
		b.WriteString("\n# Entries added by HostAliases.\n")
		for _, a := range tp.spec.HostAliases {
			fmt.Fprintf(&b, "%s\t%s\n", a.IP, strings.Join(a.Hostnames, "\t"))
		}
	}
	return b.String()
}

// resolvConf is /etc/resolv.conf of the pod, as the kubelet writes it for its
// DNS policy. Default is the name server of the network adapter of v86 (the
// gateway), which answers every name with the address that fetch() takes.
func (tp *tabPod) resolvConf() string {
	var servers, searches, options []string
	policy := corev1.DNSClusterFirst
	var cfg *corev1.PodDNSConfig
	if tp.spec != nil {
		cfg = tp.spec.DNSConfig
		if tp.spec.DNSPolicy != "" {
			policy = tp.spec.DNSPolicy
		}
		if policy == corev1.DNSClusterFirst && tp.spec.HostNetwork {
			policy = corev1.DNSDefault
		}
	}
	switch policy {
	case corev1.DNSClusterFirst, corev1.DNSClusterFirstWithHostNet:
		servers = []string{clusterDNSIP}
		searches = []string{tp.namespace + ".svc." + clusterDomain, "svc." + clusterDomain, clusterDomain}
		options = []string{"ndots:5"}
	case corev1.DNSDefault:
		servers = []string{tp.n.gateway()}
	}
	if cfg != nil {
		servers = append(servers, cfg.Nameservers...)
		searches = append(searches, cfg.Searches...)
		for _, o := range cfg.Options {
			opt := o.Name
			if o.Value != nil {
				opt += ":" + *o.Value
			}
			// a pod option replaces the one of the same name
			options = slices.DeleteFunc(options, func(s string) bool { return strings.SplitN(s, ":", 2)[0] == o.Name })
			options = append(options, opt)
		}
	}
	var b strings.Builder
	if len(searches) > 0 {
		b.WriteString("search " + strings.Join(searches, " ") + "\n")
	}
	for _, s := range servers {
		b.WriteString("nameserver " + s + "\n")
	}
	if len(options) > 0 {
		b.WriteString("options " + strings.Join(options, " ") + "\n")
	}
	return b.String()
}

// clusterDNS answers a DNS query (the message) of a pod in namespace ns, or
// returns nil when it cannot parse it.
func clusterDNS(ns string, query []byte) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return nil
	}
	rcode, answers, extra := dnsmessage.RCodeNameError, []dnsmessage.Resource(nil), []dnsmessage.Resource(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if c, err := clusterNet(ctx); err != nil {
		rcode = dnsmessage.RCodeServerFailure
	} else {
		rcode, answers, extra = c.lookup(q)
	}
	name := strings.ToLower(q.Name.String())
	authoritative := strings.HasSuffix(name, "."+clusterDomain+".") || strings.HasSuffix(name, ".in-addr.arpa.")
	b := dnsmessage.NewBuilder(make([]byte, 0, 512), dnsmessage.Header{
		ID: h.ID, Response: true, Authoritative: authoritative, RecursionDesired: h.RecursionDesired, RecursionAvailable: true, RCode: rcode,
	})
	b.EnableCompression()
	b.StartQuestions()
	b.Question(q)
	b.StartAnswers()
	for _, r := range answers {
		addResource(&b, r)
	}
	b.StartAdditionals()
	for _, r := range extra {
		addResource(&b, r)
	}
	msg, err := b.Finish()
	if err != nil {
		return nil
	}
	return msg
}

func addResource(b *dnsmessage.Builder, r dnsmessage.Resource) {
	switch body := r.Body.(type) {
	case *dnsmessage.AResource:
		b.AResource(r.Header, *body)
	case *dnsmessage.SRVResource:
		b.SRVResource(r.Header, *body)
	case *dnsmessage.CNAMEResource:
		b.CNAMEResource(r.Header, *body)
	case *dnsmessage.PTRResource:
		b.PTRResource(r.Header, *body)
	}
}

func rrA(name string, ip string) dnsmessage.Resource {
	var a [4]byte
	copy(a[:], net.ParseIP(ip).To4())
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: dnsTTL},
		Body:   &dnsmessage.AResource{A: a},
	}
}

func rrSRV(name string, port int, target string, weight uint16) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET, TTL: dnsTTL},
		Body:   &dnsmessage.SRVResource{Priority: 0, Weight: weight, Port: uint16(port), Target: dnsmessage.MustNewName(target)},
	}
}

func rrName(name string, typ dnsmessage.Type, target string) dnsmessage.Resource {
	h := dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET, TTL: dnsTTL}
	if typ == dnsmessage.TypeCNAME {
		return dnsmessage.Resource{Header: h, Body: &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName(target)}}
	}
	return dnsmessage.Resource{Header: h, Body: &dnsmessage.PTRResource{PTR: dnsmessage.MustNewName(target)}}
}

// dashed is the name of an address in DNS: 10-244-1-2.
func dashed(ip string) string { return strings.ReplaceAll(ip, ".", "-") }

func undashed(s string) string {
	ip := net.ParseIP(strings.ReplaceAll(s, "-", "."))
	if ip == nil || ip.To4() == nil {
		return ""
	}
	return ip.String()
}

// endpointName is the first label of an endpoint of a headless Service.
func endpointName(e endpoint) string {
	if e.hostname != "" {
		return e.hostname
	}
	return dashed(e.ip)
}

// lookup answers q like the kubernetes plugin of CoreDNS: it returns the
// response code, the answers and the additional records.
func (c *clusterCache) lookup(q dnsmessage.Question) (dnsmessage.RCode, []dnsmessage.Resource, []dnsmessage.Resource) {
	name := strings.ToLower(q.Name.String())
	fqdn := name
	name = strings.TrimSuffix(name, ".")
	if strings.HasSuffix(name, ".in-addr.arpa") {
		return c.lookupPTR(q, fqdn, strings.TrimSuffix(name, ".in-addr.arpa"))
	}
	zone := "." + clusterDomain
	if name == clusterDomain || name == "svc"+zone || name == "pod"+zone {
		return dnsmessage.RCodeSuccess, nil, nil
	}
	if !strings.HasSuffix(name, zone) {
		return c.lookupExternal(q, fqdn, name)
	}
	labels := strings.Split(strings.TrimSuffix(name, zone), ".")
	n := len(labels)
	switch {
	// <a-b-c-d>.<ns>.pod.cluster.local
	case n == 3 && labels[2] == "pod":
		ip := undashed(labels[0])
		if ip == "" {
			return dnsmessage.RCodeNameError, nil, nil
		}
		if q.Type == dnsmessage.TypeA {
			return dnsmessage.RCodeSuccess, []dnsmessage.Resource{rrA(fqdn, ip)}, nil
		}
		return dnsmessage.RCodeSuccess, nil, nil
	case n == 2 && (labels[1] == "svc" || labels[1] == "pod"):
		// a namespace: the name exists
		return dnsmessage.RCodeSuccess, nil, nil
	case n < 3 || labels[n-1] != "svc":
		return dnsmessage.RCodeNameError, nil, nil
	}
	ns, svcName := labels[n-2], labels[n-3]
	svc := c.service(ns, svcName)
	if svc == nil {
		return dnsmessage.RCodeNameError, nil, nil
	}
	svcFQDN := svcName + "." + ns + ".svc." + clusterDomain + "."
	headless := svc.Spec.ClusterIP == corev1.ClusterIPNone
	var eps []endpoint
	if headless {
		for _, e := range c.endpoints(svc) {
			if e.ready || svc.Spec.PublishNotReadyAddresses {
				eps = append(eps, e)
			}
		}
	}
	switch n {
	case 3: // <svc>.<ns>.svc
		switch {
		case svc.Spec.Type == corev1.ServiceTypeExternalName:
			target := strings.TrimSuffix(strings.ToLower(svc.Spec.ExternalName), ".") + "."
			ans := []dnsmessage.Resource{rrName(fqdn, dnsmessage.TypeCNAME, target)}
			if q.Type == dnsmessage.TypeA {
				// The stub resolvers of glibc and musl do not ask again for
				// the target. A Service is answered here (one step, so a
				// loop of ExternalNames ends), any other name as the
				// network adapter of v86 does.
				if !strings.HasSuffix(target, zone+".") {
					ans = append(ans, rrA(target, fetchAdapterIP))
				} else if t := strings.Split(strings.TrimSuffix(target, zone+"."), "."); len(t) == 3 && t[2] == "svc" {
					if s := c.service(t[1], t[0]); s != nil && s.Spec.Type != corev1.ServiceTypeExternalName {
						_, more, _ := c.lookup(dnsmessage.Question{Name: dnsmessage.MustNewName(target), Type: dnsmessage.TypeA, Class: q.Class})
						ans = append(ans, more...)
					}
				}
			}
			return dnsmessage.RCodeSuccess, ans, nil
		case q.Type == dnsmessage.TypeA && headless:
			var ans []dnsmessage.Resource
			for _, e := range eps {
				ans = append(ans, rrA(fqdn, e.ip))
			}
			return dnsmessage.RCodeSuccess, ans, nil
		case q.Type == dnsmessage.TypeA:
			if svc.Spec.ClusterIP == "" {
				return dnsmessage.RCodeSuccess, nil, nil
			}
			return dnsmessage.RCodeSuccess, []dnsmessage.Resource{rrA(fqdn, svc.Spec.ClusterIP)}, nil
		case q.Type == dnsmessage.TypeSRV:
			ans, extra := c.srv(svc, eps, fqdn, svcFQDN, "", "")
			return dnsmessage.RCodeSuccess, ans, extra
		}
		return dnsmessage.RCodeSuccess, nil, nil
	case 4: // <endpoint>.<svc>.<ns>.svc
		if !headless {
			return dnsmessage.RCodeNameError, nil, nil
		}
		for _, e := range eps {
			if endpointName(e) == labels[0] {
				if q.Type == dnsmessage.TypeA {
					return dnsmessage.RCodeSuccess, []dnsmessage.Resource{rrA(fqdn, e.ip)}, nil
				}
				return dnsmessage.RCodeSuccess, nil, nil
			}
		}
		return dnsmessage.RCodeNameError, nil, nil
	case 5: // _<port>._<proto>.<svc>.<ns>.svc
		if !strings.HasPrefix(labels[0], "_") || !strings.HasPrefix(labels[1], "_") {
			return dnsmessage.RCodeNameError, nil, nil
		}
		ans, extra := c.srv(svc, eps, fqdn, svcFQDN, labels[0][1:], labels[1][1:])
		if len(ans) == 0 {
			return dnsmessage.RCodeNameError, nil, nil
		}
		if q.Type != dnsmessage.TypeSRV {
			return dnsmessage.RCodeSuccess, nil, nil
		}
		return dnsmessage.RCodeSuccess, ans, extra
	}
	return dnsmessage.RCodeNameError, nil, nil
}

// lookupExternal answers a name outside the cluster domain. There is no
// upstream name server: like the network adapter of v86, an A record of
// fetchAdapterIP stands for every name, and HTTP to it goes out through
// fetch() with the name in the Host header. But a name that a pod meant
// relative to its search domains (db-0.db, web.default, kubernetes.default,
// a single label) does not exist, as with a real upstream: the resolver then
// tries the search domains, as busybox nslookup and resolvers with a small
// ndots do.
func (c *clusterCache) lookupExternal(q dnsmessage.Question, fqdn, name string) (dnsmessage.RCode, []dnsmessage.Resource, []dnsmessage.Resource) {
	labels := strings.Split(name, ".")
	last := labels[len(labels)-1]
	if len(labels) == 1 || last == "svc" || last == "pod" || last == "local" || last == "cluster" {
		return dnsmessage.RCodeNameError, nil, nil
	}
	for _, o := range c.svcs.List() {
		// a namespace (one with a Service), or a Service: <name>.<svc>
		if svc := o.(*corev1.Service); svc.Namespace == last || svc.Name == last {
			return dnsmessage.RCodeNameError, nil, nil
		}
	}
	if q.Type == dnsmessage.TypeA {
		return dnsmessage.RCodeSuccess, []dnsmessage.Resource{rrA(fqdn, fetchAdapterIP)}, nil
	}
	return dnsmessage.RCodeSuccess, nil, nil
}

// srv returns the SRV records of the named ports of svc that match port and
// proto ("" for all), and the A records of their targets.
func (c *clusterCache) srv(svc *corev1.Service, eps []endpoint, fqdn, svcFQDN, port, proto string) (ans, extra []dnsmessage.Resource) {
	for _, sp := range svc.Spec.Ports {
		p := strings.ToLower(string(sp.Protocol))
		if p == "" {
			p = "tcp"
		}
		if sp.Name == "" || (port != "" && port != strings.ToLower(sp.Name)) || (proto != "" && proto != p) {
			continue
		}
		if svc.Spec.ClusterIP != corev1.ClusterIPNone {
			ans = append(ans, rrSRV(fqdn, int(sp.Port), svcFQDN, 100))
			if svc.Spec.ClusterIP != "" {
				extra = append(extra, rrA(svcFQDN, svc.Spec.ClusterIP))
			}
			continue
		}
		w := uint16(100 / max(1, len(eps)))
		for _, e := range eps {
			for _, ep := range e.ports {
				if ep.Port != nil && ptrOr(ep.Name, "") == sp.Name {
					target := endpointName(e) + "." + svcFQDN
					ans = append(ans, rrSRV(fqdn, int(*ep.Port), target, w))
					extra = append(extra, rrA(target, e.ip))
				}
			}
		}
	}
	return ans, extra
}

// lookupPTR answers a reverse lookup of a ClusterIP or of an endpoint of a
// headless Service.
func (c *clusterCache) lookupPTR(q dnsmessage.Question, fqdn, rev string) (dnsmessage.RCode, []dnsmessage.Resource, []dnsmessage.Resource) {
	parts := strings.Split(rev, ".")
	if len(parts) != 4 {
		return dnsmessage.RCodeNameError, nil, nil
	}
	slices.Reverse(parts)
	ip := net.ParseIP(strings.Join(parts, "."))
	if ip == nil || !inClusterNet(ip.String()) {
		return dnsmessage.RCodeNameError, nil, nil
	}
	var target string
	if svc := c.serviceByIP(ip.String()); svc != nil {
		target = svc.Name + "." + svc.Namespace + ".svc." + clusterDomain + "."
	} else {
		for _, o := range c.svcs.List() {
			svc := o.(*corev1.Service)
			if svc.Spec.ClusterIP != corev1.ClusterIPNone {
				continue
			}
			for _, e := range c.endpoints(svc) {
				if e.ip == ip.String() {
					target = endpointName(e) + "." + svc.Name + "." + svc.Namespace + ".svc." + clusterDomain + "."
				}
			}
		}
	}
	if target == "" {
		return dnsmessage.RCodeNameError, nil, nil
	}
	if q.Type != dnsmessage.TypePTR {
		return dnsmessage.RCodeSuccess, nil, nil
	}
	return dnsmessage.RCodeSuccess, []dnsmessage.Resource{rrName(fqdn, dnsmessage.TypePTR, target)}, nil
}

// fetchAdapterIP is what the network adapter of v86 answers for every name
// (dns_method "static"): HTTP to it goes out through fetch().
const fetchAdapterIP = "192.168.87.1"
