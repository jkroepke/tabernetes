// kwasm-node joins a Kubernetes node to a kwasm control plane running in a
// web browser. The connection to the browser is a WebRTC peer connection;
// every TCP connection is carried by its own data channel.
package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/vishvananda/netlink"
)

const apiserverIP = "10.0.0.1"

type offerToken struct {
	V      int    `json:"v"`
	Name   string `json:"n"`
	IP     string `json:"ip"`
	Token  string `json:"t"`
	CA     string `json:"ca"`
	SDP    string `json:"sdp"`
	Port   int    `json:"p"`
	Server string `json:"s,omitempty"`
}

type answerToken struct {
	V   int    `json:"v"`
	SDP string `json:"sdp"`
}

func encode(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestCompression)
	w.Write(raw)
	w.Close()
	return base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}

func decode(s string, v any) error {
	s = strings.TrimSpace(s)
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return fmt.Errorf("token is not valid base64url: %w", err)
	}
	raw, err := io.ReadAll(flate.NewReader(bytes.NewReader(b)))
	if err != nil {
		return fmt.Errorf("token is corrupt: %w", err)
	}
	return json.Unmarshal(raw, v)
}

func main() {
	if filepath.Base(os.Args[0]) == filepath.Base(runcWrapper) {
		runRuncWrapper(os.Args[1:])
		return
	}
	stun := flag.String("stun", "stun:stun.l.google.com:19302", "STUN server, empty to disable")
	noKubelet := flag.Bool("no-kubelet", false, "only run the tunnel (for testing)")
	advertise := flag.String("advertise", "127.0.0.1", "extra host ICE candidate addresses, comma separated (for docker -p port mappings), empty to disable")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: kwasm-node [flags] join <token>\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	args := flag.Args()
	if len(args) == 1 {
		args = []string{"join", args[0]}
	}
	if len(args) != 2 || args[0] != "join" {
		flag.Usage()
		os.Exit(2)
	}
	var tok offerToken
	if err := decode(args[1], &tok); err != nil {
		log.Fatal(err)
	}
	if tok.V != 1 {
		log.Fatalf("unsupported token version %d", tok.V)
	}

	if inUserNS {
		if err := setupUserNS(); err != nil {
			log.Fatal(err)
		}
	} else if !*noKubelet && !hasCapSysAdmin() {
		os.Exit(runInUserNS(tok.Port))
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	pc, err := newPeer(tok, *stun, *advertise)
	if err != nil {
		log.Fatal(err)
	}
	defer pc.Close()
	pc.OnDataChannel(onDataChannel)

	connected := make(chan struct{})
	var once sync.Once
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("webrtc: connection %s", s)
		switch s {
		case webrtc.PeerConnectionStateConnected:
			once.Do(func() { close(connected) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			cancel()
		}
	})

	answer, err := answerOffer(ctx, pc, tok.SDP)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n\033[1mPaste this into the browser terminal:\033[0m\n\naccept %s\n\n", answer)

	select {
	case <-connected:
	case <-time.After(10 * time.Minute):
		log.Fatal("timed out waiting for the browser")
	case <-ctx.Done():
		return
	}
	log.Printf("connected to the control plane in the browser, joining as %s (%s)", tok.Name, tok.IP)

	if !*noKubelet {
		for _, ip := range []string{apiserverIP, tok.IP} {
			if err := addLoopbackIP(ip); err != nil {
				log.Fatalf("adding %s to lo: %v", ip, err)
			}
		}
	}
	t := &tunnel{pc: pc}
	for _, addr := range []string{"127.0.0.1:6443", apiserverIP + ":6443"} {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			log.Printf("listen %s: %v", addr, err)
			continue
		}
		go t.serve(ctx, l)
	}

	if !*noKubelet {
		if err := runNode(ctx, tok); err != nil {
			log.Fatal(err)
		}
	}
	<-ctx.Done()
}

func newPeer(tok offerToken, stun, advertise string) (*webrtc.PeerConnection, error) {
	se := webrtc.SettingEngine{}
	// The ICE checks start with the answer, but the browser only joins them
	// once the "accept" line is pasted. pion gives up after 30s by default;
	// wait as long as for the browser in main.
	se.SetICETimeouts(10*time.Second, 10*time.Minute, 2*time.Second)
	se.DetachDataChannels()
	if tok.Port > 0 {
		// One socket for host and STUN candidates on the fixed port that
		// `docker run -p` publishes. A port range of one port would make every
		// further candidate (the advertised address, STUN) fail to bind.
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: tok.Port})
		if err != nil {
			return nil, err
		}
		se.SetICEUDPMux(ice.NewUniversalUDPMuxDefault(ice.UniversalUDPMuxParams{UDPConn: conn}))
	}
	if advertise != "" {
		if err := se.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        strings.Split(advertise, ","),
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteAppend,
		}); err != nil {
			return nil, err
		}
	}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	cfg := webrtc.Configuration{}
	if stun != "" {
		cfg.ICEServers = []webrtc.ICEServer{{URLs: []string{stun}}}
	}
	return api.NewPeerConnection(cfg)
}

func answerOffer(ctx context.Context, pc *webrtc.PeerConnection, sdp string) (string, error) {
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		return "", fmt.Errorf("invalid offer: %w", err)
	}
	ans, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(ans); err != nil {
		return "", err
	}
	select {
	case <-gathered:
	case <-time.After(10 * time.Second):
		log.Print("ICE gathering is slow, sending what we have")
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return encode(answerToken{V: 1, SDP: pc.LocalDescription().SDP})
}

type tunnel struct {
	pc *webrtc.PeerConnection
}

// serve forwards local TCP connections (kubelet, kube-proxy, pods) to the
// kube-apiserver in the browser.
func (t *tunnel) serve(ctx context.Context, l net.Listener) {
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			ordered := true
			dc, err := t.pc.CreateDataChannel("api", &webrtc.DataChannelInit{Ordered: &ordered})
			if err != nil {
				log.Printf("create data channel: %v", err)
				c.Close()
				return
			}
			dc.OnOpen(func() {
				raw, err := dc.Detach()
				if err != nil {
					log.Printf("detach: %v", err)
					c.Close()
					return
				}
				splice(c, raw)
			})
		}()
	}
}

func init() {
	// Channels opened by the browser: apiserver -> kubelet (logs, exec, port-forward).
	onDataChannel = func(dc *webrtc.DataChannel) {
		label := dc.Label()
		if label == "ctl" {
			// The browser only opens "ctl" so the offer carries an SCTP section; nothing to do.
			return
		}
		target, ok := strings.CutPrefix(label, "kubelet:")
		if !ok {
			log.Printf("ignoring data channel %q", label)
			dc.Close()
			return
		}
		dc.OnOpen(func() {
			raw, err := dc.Detach()
			if err != nil {
				return
			}
			c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", target), 10*time.Second)
			if err != nil {
				log.Printf("dial kubelet: %v", err)
				raw.Close()
				return
			}
			splice(c, raw)
		})
	}
}

var onDataChannel func(*webrtc.DataChannel)

// dataChannelRW turns a detached data channel into a stream: SCTP messages are
// at most 64 KiB, writes are chunked accordingly.
type dataChannelRW struct {
	io.ReadWriteCloser
}

func (d dataChannelRW) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		chunk := min(len(p), 16*1024)
		w, err := d.ReadWriteCloser.Write(p[:chunk])
		n += w
		if err != nil {
			return n, err
		}
		p = p[chunk:]
	}
	return n, nil
}

func splice(c net.Conn, raw io.ReadWriteCloser) {
	dc := dataChannelRW{raw}
	var once sync.Once
	closeBoth := func() { c.Close(); raw.Close() }
	go func() {
		buf := make([]byte, 64*1024)
		io.CopyBuffer(c, dc, buf)
		once.Do(closeBoth)
	}()
	go func() {
		buf := make([]byte, 16*1024)
		io.CopyBuffer(dc, c, buf)
		once.Do(closeBoth)
	}()
}

// ---- node bootstrap ----

func addLoopbackIP(ip string) error {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return err
	}
	addr, err := netlink.ParseAddr(ip + "/32")
	if err != nil {
		return err
	}
	return netlink.AddrReplace(lo, addr)
}

func writeFile(path, content string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), mode)
}

func runNode(ctx context.Context, tok offerToken) error {
	prepareUnprivileged()
	if err := setupCgroups(); err != nil {
		log.Printf("cgroups: %v", err)
	}
	files := map[string]string{
		"/etc/kubernetes/pki/ca.crt": tok.CA,
		"/etc/kubernetes/bootstrap-kubelet.conf": fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: kwasm
  cluster:
    server: https://%s:6443
    certificate-authority: /etc/kubernetes/pki/ca.crt
contexts:
- name: kwasm
  context: {cluster: kwasm, user: kubelet-bootstrap}
current-context: kwasm
users:
- name: kubelet-bootstrap
  user: {token: %s}
`, apiserverIP, tok.Token),
		"/var/lib/kubelet/config.yaml": `apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
authentication:
  anonymous: {enabled: false}
  webhook: {enabled: true}
  x509: {clientCAFile: /etc/kubernetes/pki/ca.crt}
authorization: {mode: Webhook}
cgroupDriver: cgroupfs
clusterDNS: [10.96.0.10]
clusterDomain: cluster.local
containerRuntimeEndpoint: unix:///run/containerd/containerd.sock
failSwapOn: false
rotateCertificates: true
resolvConf: /etc/resolv.conf
evictionHard:
  memory.available: "1Mi"
  nodefs.available: "1%"
  nodefs.inodesFree: "1%"
  imagefs.available: "1%"
imageGCHighThresholdPercent: 100
localStorageCapacityIsolation: false
failCgroupV1: false
`,
	}
	if inUserNS {
		files["/var/lib/kubelet/config.yaml"] += "featureGates:\n  KubeletInUserNamespace: true\n"
	}
	for p, c := range files {
		if err := writeFile(p, c, 0o600); err != nil {
			return err
		}
	}
	containerdConfig := "version = 3\n"
	if inUserNS {
		// No AppArmor and no lower oom_score_adj than our own in a user namespace.
		containerdConfig += "[plugins.'io.containerd.cri.v1.runtime']\n" +
			"  disable_apparmor = true\n" +
			"  restrict_oom_score_adj = true\n"
	}
	if inUserNS || seccompFiltered() {
		// containerd calls runc through the agent, see runc.go.
		containerdConfig += "[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc.options]\n" +
			"  BinaryName = '" + runcWrapper + "'\n"
	}
	if err := writeFile("/etc/kwasm/containerd.toml", containerdConfig, 0o644); err != nil {
		return err
	}
	// The kubelet reads the system UUID from /etc/machine-id.
	if _, err := os.Stat("/etc/machine-id"); err != nil {
		id := make([]byte, 16)
		if _, err := rand.Read(id); err != nil {
			return err
		}
		if err := writeFile("/etc/machine-id", hex.EncodeToString(id)+"\n", 0o444); err != nil {
			return err
		}
	}
	go supervise(ctx, "containerd", "containerd", "--config=/etc/kwasm/containerd.toml")
	waitForSocket("/run/containerd/containerd.sock", 30*time.Second)
	go supervise(ctx, "kubelet", "kubelet",
		"--bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf",
		"--kubeconfig=/var/lib/kubelet/kubeconfig",
		"--config=/var/lib/kubelet/config.yaml",
		"--hostname-override="+tok.Name,
		"--node-ip="+tok.IP,
		"--v=1",
	)
	return nil
}

func waitForSocket(path string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// supervise runs a process, prefixes its output and restarts it when it dies.
func supervise(ctx context.Context, name, bin string, args ...string) {
	if p, err := exec.LookPath(bin); err == nil {
		bin = p
	}
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, bin, args...)
		out, _ := cmd.StdoutPipe()
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			log.Printf("%s: %v", name, err)
			time.Sleep(2 * time.Second)
			continue
		}
		logFile, _ := os.OpenFile("/var/log/"+name+".log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if logFile != nil {
				fmt.Fprintln(logFile, line)
			}
			if os.Getenv("KWASM_VERBOSE") != "" || (strings.HasPrefix(line, "E") || strings.HasPrefix(line, "F")) && !isNoise(line) {
				log.Printf("%s: %s", name, line)
			}
		}
		err := cmd.Wait()
		if logFile != nil {
			logFile.Close()
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("%s exited (%v), restarting", name, err)
		time.Sleep(2 * time.Second)
	}
}

// noise are kubelet errors that are expected in a container or during the
// first seconds of a node. They stay in /var/log/kubelet.log.
var noise = []string{
	"cannot find filesystem info for device", // cadvisor cannot map the container's overlay to a disk
	"HasDedicatedImageFs",                    // same, from the eviction manager
	"checkpoint is not found",                // first start
	"PLEG is not healthy: pleg has yet to be successful",
	"not found\" node=", // node not registered yet
	"Error getting the current node from lister",
	"Error updating node status, will retry",
	"Failed to get node when trying to set owner ref to the node lease",
	"/run/flannel/subnet.env",             // pods start before flannel is ready on this node
	"Stopped receiving OOM notifications", // /dev/kmsg is /dev/null without --privileged
	// The kubelet sends events before its client certificate exists.
	`User \"system:anonymous\" cannot create resource \"events\"`,
}

func isNoise(line string) bool {
	for _, n := range noise {
		if strings.Contains(line, n) {
			return true
		}
	}
	return false
}

// prepareUnprivileged makes a container without --privileged usable for
// containerd and the kubelet. Docker mounts the cgroup tree and /proc/sys
// read-only and hides /dev/kmsg there. With --privileged it does nothing.
func prepareUnprivileged() {
	for _, p := range []string{"/sys/fs/cgroup", "/proc/sys"} {
		if err := remountRW(p); err != nil {
			log.Printf("remount %s read-write: %v", p, err)
		}
	}
	// The kubelet's OOM watcher needs to open /dev/kmsg.
	if _, err := os.Lstat("/dev/kmsg"); errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink(kmsgTarget, "/dev/kmsg"); err != nil {
			log.Printf("/dev/kmsg: %v", err)
		}
	}
}

const kmsgTarget = "/dev/null"

// seccompFiltered reports whether this process runs under a seccomp filter,
// as it does in a Docker container without --privileged.
func seccompFiltered() bool {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for line := range strings.Lines(string(status)) {
		if v, ok := strings.CutPrefix(line, "Seccomp:"); ok {
			return strings.TrimSpace(v) == "2"
		}
	}
	return false
}

func readOnly(path string) (bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false, err
	}
	const stRdonly = 0x1
	return st.Flags&stRdonly != 0, nil
}

func remountRW(path string) error {
	ro, err := readOnly(path)
	if err != nil || !ro {
		return err
	}
	const keep = syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC
	// /proc/sys is a read-only bind mount, /sys/fs/cgroup a read-only mount.
	for _, flags := range []uintptr{syscall.MS_REMOUNT | syscall.MS_BIND | keep, syscall.MS_REMOUNT | keep} {
		if err := syscall.Mount("", path, "", flags, ""); err != nil {
			return err
		}
		if ro, err = readOnly(path); err != nil || !ro {
			return err
		}
	}
	return errors.New("still read-only")
}

// setupCgroups moves all processes into a child cgroup so that the kubelet
// can enable controllers on the container's cgroup v2 root ("no internal
// processes" rule).
func setupCgroups() error {
	const root = "/sys/fs/cgroup"
	if _, err := os.Stat(root + "/cgroup.controllers"); err != nil {
		return errors.New("cgroup v2 not available")
	}
	if err := os.MkdirAll(root+"/init", 0o755); err != nil {
		return err
	}
	procs, err := os.ReadFile(root + "/cgroup.procs")
	if err != nil {
		return err
	}
	for _, pid := range strings.Fields(string(procs)) {
		_ = os.WriteFile(root+"/init/cgroup.procs", []byte(pid), 0o644)
	}
	ctrls, err := os.ReadFile(root + "/cgroup.controllers")
	if err != nil {
		return err
	}
	var enable []string
	for _, c := range strings.Fields(string(ctrls)) {
		enable = append(enable, "+"+c)
	}
	return os.WriteFile(root+"/cgroup.subtree_control", []byte(strings.Join(enable, " ")), 0o644)
}
