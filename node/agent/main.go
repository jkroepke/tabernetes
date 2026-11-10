// tabernetes joins a Kubernetes node to a tabernetes control plane running in a
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
		fmt.Fprintf(os.Stderr, "usage: tabernetes [flags] join <token>\n")
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

	m, err := newMesh(ctx, cancel, tok, *stun, *advertise)
	if err != nil {
		log.Fatal(err)
	}
	defer m.close()
	pc, err := m.newPeer()
	if err != nil {
		log.Fatal(err)
	}
	connected := make(chan struct{})
	var once sync.Once
	m.add(pc, func() { once.Do(func() { close(connected) }) })

	sdp, err := answerOffer(ctx, pc, tok.SDP)
	if err != nil {
		log.Fatal(err)
	}
	answer, err := encode(answerToken{V: 1, SDP: sdp})
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
	for _, addr := range []string{"127.0.0.1:6443", apiserverIP + ":6443"} {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			log.Printf("listen %s: %v", addr, err)
			continue
		}
		go m.serve(ctx, l)
	}

	if !*noKubelet {
		if err := runNode(ctx, tok); err != nil {
			log.Fatal(err)
		}
	}
	<-ctx.Done()
}

// mesh is the set of WebRTC peer connections to the browser: one to every
// tab of the page. The first one comes from the token. Every further tab
// sends its offer through a peer that is already connected (a data channel
// "signal"), so the node lives on as long as one of the tabs is open. The
// agent ends once no peer is left.
type mesh struct {
	ctx    context.Context
	cancel context.CancelFunc
	api    *webrtc.API
	cfg    webrtc.Configuration
	mux    ice.UDPMux

	mu    sync.Mutex
	peers map[*webrtc.PeerConnection]bool // connected
	next  int
}

func newMesh(ctx context.Context, cancel context.CancelFunc, tok offerToken, stun, advertise string) (*mesh, error) {
	m := &mesh{ctx: ctx, cancel: cancel, peers: map[*webrtc.PeerConnection]bool{}}
	se := webrtc.SettingEngine{}
	// The ICE checks start with the answer, but the browser only joins them
	// once the "accept" line is pasted. pion gives up after 30s by default;
	// wait as long as for the browser in main.
	se.SetICETimeouts(10*time.Second, 10*time.Minute, 2*time.Second)
	se.DetachDataChannels()
	// Writes wait until SCTP has sent what is queued. Without it a fast
	// sender (kubectl logs -f, kubectl cp) queues without limit in memory.
	se.EnableDataChannelBlockWrite(true)
	if tok.Port > 0 {
		// One socket for host and STUN candidates on the fixed port that
		// `docker run -p` publishes. A port range of one port would make every
		// further candidate (the advertised address, STUN) fail to bind. The
		// peers of all tabs share it.
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: tok.Port})
		if err != nil {
			return nil, err
		}
		m.mux = ice.NewUniversalUDPMuxDefault(ice.UniversalUDPMuxParams{UDPConn: conn})
		se.SetICEUDPMux(m.mux)
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
	m.api = webrtc.NewAPI(webrtc.WithSettingEngine(se))
	if stun != "" {
		m.cfg.ICEServers = []webrtc.ICEServer{{URLs: []string{stun}}}
	}
	return m, nil
}

func (m *mesh) newPeer() (*webrtc.PeerConnection, error) {
	return m.api.NewPeerConnection(m.cfg)
}

// add watches pc: onConnected runs once it is connected, and the agent ends
// when the last peer fails or closes.
func (m *mesh) add(pc *webrtc.PeerConnection, onConnected func()) {
	m.mu.Lock()
	m.next++
	n := m.next
	m.peers[pc] = false
	m.mu.Unlock()
	pc.OnDataChannel(func(dc *webrtc.DataChannel) { m.onDataChannel(dc) })
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if _, ok := m.peers[pc]; !ok {
			return
		}
		log.Printf("webrtc: tab %d %s", n, s)
		switch s {
		case webrtc.PeerConnectionStateConnected:
			m.peers[pc] = true
			if onConnected != nil {
				go onConnected()
			}
		case webrtc.PeerConnectionStateDisconnected:
			// A closed tab may end here without a goodbye, and ICE only
			// gives up on it after minutes. Without any connected tab for
			// a while, the browser is gone.
			m.peers[pc] = false
			go m.orphaned()
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			delete(m.peers, pc)
			go pc.Close()
			if len(m.peers) == 0 {
				log.Print("webrtc: no tab is left")
				m.cancel()
			}
		}
	})
}

// orphanTimeout is how long the agent waits for a tab when none is
// connected any more.
const orphanTimeout = 30 * time.Second

func (m *mesh) orphaned() {
	select {
	case <-m.ctx.Done():
		return
	case <-time.After(orphanTimeout):
	}
	if len(m.connected()) == 0 {
		log.Printf("webrtc: no tab connected for %s", orphanTimeout)
		m.cancel()
	}
}

// connected returns the connected peers.
func (m *mesh) connected() []*webrtc.PeerConnection {
	m.mu.Lock()
	defer m.mu.Unlock()
	var c []*webrtc.PeerConnection
	for pc, ok := range m.peers {
		if ok {
			c = append(c, pc)
		}
	}
	return c
}

func (m *mesh) close() {
	m.mu.Lock()
	peers := m.peers
	m.peers = map[*webrtc.PeerConnection]bool{}
	m.mu.Unlock()
	for pc := range peers {
		pc.Close()
	}
	if m.mux != nil {
		m.mux.Close()
	}
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
	return pc.LocalDescription().SDP, nil
}

// signal answers the offer of another tab: one JSON message {"sdp": ...}
// each way on a data channel "signal".
func (m *mesh) signal(raw io.ReadWriteCloser) {
	defer raw.Close()
	buf := make([]byte, 64*1024)
	n, err := raw.Read(buf)
	if err != nil {
		return
	}
	var req struct {
		SDP string `json:"sdp"`
	}
	if err := json.Unmarshal(buf[:n], &req); err != nil {
		log.Printf("signal: %v", err)
		return
	}
	pc, err := m.newPeer()
	if err != nil {
		log.Printf("signal: %v", err)
		return
	}
	m.add(pc, nil)
	sdp, err := answerOffer(m.ctx, pc, req.SDP)
	if err != nil {
		log.Printf("signal: %v", err)
		pc.Close()
		return
	}
	b, _ := json.Marshal(map[string]string{"sdp": sdp})
	raw.Write(b)
}

// serve forwards local TCP connections (kubelet, kube-proxy, pods) to the
// kube-apiserver in the browser, through any connected tab.
func (m *mesh) serve(ctx context.Context, l net.Listener) {
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			raw := m.openAPI()
			if raw == nil {
				c.Close()
				return
			}
			splice(c, raw)
		}()
	}
}

// openAPI opens a data channel to the kube-apiserver. A tab that was just
// closed may still count as connected for some seconds: then the channel
// does not open, and the next tab gets its turn.
func (m *mesh) openAPI() io.ReadWriteCloser {
	ordered := true
	for _, pc := range m.connected() {
		dc, err := pc.CreateDataChannel("api", &webrtc.DataChannelInit{Ordered: &ordered})
		if err != nil {
			continue
		}
		opened := make(chan io.ReadWriteCloser, 1)
		dc.OnOpen(func() {
			raw, err := dc.Detach()
			if err != nil {
				raw = nil
			}
			opened <- raw
		})
		select {
		case raw := <-opened:
			if raw != nil {
				return raw
			}
		case <-time.After(3 * time.Second):
			dc.Close()
		case <-m.ctx.Done():
			return nil
		}
	}
	return nil
}

// onDataChannel takes the channels a tab opens: "kubelet:<port>" from the
// kube-apiserver to the kubelet (logs, exec, port-forward), and "signal".
func (m *mesh) onDataChannel(dc *webrtc.DataChannel) {
	label := dc.Label()
	switch label {
	case "ctl":
		// The browser only opens "ctl" so the offer carries an SCTP section; nothing to do.
		return
	case "signal":
		dc.OnOpen(func() {
			if raw, err := dc.Detach(); err == nil {
				m.signal(raw)
			}
		})
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
- name: tabernetes
  cluster:
    server: https://%s:6443
    certificate-authority: /etc/kubernetes/pki/ca.crt
contexts:
- name: tabernetes
  context: {cluster: tabernetes, user: kubelet-bootstrap}
current-context: tabernetes
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
# flannel, kube-proxy, CoreDNS and pause are pulled on every join: in parallel.
serializeImagePulls: false
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
	// Now and then a registry download stops in the middle and never sends
	// another byte (seen on WSL2 with Docker Desktop, with and without
	// pasta). containerd cancels a pull without progress, and the next try
	// opens new connections and works.
	// Do that after 20 seconds instead of the default five minutes. Shorter
	// would also cancel healthy downloads that wait for a few TCP retransmits,
	// and each failed pull costs a growing kubelet back-off.
	//
	// The default pull through the transfer service counts a layer as active
	// only after it reported progress, so a download that stalls in its first
	// bytes is never cancelled. The local pull counts every HTTP request from
	// its start.
	containerdConfig := "version = 3\n" +
		"[plugins.'io.containerd.cri.v1.images']\n" +
		"  image_pull_progress_timeout = '20s'\n" +
		"  use_local_image_pull = true\n"
	if startImageMirror() {
		containerdConfig += "[plugins.'io.containerd.cri.v1.images'.registry]\n" +
			"  config_path = '" + registryConfigDir + "'\n"
	}
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
	if err := writeFile("/etc/tabernetes/containerd.toml", containerdConfig, 0o644); err != nil {
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
	go supervise(ctx, "containerd", "containerd", "--config=/etc/tabernetes/containerd.toml")
	if err := waitForSocket(ctx, "/run/containerd/containerd.sock", 30*time.Second); err != nil {
		return fmt.Errorf("containerd did not start, see /var/log/containerd.log: %w", err)
	}
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

func waitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		c, err := net.Dial("unix", path)
		if err == nil {
			c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no socket at %s after %s: %w", path, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
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
			if os.Getenv("TABERNETES_VERBOSE") != "" || (strings.HasPrefix(line, "E") || strings.HasPrefix(line, "F")) && !isNoise(line) {
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
	// Every 5 minutes: in the user namespace the kubelet may not read
	// /proc/1/ns/pid of the agent, so it skips its own OOM score.
	"failed to find pid namespace of init process",
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
