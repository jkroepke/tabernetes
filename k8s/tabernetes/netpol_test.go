package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

type testNPView struct {
	pols []*networkingv1.NetworkPolicy
	nss  map[string]labels.Set
}

func (v testNPView) policies(ns string) []*networkingv1.NetworkPolicy {
	var out []*networkingv1.NetworkPolicy
	for _, p := range v.pols {
		if p.Namespace == ns {
			out = append(out, p)
		}
	}
	return out
}

func (v testNPView) nsLabels(ns string) labels.Set {
	if l, ok := v.nss[ns]; ok {
		return l
	}
	return labels.Set{corev1.LabelMetadataName: ns}
}

func testPod(ns, name, ip string, l map[string]string, ports ...corev1.ContainerPort) npPeer {
	return npPeer{ip: ip, pod: &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: l},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Ports: ports}}},
	}}
}

func policy(ns, name string, sel map[string]string, spec networkingv1.NetworkPolicySpec) *networkingv1.NetworkPolicy {
	spec.PodSelector = metav1.LabelSelector{MatchLabels: sel}
	return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: spec}
}

func TestNetworkPolicy(t *testing.T) {
	web := testPod("default", "web", "10.244.0.2", map[string]string{"app": "web"}, corev1.ContainerPort{Name: "http", ContainerPort: 80})
	client := testPod("default", "client", "10.244.0.3", map[string]string{"role": "client"})
	other := testPod("default", "other", "10.244.0.4", map[string]string{"role": "other"})
	prod := testPod("prod", "client", "10.244.1.3", map[string]string{"role": "client"})
	db := testPod("db", "db", "10.244.2.2", map[string]string{"app": "db"})
	apiserver := npPeer{ip: "10.0.0.1"}
	internet := npPeer{outside: true}
	tcp := corev1.ProtocolTCP

	denyIngress := policy("default", "deny", nil, networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}})
	allowClients := policy("default", "clients", map[string]string{"app": "web"}, networkingv1.NetworkPolicySpec{
		Ingress: []networkingv1.NetworkPolicyIngressRule{{
			From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "client"}}}},
			Ports: []networkingv1.NetworkPolicyPort{{Port: ptr.To(intstr.FromString("http"))}},
		}},
	})
	fromProd := policy("default", "prod", map[string]string{"app": "web"}, networkingv1.NetworkPolicySpec{
		Ingress: []networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"role": "client"}},
			}},
		}},
	})
	denyEgress := policy("default", "deny-egress", nil, networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}})
	egressDB := policy("default", "to-db", map[string]string{"role": "client"}, networkingv1.NetworkPolicySpec{
		Egress: []networkingv1.NetworkPolicyEgressRule{
			{
				To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: "db"}}}},
				Ports: []networkingv1.NetworkPolicyPort{{Port: ptr.To(intstr.FromInt32(5000)), EndPort: ptr.To[int32](5010)}},
			},
			{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/24", Except: []string{"10.0.0.2/32"}}}}},
		},
	})
	internetOK := policy("default", "internet", map[string]string{"role": "client"}, networkingv1.NetworkPolicySpec{
		Egress: []networkingv1.NetworkPolicyEgressRule{{
			To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"10.0.0.0/8"}}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(80))}},
		}},
	})
	nss := map[string]labels.Set{"prod": {corev1.LabelMetadataName: "prod", "env": "prod"}}

	for _, c := range []struct {
		name     string
		pols     []*networkingv1.NetworkPolicy
		src, dst npPeer
		port     int
		want     bool
	}{
		{"no policy", nil, client, web, 80, true},
		{"deny ingress", []*networkingv1.NetworkPolicy{denyIngress}, client, web, 80, false},
		{"deny ingress, egress free", []*networkingv1.NetworkPolicy{denyIngress}, web, apiserver, 443, true},
		{"client to the named port", []*networkingv1.NetworkPolicy{denyIngress, allowClients}, client, web, 80, true},
		{"client to another port", []*networkingv1.NetworkPolicy{denyIngress, allowClients}, client, web, 8080, false},
		{"other pod", []*networkingv1.NetworkPolicy{allowClients}, other, web, 80, false},
		{"pod of another namespace", []*networkingv1.NetworkPolicy{allowClients}, prod, web, 80, false},
		{"namespace and pod selector", []*networkingv1.NetworkPolicy{fromProd}, prod, web, 80, true},
		{"namespace and pod selector, own namespace", []*networkingv1.NetworkPolicy{fromProd}, client, web, 80, false},
		{"deny egress", []*networkingv1.NetworkPolicy{denyEgress}, client, web, 80, false},
		{"egress port range", []*networkingv1.NetworkPolicy{egressDB}, client, db, 5005, true},
		{"egress port range, outside", []*networkingv1.NetworkPolicy{egressDB}, client, db, 5011, false},
		{"egress ipBlock", []*networkingv1.NetworkPolicy{egressDB}, client, apiserver, 443, true},
		{"egress ipBlock except", []*networkingv1.NetworkPolicy{egressDB}, client, npPeer{ip: "10.0.0.2"}, 443, false},
		{"egress to a pod not in the rules", []*networkingv1.NetworkPolicy{egressDB}, client, web, 80, false},
		{"internet denied", []*networkingv1.NetworkPolicy{denyEgress}, client, internet, 80, false},
		{"internet by 0.0.0.0/0", []*networkingv1.NetworkPolicy{denyEgress, internetOK}, client, internet, 80, true},
		{"internet, no /0", []*networkingv1.NetworkPolicy{egressDB}, client, internet, 80, false},
		{"pod IP inside 0.0.0.0/0 except", []*networkingv1.NetworkPolicy{internetOK}, client, web, 80, false},
	} {
		v := testNPView{pols: c.pols, nss: nss}
		if got := npAllowed(v, c.src, c.dst, tcp, c.port); got != c.want {
			t.Errorf("%s: allowed = %v, want %v", c.name, got, c.want)
		}
	}

	// DNS: UDP 53 to the pods of kube-dns
	dns := testPod("kube-system", "", "10.96.0.10", map[string]string{"k8s-app": "kube-dns"}, corev1.ContainerPort{Name: "dns", ContainerPort: 53, Protocol: corev1.ProtocolUDP})
	allowDNS := policy("default", "dns", nil, networkingv1.NetworkPolicySpec{
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		Egress: []networkingv1.NetworkPolicyEgressRule{{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: "kube-system"}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolUDP), Port: ptr.To(intstr.FromString("dns"))}},
		}},
	})
	udp := corev1.ProtocolUDP
	if npAllowed(testNPView{pols: []*networkingv1.NetworkPolicy{denyEgress}}, client, dns, udp, 53) {
		t.Error("DNS with a default-deny egress policy: allowed")
	}
	if !npAllowed(testNPView{pols: []*networkingv1.NetworkPolicy{denyEgress, allowDNS}}, client, dns, udp, 53) {
		t.Error("DNS with a rule for kube-dns: denied")
	}
	if npAllowed(testNPView{pols: []*networkingv1.NetworkPolicy{denyEgress, allowDNS}}, client, dns, tcp, 53) {
		t.Error("TCP 53 with a rule for UDP: allowed")
	}
}
