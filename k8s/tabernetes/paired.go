package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall/js"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// A paired node is a docker node from "join --keep": it stays in the
// cluster when its tabs close and when the page reloads. The tabs find it
// again through a relay (web/src/relay.js): they post a WebRTC offer to its
// topic, encrypted with its key, and the agent answers. The pairing is a
// Secret in kube-system, so it lives in the etcd snapshot like the rest of
// the cluster, and "forget" removes it with the cluster.
//
// A paired kubectl (join --kubectl) is no node: the agent forwards a local
// port to the kube-apiserver for a kubectl outside the browser. It gets a
// ServiceAccount bound to cluster-admin, so unjoin can take its access back.
type pairing struct {
	Kind  string `json:"kind"` // "node" or "kubectl"
	Name  string `json:"name"`
	IP    string `json:"ip"`
	Relay string `json:"relay"`
	Topic string `json:"topic"`
	Key   string `json:"key"`
}

const pairingLabel = "tabernetes.dev/paired-node"

var (
	pairedMu sync.Mutex
	paired   = map[string]pairing{}
)

// randKey is an AES-256 key for the relay messages of a paired node.
func randKey() string {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	must(err)
	return base64.RawURLEncoding.EncodeToString(b)
}

func pairingSecret(name string) string { return "tabernetes-pairing-" + name }

func isPaired(name string) bool {
	pairedMu.Lock()
	defer pairedMu.Unlock()
	_, ok := paired[name]
	return ok
}

// savePairing stores the pairing of a new node.
func savePairing(ctx context.Context, p pairing) error {
	cs, err := adminClient()
	if err != nil {
		return err
	}
	_, err = cs.CoreV1().Secrets("kube-system").Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:   pairingSecret(p.Name),
			Labels: map[string]string{pairingLabel: p.Name},
		},
		StringData: map[string]string{"kind": p.Kind, "name": p.Name, "ip": p.IP, "relay": p.Relay, "topic": p.Topic, "key": p.Key},
	}, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	pairedMu.Lock()
	paired[p.Name] = p
	pairedMu.Unlock()
	return nil
}

// loadPairings reads the pairings of a restored cluster, before
// dropOldNodes, and moves the counter of the node names past them.
func loadPairings(ctx context.Context) error {
	cs, err := adminClient()
	if err != nil {
		return err
	}
	list, err := cs.CoreV1().Secrets("kube-system").List(ctx, metav1.ListOptions{LabelSelector: pairingLabel})
	if err != nil {
		return err
	}
	pairedMu.Lock()
	defer pairedMu.Unlock()
	for _, s := range list.Items {
		d := func(k string) string { return string(s.Data[k]) }
		p := pairing{Kind: d("kind"), Name: d("name"), IP: d("ip"), Relay: d("relay"), Topic: d("topic"), Key: d("key")}
		if p.Kind == "" {
			p.Kind = "node"
		}
		if p.Name == "" || p.Kind == "node" && net.ParseIP(p.IP) == nil || p.Topic == "" || p.Key == "" {
			fmt.Fprintln(os.Stderr, "tabernetes: ignoring the broken pairing", s.Name)
			continue
		}
		paired[p.Name] = p
		// node-<n-1> has the IP 10.0.0.<n> (prepareJoin)
		if n, err := strconv.Atoi(strings.TrimPrefix(p.Name, "node-")); err == nil {
			nodesMu.Lock()
			nextNode = max(nextNode, n+2)
			nodesMu.Unlock()
		}
	}
	return nil
}

// pairings returns the paired nodes, sorted by name.
func pairings() []pairing {
	pairedMu.Lock()
	defer pairedMu.Unlock()
	list := make([]pairing, 0, len(paired))
	for _, p := range paired {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

func (p pairing) js() map[string]any {
	return map[string]any{"kind": p.Kind, "name": p.Name, "ip": p.IP, "relay": p.Relay, "topic": p.Topic, "key": p.Key}
}

// restoreNode makes the kubelet of a paired node reachable for the
// kube-apiserver again: open opens a data channel to it, like the open of
// prepareJoin. A node that has a listener gets the new open: after a
// hibernation, the old one belongs to a worker that is gone.
func restoreNode(name string, open js.Value) error {
	pairedMu.Lock()
	p, ok := paired[name]
	pairedMu.Unlock()
	if !ok {
		return fmt.Errorf("%s is not a paired node", name)
	}
	if p.Kind != "node" {
		return nil // kubectl: the agent only opens connections to the kube-apiserver
	}
	nodesMu.Lock()
	defer nodesMu.Unlock()
	if jn := nodes[name]; jn != nil {
		jn.setOpen(open)
		return nil
	}
	ln, err := net.Listen("tcp", p.IP+":10250")
	if err != nil {
		return err
	}
	jn := &joinedNode{name: name, ip: p.IP, open: open, ln: ln}
	nodes[name] = jn
	go jn.serve()
	return nil
}

// unjoin removes a paired node: its pairing, the node and its pods. The
// agent keeps waiting for offers until its container is removed.
func unjoin(ctx context.Context, name string) error {
	if !isPaired(name) {
		return fmt.Errorf("%s is not a paired node", name)
	}
	cs, err := adminClient()
	if err != nil {
		return err
	}
	err = cs.CoreV1().Secrets("kube-system").Delete(ctx, pairingSecret(name), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	pairedMu.Lock()
	p := paired[name]
	delete(paired, name)
	pairedMu.Unlock()
	if p.Kind == "kubectl" {
		// Its token ends with its ServiceAccount.
		sa := "tabernetes-" + name
		err := cs.RbacV1().ClusterRoleBindings().Delete(ctx, sa, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		err = cs.CoreV1().ServiceAccounts("kube-system").Delete(ctx, sa, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}
	dropNode(name)
	return nil
}

// prepareKubectl pairs a kubectl outside the browser (join --kubectl): a
// ServiceAccount kube-system/tabernetes-kubectl-<n> with cluster-admin and
// a token for a year. It returns what the token of the agent needs.
func (c *cluster) prepareKubectl(ctx context.Context, relay string) (map[string]any, error) {
	n := 1
	for _, p := range pairings() {
		var i int
		if _, err := fmt.Sscanf(p.Name, "kubectl-%d", &i); err == nil && i >= n {
			n = i + 1
		}
	}
	name := fmt.Sprintf("kubectl-%d", n)
	sa := "tabernetes-" + name
	cs, err := adminClient()
	if err != nil {
		return nil, err
	}
	_, err = cs.CoreV1().ServiceAccounts("kube-system").Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: sa, Labels: map[string]string{pairingLabel: name}},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, err
	}
	_, err = cs.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: sa, Labels: map[string]string{pairingLabel: name}},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: sa, Namespace: "kube-system"}},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, err
	}
	tr, err := cs.CoreV1().ServiceAccounts("kube-system").CreateToken(ctx, sa, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: ptr.To(int64(365 * 24 * 3600))},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating the token: %w", err)
	}
	p := pairing{Kind: "kubectl", Name: name, Relay: relay, Topic: "tabernetes-" + randToken(24), Key: randKey()}
	if err := savePairing(ctx, p); err != nil {
		return nil, fmt.Errorf("saving the pairing: %w", err)
	}
	return map[string]any{
		"name":    name,
		"token":   tr.Status.Token,
		"ca":      string(c.ca.caPEM),
		"pairing": p.js(),
	}, nil
}
