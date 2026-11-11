package main

// Stage 0: the boot before the pod. The page boots the kernel with this
// program as /init of an initramfs and an empty 9p file system. init
// reports "ready" on ttyS1 and waits for one line from the page. At that
// point the page may save the state of the VM: every later pod with the same
// memory size starts from that state instead of booting the kernel again
// (web/src/vmworker.js).
//
// The line is "go <unix time in ms> <random bytes in hex>". A restored VM has
// the clock and the random number generator of the saved one, so init sets
// both before anything else runs. Then it mounts the 9p file system with the
// layers of the pod, sets up eth0 and makes the 9p file system the root.

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const newRoot = "/newroot"

// inInitramfs tells whether init runs as /init of the initramfs, before the
// root file system of the pod.
func inInitramfs() bool {
	return os.Getpid() == 1 && os.Args[0] == "/init"
}

func stage0() error {
	os.MkdirAll("/dev", 0o755)
	if err := syscall.Mount("devtmpfs", "/dev", "devtmpfs", 0, ""); err != nil && !errors.Is(err, syscall.EBUSY) {
		return fmt.Errorf("mount /dev: %w", err)
	}
	tty, err := os.OpenFile("/dev/ttyS1", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	noEcho(tty)
	control = tty
	report("ready")
	// The page saves the VM here, while init waits for the line.
	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read ttyS1: %w", err)
	}
	f := strings.Fields(line)
	if len(f) != 3 || f[0] != "go" {
		return fmt.Errorf("ttyS1: unexpected %q", line)
	}
	ms, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return fmt.Errorf("ttyS1: time: %w", err)
	}
	tv := syscall.NsecToTimeval(ms * 1e6)
	if err := syscall.Settimeofday(&tv); err != nil {
		return fmt.Errorf("settimeofday: %w", err)
	}
	seed, err := hex.DecodeString(f[2])
	if err != nil {
		return fmt.Errorf("ttyS1: seed: %w", err)
	}
	if err := addEntropy(seed); err != nil {
		return err
	}

	os.MkdirAll(newRoot, 0o755)
	// cache=loose: only the guest changes the files, it may keep them
	if err := syscall.Mount("host9p", newRoot, "9p", 0, "trans=virtio,version=9p2000.L,cache=loose,msize=262144"); err != nil {
		return fmt.Errorf("mount 9p: %w", err)
	}
	b, err := os.ReadFile(newRoot + "/.tabernetes/config.json")
	if err != nil {
		return err
	}
	var c config
	if err := json.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("config.json: %w", err)
	}
	if err := setupNetwork(c.IP, c.Gateway); err != nil {
		return err
	}
	// What switch_root does. The initramfs keeps only this program.
	syscall.Unmount("/dev", syscall.MNT_DETACH)
	if err := os.Chdir(newRoot); err != nil {
		return err
	}
	if err := syscall.Mount(".", "/", "", syscall.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move the root: %w", err)
	}
	if err := syscall.Chroot("."); err != nil {
		return fmt.Errorf("chroot: %w", err)
	}
	return os.Chdir("/")
}

func noEcho(f *os.File) {
	var t syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t))); e != 0 {
		return
	}
	t.Lflag &^= syscall.ECHO | syscall.ECHONL
	syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCSETS, uintptr(unsafe.Pointer(&t)))
}

// addEntropy mixes the seed of the page into the random number generator
// and reseeds it, so that two pods from the same state differ.
func addEntropy(seed []byte) error {
	const rndAddEntropy, rndReseedCRNG = 0x40085203, 0x5207
	f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	// struct rand_pool_info: entropy_count (bits), buf_size, buf
	b := make([]byte, 8+len(seed))
	binary.LittleEndian.PutUint32(b[0:], uint32(len(seed)*8))
	binary.LittleEndian.PutUint32(b[4:], uint32(len(seed)))
	copy(b[8:], seed)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), rndAddEntropy, uintptr(unsafe.Pointer(&b[0]))); e != 0 {
		return fmt.Errorf("RNDADDENTROPY: %w", e)
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), rndReseedCRNG, 0); e != 0 {
		return fmt.Errorf("RNDRESEEDCRNG: %w", e)
	}
	return nil
}

// setupNetwork does what ip= on the kernel command line did: lo and eth0 up,
// the address of the pod on eth0 (/24), the default route to the gateway.
func setupNetwork(ip, gateway string) error {
	s, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(s)
	if err := ifUp(s, "lo"); err != nil {
		return err
	}
	if ip == "" {
		return nil
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return err
	}
	if err := ifAddr(s, "eth0", syscall.SIOCSIFADDR, addr); err != nil {
		return err
	}
	if err := ifAddr(s, "eth0", syscall.SIOCSIFNETMASK, netip.AddrFrom4([4]byte{255, 255, 255, 0})); err != nil {
		return err
	}
	if err := ifUp(s, "eth0"); err != nil {
		return err
	}
	if gateway == "" {
		return nil
	}
	gw, err := netip.ParseAddr(gateway)
	if err != nil {
		return err
	}
	// struct rtentry: dst, gateway and genmask (sockaddr_in each) after an
	// unsigned long, then the flags.
	var rt [128]byte
	w := int(unsafe.Sizeof(uintptr(0)))
	sockaddr(rt[w:], netip.IPv4Unspecified())
	sockaddr(rt[w+16:], gw)
	sockaddr(rt[w+32:], netip.IPv4Unspecified())
	binary.LittleEndian.PutUint16(rt[w+48:], syscall.RTF_UP|syscall.RTF_GATEWAY)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(s), syscall.SIOCADDRT, uintptr(unsafe.Pointer(&rt[0]))); e != 0 {
		return fmt.Errorf("add default route: %w", e)
	}
	return nil
}

// ifreq is struct ifreq: the name and a union (sockaddr or flags).
type ifreq struct {
	name [16]byte
	data [24]byte
}

func newIfreq(name string) *ifreq {
	r := &ifreq{}
	copy(r.name[:], name)
	return r
}

func ifUp(s int, name string) error {
	r := newIfreq(name)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(s), syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(r))); e != 0 {
		return fmt.Errorf("%s: SIOCGIFFLAGS: %w", name, e)
	}
	flags := binary.LittleEndian.Uint16(r.data[:]) | syscall.IFF_UP
	binary.LittleEndian.PutUint16(r.data[:], flags)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(s), syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(r))); e != 0 {
		return fmt.Errorf("%s: SIOCSIFFLAGS: %w", name, e)
	}
	return nil
}

func ifAddr(s int, name string, req uintptr, a netip.Addr) error {
	r := newIfreq(name)
	sockaddr(r.data[:], a)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(s), req, uintptr(unsafe.Pointer(r))); e != 0 {
		return fmt.Errorf("%s: ioctl %#x %s: %w", name, req, a, e)
	}
	return nil
}

// sockaddr writes a struct sockaddr_in.
func sockaddr(b []byte, a netip.Addr) {
	binary.LittleEndian.PutUint16(b, syscall.AF_INET)
	ip := a.As4()
	copy(b[4:8], ip[:])
}
