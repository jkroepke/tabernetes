package main

import (
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	"golang.org/x/net/dns/dnsmessage"
)

func testClusterCache(t *testing.T) *clusterCache {
	svcs := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{byClusterIP: func(o any) ([]string, error) {
		return o.(*corev1.Service).Spec.ClusterIPs, nil
	}})
	eps := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{byService: func(o any) ([]string, error) {
		s := o.(*discoveryv1.EndpointSlice)
		return []string{s.Namespace + "/" + s.Labels[discoveryv1.LabelServiceName]}, nil
	}})
	add := func(i cache.Indexer, o any) {
		if err := i.Add(o); err != nil {
			t.Fatal(err)
		}
	}
	port := func(name string, p int32) corev1.ServicePort {
		return corev1.ServicePort{Name: name, Port: p, Protocol: corev1.ProtocolTCP}
	}
	add(svcs, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}, Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.20", ClusterIPs: []string{"10.96.0.20"}, Ports: []corev1.ServicePort{port("http", 8080)}}})
	add(svcs, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "db"}, Spec: corev1.ServiceSpec{ClusterIP: "None", ClusterIPs: []string{"None"}, Ports: []corev1.ServicePort{port("http", 80)}}})
	add(svcs, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ext"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "web.default.svc.cluster.local"}})
	add(svcs, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "out"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "example.com"}})
	slice := func(svc string, eps ...discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
		return &discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Namespace: "default", Name: svc + "-x", Labels: map[string]string{discoveryv1.LabelServiceName: svc}},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints:   eps,
			Ports:       []discoveryv1.EndpointPort{{Name: ptr.To("http"), Port: ptr.To[int32](80), Protocol: ptr.To(corev1.ProtocolTCP)}},
		}
	}
	add(eps, slice("web", discoveryv1.Endpoint{Addresses: []string{"10.244.1.2"}}, discoveryv1.Endpoint{Addresses: []string{"10.244.1.3"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(false)}}))
	add(eps, slice("db", discoveryv1.Endpoint{Addresses: []string{"10.244.1.5"}, Hostname: ptr.To("db-1")}, discoveryv1.Endpoint{Addresses: []string{"10.244.1.4"}, Hostname: ptr.To("db-0")}))
	return &clusterCache{svcs: svcs, slices: eps}
}

// ask returns the response code and the answers as "TYPE value" strings.
func ask(t *testing.T, c *clusterCache, name string, typ dnsmessage.Type) (dnsmessage.RCode, []string) {
	t.Helper()
	rcode, ans, extra := c.lookup(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET})
	var out []string
	for _, r := range append(ans, extra...) {
		switch b := r.Body.(type) {
		case *dnsmessage.AResource:
			out = append(out, "A "+r.Header.Name.String()+" "+strings.Join([]string{itoa(b.A[0]), itoa(b.A[1]), itoa(b.A[2]), itoa(b.A[3])}, "."))
		case *dnsmessage.SRVResource:
			out = append(out, "SRV "+itoa(int(b.Port))+" "+b.Target.String())
		case *dnsmessage.CNAMEResource:
			out = append(out, "CNAME "+b.CNAME.String())
		case *dnsmessage.PTRResource:
			out = append(out, "PTR "+b.PTR.String())
		}
	}
	return rcode, out
}

func itoa[T ~uint8 | ~int](v T) string { return strconv.Itoa(int(v)) }

func mustCIDR(t *testing.T, s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestClusterDNS(t *testing.T) {
	c := testClusterCache(t)
	for _, tc := range []struct {
		name  string
		typ   dnsmessage.Type
		rcode dnsmessage.RCode
		want  []string
	}{
		{"web.default.svc.cluster.local.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, []string{"A web.default.svc.cluster.local. 10.96.0.20"}},
		{"WEB.default.svc.cluster.local.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, []string{"A web.default.svc.cluster.local. 10.96.0.20"}},
		{"web.default.svc.cluster.local.", dnsmessage.TypeAAAA, dnsmessage.RCodeSuccess, nil},
		{"web.other.svc.cluster.local.", dnsmessage.TypeA, dnsmessage.RCodeNameError, nil},
		{"web.default.svc.default.svc.cluster.local.", dnsmessage.TypeA, dnsmessage.RCodeNameError, nil},
		{"example.com.default.svc.cluster.local.", dnsmessage.TypeA, dnsmessage.RCodeNameError, nil},
		// headless: the ready endpoints, and each by its hostname
		{"db.default.svc.cluster.local.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, []string{"A db.default.svc.cluster.local. 10.244.1.4", "A db.default.svc.cluster.local. 10.244.1.5"}},
		{"db-0.db.default.svc.cluster.local.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, []string{"A db-0.db.default.svc.cluster.local. 10.244.1.4"}},
		{"db-2.db.default.svc.cluster.local.", dnsmessage.TypeA, dnsmessage.RCodeNameError, nil},
		{"_http._tcp.db.default.svc.cluster.local.", dnsmessage.TypeSRV, dnsmessage.RCodeSuccess, []string{
			"SRV 80 db-0.db.default.svc.cluster.local.", "SRV 80 db-1.db.default.svc.cluster.local.",
			"A db-0.db.default.svc.cluster.local. 10.244.1.4", "A db-1.db.default.svc.cluster.local. 10.244.1.5"}},
		{"_http._tcp.web.default.svc.cluster.local.", dnsmessage.TypeSRV, dnsmessage.RCodeSuccess, []string{"SRV 8080 web.default.svc.cluster.local.", "A web.default.svc.cluster.local. 10.96.0.20"}},
		{"_http._udp.web.default.svc.cluster.local.", dnsmessage.TypeSRV, dnsmessage.RCodeNameError, nil},
		{"ext.default.svc.cluster.local.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, []string{"CNAME web.default.svc.cluster.local.", "A web.default.svc.cluster.local. 10.96.0.20"}},
		{"out.default.svc.cluster.local.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, []string{"CNAME example.com.", "A example.com. 192.168.87.1"}},
		{"10-244-1-9.default.pod.cluster.local.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, []string{"A 10-244-1-9.default.pod.cluster.local. 10.244.1.9"}},
		{"default.svc.cluster.local.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, nil},
		{"20.0.96.10.in-addr.arpa.", dnsmessage.TypePTR, dnsmessage.RCodeSuccess, []string{"PTR web.default.svc.cluster.local."}},
		{"4.1.244.10.in-addr.arpa.", dnsmessage.TypePTR, dnsmessage.RCodeSuccess, []string{"PTR db-0.db.default.svc.cluster.local."}},
		{"1.1.168.192.in-addr.arpa.", dnsmessage.TypePTR, dnsmessage.RCodeNameError, nil},
		// outside the cluster: fetch() stands for every name, but not for
		// names meant relative to the search domains
		{"example.com.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, []string{"A example.com. 192.168.87.1"}},
		{"example.com.", dnsmessage.TypeAAAA, dnsmessage.RCodeSuccess, nil},
		{"db-0.db.", dnsmessage.TypeA, dnsmessage.RCodeNameError, nil},
		{"web.default.", dnsmessage.TypeA, dnsmessage.RCodeNameError, nil},
		{"_http._tcp.db.", dnsmessage.TypeSRV, dnsmessage.RCodeNameError, nil},
		{"nope.", dnsmessage.TypeA, dnsmessage.RCodeNameError, nil},
	} {
		rcode, got := ask(t, c, tc.name, tc.typ)
		if rcode != tc.rcode || !slices.Equal(got, tc.want) {
			t.Errorf("%s %v: got %v %q, want %v %q", tc.name, tc.typ, rcode, got, tc.rcode, tc.want)
		}
	}
}

func TestResolvConf(t *testing.T) {
	n := &tabNode{}
	n.podCIDR = mustCIDR(t, "10.244.1.0/24")
	tp := &tabPod{n: n, namespace: "shop", ip: "10.244.1.7", hostname: "db-0", spec: &corev1.PodSpec{Subdomain: "db"}}
	if got, want := tp.resolvConf(), "search shop.svc.cluster.local svc.cluster.local cluster.local\nnameserver 10.96.0.10\noptions ndots:5\n"; got != want {
		t.Errorf("resolv.conf: %q, want %q", got, want)
	}
	if got := tp.hostsFile(); !strings.Contains(got, "10.244.1.7\tdb-0.db.shop.svc.cluster.local\tdb-0\n") {
		t.Errorf("hosts: %q", got)
	}
	tp.spec.DNSPolicy = corev1.DNSDefault
	tp.spec.DNSConfig = &corev1.PodDNSConfig{Options: []corev1.PodDNSConfigOption{{Name: "ndots", Value: ptr.To("2")}}}
	if got, want := tp.resolvConf(), "nameserver 10.244.1.1\noptions ndots:2\n"; got != want {
		t.Errorf("resolv.conf (Default): %q, want %q", got, want)
	}
}
