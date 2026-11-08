package main

// Without CAP_SYS_ADMIN (docker run without --privileged and without
// --cap-add SYS_ADMIN), the agent runs itself again in new user, mount,
// network, cgroup, UTS and IPC namespaces. In there it is root with all capabilities,
// but only over its own namespaces. pasta connects the new network namespace
// to the container's network and forwards the WebRTC UDP port.

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

const usernsEnv = "TABERNETES_USERNS"

// inUserNS is true in the agent that runs in its own user namespace.
var inUserNS = os.Getenv(usernsEnv) != ""

// hasCapSysAdmin reports whether CAP_SYS_ADMIN is in the effective set.
func hasCapSysAdmin() bool {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for line := range strings.Lines(string(status)) {
		if v, ok := strings.CutPrefix(line, "CapEff:"); ok {
			caps, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
			const capSysAdmin = 21
			return err == nil && caps&(1<<capSysAdmin) != 0
		}
	}
	return false
}

// runInUserNS starts the agent again in new namespaces, connects the network
// with pasta and returns the exit code of the agent in there.
func runInUserNS(udpPort int) int {
	r, w, err := os.Pipe()
	if err != nil {
		log.Fatal(err)
	}
	cmd := exec.Command("/proc/self/exe", os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), usernsEnv+"=1")
	cmd.ExtraFiles = []*os.File{r} // fd 3: closed when the network is ready
	// uid and gid 0-65535 stay the same, so files keep their owners. Root in
	// there has capabilities only over the new namespaces.
	ids := []syscall.SysProcIDMap{{ContainerID: 0, HostID: 0, Size: 65536}}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// Pods with hostNetwork or hostIPC join these, so they must belong
		// to the new user namespace.
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWNET |
			syscall.CLONE_NEWCGROUP | syscall.CLONE_NEWUTS | syscall.CLONE_NEWIPC,
		UidMappings:                ids,
		GidMappings:                ids,
		GidMappingsEnableSetgroups: true,
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("user namespace: %v (needs --security-opt seccomp=unconfined)", err)
	}
	r.Close()

	udp := "none"
	if udpPort > 0 {
		udp = strconv.Itoa(udpPort)
	}
	// pasta copies the container's address and routes into the namespace,
	// NATs outgoing traffic and forwards the WebRTC port. It ends when the
	// namespace is gone.
	pasta := exec.Command("pasta", "--runas", "0", "--config-net", "--quiet",
		"-u", udp, "-t", "none", "-T", "none", "-U", "none",
		strconv.Itoa(cmd.Process.Pid))
	pasta.Stdout, pasta.Stderr = os.Stderr, os.Stderr
	if err := pasta.Run(); err != nil {
		_ = cmd.Process.Kill()
		log.Fatalf("pasta: %v (needs --device /dev/net/tun)", err)
	}
	w.Close()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for s := range sig {
			_ = cmd.Process.Signal(s)
		}
	}()
	if err := cmd.Wait(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		log.Print(err)
		return 1
	}
	return 0
}

// setupUserNS runs first in the agent in the new namespaces: it waits for
// the network and mounts the file systems that belong to the namespaces.
func setupUserNS() error {
	ready := os.NewFile(3, "ready")
	_, _ = ready.Read(make([]byte, 1)) // EOF when pasta is done
	ready.Close()

	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}
	const flags = syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC
	// sysfs for the new network namespace. The kernel refuses it when the
	// container's /sys hides parts of it, then the old one stays.
	if err := syscall.Mount("sysfs", "/sys", "sysfs", flags|syscall.MS_RDONLY, ""); err != nil {
		log.Printf("sysfs: %v (keeping the container's /sys)", err)
	}
	// A cgroup2 mount of the new cgroup namespace, writable for the kubelet.
	if err := syscall.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", flags, ""); err != nil {
		return fmt.Errorf("cgroup2: %w", err)
	}
	return nil
}
