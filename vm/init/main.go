// Command init is PID 1 of a pod that runs in v86 on a tab node
// (k8s/tabernetes/v86.go). The kernel starts it as /init of an initramfs,
// where it waits for the pod (stage0.go): the page may save the VM at that
// point and restore it for later pods. Then it mounts the layers of the
// image over 9p, with the container as /.tabernetes/config.json on top, and
// makes that the root file system.
//
// It does what containerd and runc do for a container: mount /proc, /sys and
// /dev, write /etc/hosts and /etc/resolv.conf, set the hostname, then start
// the entrypoint with the environment, working directory and user of the
// container. stage0 has set up eth0. The console (ttyS0) is stdout and stderr of the container; the second serial
// port (ttyS1) tells the page "started", "exit <code>" and, if the
// entrypoint did not start, "error <message>" before that. kubectl exec and
// attach come in over the network (exec.go).
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// config is the container, written by the page (web/src/vmworker.js).
type config struct {
	Args     []string `json:"args"`
	Env      []string `json:"env"`
	Cwd      string   `json:"cwd"`
	User     string   `json:"user"`
	Hostname string   `json:"hostname"`
	IP       string   `json:"ip"`
	Gateway  string   `json:"gateway"`
	// stdin, stdinOnce and tty of the container (kubectl run -it)
	Stdin     bool `json:"stdin"`
	StdinOnce bool `json:"stdinOnce"`
	TTY       bool `json:"tty"`
}

var control *os.File

func main() {
	var code int
	var err error
	if inInitramfs() {
		err = stage0()
	} else {
		control, _ = os.OpenFile("/dev/ttyS1", os.O_WRONLY, 0)
	}
	if err == nil {
		code, err = run()
	}
	if err != nil {
		// The kubelet reports a container that did not start as StartError
		// with exit code 128, and the error in the events, not in the log.
		if control != nil {
			report("error %s", strings.ReplaceAll(err.Error(), "\n", " "))
		} else {
			fmt.Fprintln(os.Stderr, "tabernetes init:", err)
		}
		if code == 0 {
			code = 128
		}
	}
	report("exit %d", code)
	// The page ends the VM. PID 1 must not exit (the kernel panics), and the
	// kernel cannot power off without ACPI: it would say so in the log.
	select {}
}

func report(format string, a ...any) {
	if control != nil {
		fmt.Fprintf(control, format+"\n", a...)
	}
}

func run() (int, error) {
	for _, m := range []struct{ fs, dir, data string }{
		{"proc", "/proc", ""},
		{"sysfs", "/sys", ""},
		{"devtmpfs", "/dev", ""},
		{"devpts", "/dev/pts", "newinstance,ptmxmode=0666"},
		{"tmpfs", "/dev/shm", "mode=1777"},
	} {
		os.MkdirAll(m.dir, 0o755)
		if err := syscall.Mount(m.fs, m.dir, m.fs, 0, m.data); err != nil && !errors.Is(err, syscall.EBUSY) {
			return 0, fmt.Errorf("mount %s: %w", m.dir, err)
		}
	}
	// programs open /dev/ptmx, the pairs are in the instance of /dev/pts
	os.Remove("/dev/ptmx")
	os.Symlink("pts/ptmx", "/dev/ptmx")
	// What a container runtime puts into /dev: the log files of nginx are
	// links to /dev/stdout and /dev/stderr. A container that does not run as
	// root opens the console through them, too.
	os.Symlink("/proc/self/fd", "/dev/fd")
	os.Symlink("/proc/self/fd/0", "/dev/stdin")
	os.Symlink("/proc/self/fd/1", "/dev/stdout")
	os.Symlink("/proc/self/fd/2", "/dev/stderr")
	os.Chmod("/dev/console", 0o666)
	// /dev was mounted after the open above
	if control == nil {
		control, _ = os.OpenFile("/dev/ttyS1", os.O_WRONLY, 0)
	}
	console, err := os.OpenFile("/dev/console", os.O_WRONLY, 0)
	if err != nil {
		return 0, err
	}
	os.Stdout, os.Stderr = console, console

	b, err := os.ReadFile("/.tabernetes/config.json")
	if err != nil {
		return 0, err
	}
	var c config
	if err := json.Unmarshal(b, &c); err != nil {
		return 0, fmt.Errorf("config.json: %w", err)
	}
	if len(c.Args) == 0 {
		return 0, errors.New("no command specified")
	}

	// what the kubelet mounts into every container
	if c.Hostname != "" {
		syscall.Sethostname([]byte(c.Hostname))
	}
	os.MkdirAll("/etc", 0o755)
	hosts := "# Kubernetes-managed hosts file.\n127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n"
	if c.IP != "" {
		hosts += c.IP + "\t" + c.Hostname + "\n"
	}
	os.WriteFile("/etc/hosts", []byte(hosts), 0o644)
	if c.Gateway != "" {
		os.WriteFile("/etc/resolv.conf", []byte("nameserver "+c.Gateway+"\n"), 0o644)
		// The network of the page learns the MAC of eth0 from the first
		// packet: send one, so that port-forward finds the pod.
		if u, err := net.Dial("udp4", net.JoinHostPort(c.Gateway, "9")); err == nil {
			u.Write([]byte{0})
			u.Close()
		}
	}

	env := c.Env
	if !hasEnv(env, "PATH") {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	if !hasEnv(env, "HOSTNAME") && c.Hostname != "" {
		env = append(env, "HOSTNAME="+c.Hostname)
	}
	cred, home, err := lookupUser(c.User)
	if err != nil {
		return 0, err
	}
	if !hasEnv(env, "HOME") {
		env = append(env, "HOME="+home)
	}
	path, err := lookPath(c.Args[0], getEnv(env, "PATH"))
	if err != nil {
		return 128, err
	}
	dir := c.Cwd
	if dir == "" {
		dir = "/"
	}
	ctr := &process{env: env, dir: dir, cred: cred}
	go reaper()
	main, err := ctr.startMain(path, c, console)
	if err != nil {
		return 128, err
	}
	go serveExec(ctr)
	report("started")
	return exitCode(<-main), nil
}

// exitCode is the exit code of a process as a shell reports it.
func exitCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

func hasEnv(env []string, key string) bool {
	_, ok := lookupEnv(env, key)
	return ok
}

func getEnv(env []string, key string) string {
	v, _ := lookupEnv(env, key)
	return v
}

func lookupEnv(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, _ := strings.Cut(env[i], "="); k == key {
			return v, true
		}
	}
	return "", false
}

// lookPath finds name in the PATH of the container, not of init.
func lookPath(name, path string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("exec: %q: executable file not found in $PATH", name)
}

// lookupUser resolves the USER of the image: uid, uid:gid, name or
// name:group, as runc does with /etc/passwd and /etc/group of the image.
func lookupUser(spec string) (*syscall.Credential, string, error) {
	if spec == "" {
		return nil, "/root", nil
	}
	u, g, hasGroup := strings.Cut(spec, ":")
	uid, gid, home := -1, -1, "/"
	if n, err := strconv.Atoi(u); err == nil {
		uid = n
	}
	for _, f := range readDB("/etc/passwd") {
		if len(f) < 6 {
			continue
		}
		if f[0] == u || f[2] == u {
			uid, _ = strconv.Atoi(f[2])
			gid, _ = strconv.Atoi(f[3])
			home = f[5]
			break
		}
	}
	if uid < 0 {
		return nil, "", fmt.Errorf("unable to find user %s: no matching entries in passwd file", u)
	}
	if hasGroup {
		gid = -1
		if n, err := strconv.Atoi(g); err == nil {
			gid = n
		}
		for _, f := range readDB("/etc/group") {
			if len(f) >= 3 && f[0] == g {
				gid, _ = strconv.Atoi(f[2])
			}
		}
		if gid < 0 {
			return nil, "", fmt.Errorf("unable to find group %s: no matching entries in group file", g)
		}
	}
	if gid < 0 {
		gid = 0
	}
	if uid == 0 && home == "/" {
		home = "/root"
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), NoSetGroups: true}, home, nil
}

func readDB(path string) [][]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var rows [][]string
	s := bufio.NewScanner(f)
	for s.Scan() {
		if line := s.Text(); line != "" && !strings.HasPrefix(line, "#") {
			rows = append(rows, strings.Split(line, ":"))
		}
	}
	return rows
}
