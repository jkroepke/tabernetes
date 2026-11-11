package main

// kubectl exec and attach. The kubelet of the tab (k8s/tabernetes/v86.go)
// connects to execPort over the network of the page, as port-forward does,
// and sends one line of JSON (execRequest). After that, both sides send
// frames: one byte type, four bytes big-endian length, the payload.

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

type execRequest struct {
	Cmd    []string `json:"cmd"`
	TTY    bool     `json:"tty"`
	Stdin  bool     `json:"stdin"`
	Attach bool     `json:"attach"`
	Rows   uint16   `json:"rows"`
	Cols   uint16   `json:"cols"`
}

// process is the container: what every process in it gets, and the main
// process, which kubectl attach connects to.
type process struct {
	env  []string
	dir  string
	cred *syscall.Credential

	main      *proc
	stdinOnce bool

	mu        sync.Mutex
	subs      map[*subscriber]struct{}
	exited    bool
	code      int
	attaches  sync.WaitGroup
	closeOnce sync.Once
}

// proc is one started process with the ends of its stdio that init holds.
type proc struct {
	pid         int
	done        <-chan syscall.WaitStatus
	stdin       *os.File // nil without stdin
	pty         *os.File // the master, with a tty
	out         []output
	stdinMu     sync.Mutex
	inputs      map[*os.File]struct{}
	stdinClosed bool
}

type output struct {
	fd byte // outStdout or outStderr
	r  *os.File
}

func (p *proc) close() {
	if p.stdin != nil && p.stdin != p.pty {
		p.closeStdin()
	}
	for _, o := range p.out {
		o.r.Close()
	}
}

// closeStdin tells the process that stdin ended. A tty has no end of its
// own, so it gets ^D, as a terminal does.
func (p *proc) closeStdin() {
	if p.stdin == nil {
		return
	}
	if p.pty != nil {
		// A full tty must not hold a disconnected attach open forever.
		if f, err := duplicateInput(p.stdin); err == nil {
			defer f.Close()
			f.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
			f.Write([]byte{4})
		}
		return
	}
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	p.stdinClosed = true
	p.stdin.Close()
	for f := range p.inputs {
		f.Close()
	}
}

func (p *proc) input(enabled bool) (*inputStream, error) {
	s := &inputStream{resize: func(rows, cols uint16) {
		if p.pty != nil {
			setSize(p.pty, rows, cols)
		}
	}}
	if !enabled || p.stdin == nil {
		return s, nil
	}
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if p.stdinClosed {
		return s, nil
	}
	f, err := duplicateInput(p.stdin)
	if err != nil {
		return nil, err
	}
	if p.inputs == nil {
		p.inputs = make(map[*os.File]struct{})
	}
	p.inputs[f] = struct{}{}
	s.stdin = f
	s.release = func() {
		p.stdinMu.Lock()
		delete(p.inputs, f)
		p.stdinMu.Unlock()
	}
	s.end = func() {
		if p.pty != nil {
			f.Write([]byte{4})
		} else {
			p.closeStdin()
		}
	}
	return s, nil
}

// start starts argv in the container: as its user, in its working directory,
// in a session of its own, with a tty or with pipes.
func (c *process) start(path string, argv []string, tty, stdin bool, rows, cols uint16) (*proc, error) {
	env := c.env
	if tty && !hasEnv(env, "TERM") {
		// as containerd does for a container or exec with a tty
		env = append(env[:len(env):len(env)], "TERM=xterm")
	}
	attr := &os.ProcAttr{Dir: c.dir, Env: env, Sys: &syscall.SysProcAttr{Setsid: true, Credential: c.cred}}
	p := &proc{}
	var child []*os.File // the ends of the child, closed after the start
	defer func() {
		for _, f := range child {
			f.Close()
		}
	}()
	if tty {
		m, s, err := openPTY(rows, cols)
		if err != nil {
			return nil, err
		}
		child = append(child, s)
		attr.Files = []*os.File{s, s, s}
		attr.Sys.Setctty, attr.Sys.Ctty = true, 0
		p.pty = m
		if stdin {
			p.stdin = m
		}
		p.out = []output{{outStdout, m}}
	} else {
		attr.Files = make([]*os.File, 3)
		if stdin {
			r, w, err := os.Pipe()
			if err != nil {
				return nil, err
			}
			child = append(child, r)
			attr.Files[0], p.stdin = r, w
		} else {
			null, err := os.Open("/dev/null")
			if err != nil {
				return nil, err
			}
			child = append(child, null)
			attr.Files[0] = null
		}
		for i, fd := range []byte{outStdout, outStderr} {
			r, w, err := os.Pipe()
			if err != nil {
				p.close()
				return nil, err
			}
			child = append(child, w)
			attr.Files[i+1] = w
			p.out = append(p.out, output{fd, r})
		}
	}
	pid, done, err := spawn(path, argv, attr)
	if err != nil {
		p.close()
		if p.pty != nil {
			p.pty.Close()
		}
		var pe *os.PathError
		if errors.As(err, &pe) {
			err = fmt.Errorf("exec: %q: %w", argv[0], pe.Err)
		}
		return nil, err
	}
	p.pid, p.done = pid, done
	return p, nil
}

// startMain starts the entrypoint. Its output goes to the console, which is
// the log of the container, and to every kubectl attach.
func (c *process) startMain(path string, cfg config, console *os.File) (<-chan syscall.WaitStatus, error) {
	var rows, cols uint16
	if cfg.TTY {
		rows, cols = 24, 80
	}
	p, err := c.start(path, cfg.Args, cfg.TTY, cfg.Stdin, rows, cols)
	if err != nil {
		return nil, err
	}
	c.main, c.stdinOnce = p, cfg.StdinOnce
	c.subs = map[*subscriber]struct{}{}
	var wg sync.WaitGroup
	for _, o := range p.out {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 32<<10)
			for {
				n, err := o.r.Read(buf)
				if n > 0 {
					console.Write(buf[:n])
					c.broadcast(o.fd, append([]byte(nil), buf[:n]...))
				}
				if err != nil {
					return
				}
			}
		}()
	}
	done := make(chan syscall.WaitStatus, 1)
	go func() {
		ws := <-p.done
		// what is still in the pipes, unless a process in the background
		// holds them open
		waitTimeout(&wg, time.Second)
		p.close()
		if p.pty != nil {
			p.pty.Close()
		}
		wg.Wait()
		c.mu.Lock()
		c.exited, c.code = true, exitCode(ws)
		for s := range c.subs {
			close(s.ch)
		}
		c.subs = nil
		c.mu.Unlock()
		// let kubectl attach see the end before the page stops the VM
		waitTimeout(&c.attaches, 500*time.Millisecond)
		done <- ws
	}()
	return done, nil
}

type subscriber struct {
	ch      chan frame
	dropped bool
}

func (c *process) broadcast(t byte, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for s := range c.subs {
		select {
		case s.ch <- frame{t, b}:
		default:
			// too slow: drop it rather than hold up the container
			s.dropped = true
			close(s.ch)
			delete(c.subs, s)
		}
	}
}

func (c *process) subscribe() (*subscriber, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exited {
		return nil, false
	}
	s := &subscriber{ch: make(chan frame, 1024)}
	c.subs[s] = struct{}{}
	return s, true
}

func (c *process) unsubscribe(s *subscriber) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.subs, s)
}

// serveExec takes kubectl exec and attach from the kubelet.
func serveExec(c *process) {
	l, err := net.Listen("tcp4", fmt.Sprintf(":%d", execPort))
	if err != nil {
		fmt.Fprintln(os.Stderr, "tabernetes init: kubectl exec:", err)
		return
	}
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go c.serve(conn)
	}
}

func (c *process) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	w := &frameWriter{w: conn}
	var req execRequest
	if err := json.Unmarshal(line, &req); err != nil {
		w.write(outError, []byte(err.Error()))
		return
	}
	if req.Attach {
		c.attach(
			req,
			conn,
			r,
			w,
		)
	} else {
		c.exec(
			req,
			conn,
			r,
			w,
		)
	}
}

func (c *process) exec(req execRequest, conn net.Conn, r *bufio.Reader, w *frameWriter) {
	if len(req.Cmd) == 0 {
		w.write(outError, []byte("exec: no command specified"))
		return
	}
	path, err := lookPath(req.Cmd[0], getEnv(c.env, "PATH"))
	var p *proc
	if err == nil {
		p, err = c.start(path, req.Cmd, req.TTY, req.Stdin, req.Rows, req.Cols)
	}
	if err != nil {
		w.write(outError, []byte(err.Error()))
		return
	}
	var wg sync.WaitGroup
	for _, o := range p.out {
		wg.Add(1)
		go func() {
			defer wg.Done()
			copyFrames(w, o.fd, o.r)
		}()
	}
	input, err := p.input(req.Stdin)
	if err != nil {
		syscall.Kill(-p.pid, syscall.SIGKILL)
		<-p.done
		p.close()
		if p.pty != nil {
			p.pty.Close()
		}
		w.write(outError, []byte(err.Error()))
		return
	}
	gone, stopInput := input.read(conn, r, w)
	defer stopInput()
	var ws syscall.WaitStatus
	select {
	case ws = <-p.done:
	case <-gone:
		// kubectl went away: so does the command, with its children
		syscall.Kill(-p.pid, syscall.SIGKILL)
		ws = <-p.done
	}
	waitTimeout(&wg, time.Second)
	p.close()
	if p.pty != nil {
		p.pty.Close()
	}
	wg.Wait()
	code := make([]byte, 4)
	binary.BigEndian.PutUint32(code, uint32(int32(exitCode(ws))))
	w.write(outExit, code)
}

// attach connects to the main process: its output from now on and, if the
// container has stdin, its input.
func (c *process) attach(req execRequest, conn net.Conn, r *bufio.Reader, w *frameWriter) {
	c.attaches.Add(1)
	defer c.attaches.Done()
	s, ok := c.subscribe()
	if !ok {
		w.write(outError, []byte("container not running"))
		return
	}
	p := c.main
	stdin := req.Stdin && p.stdin != nil
	if p.pty != nil && req.Rows > 0 {
		setSize(p.pty, req.Rows, req.Cols)
	}
	var stdinDone sync.Once
	endStdin := func() {
		if stdin && c.stdinOnce {
			stdinDone.Do(func() { c.closeOnce.Do(p.closeStdin) })
		}
	}
	input, err := p.input(stdin)
	if err != nil {
		c.unsubscribe(s)
		w.write(outError, []byte(err.Error()))
		return
	}
	input.end = endStdin
	gone, stopInput := input.read(conn, r, w)
	defer stopInput()
	for {
		select {
		case f, ok := <-s.ch:
			if !ok {
				if !s.dropped {
					c.mu.Lock()
					code := c.code
					c.mu.Unlock()
					b := make([]byte, 4)
					binary.BigEndian.PutUint32(b, uint32(int32(code)))
					w.write(outExit, b)
				}
				return
			}
			if w.write(f.t, f.b) != nil {
				c.unsubscribe(s)
				endStdin()
				return
			}
		case <-gone:
			c.unsubscribe(s)
			endStdin()
			return
		}
	}
}

func waitTimeout(wg *sync.WaitGroup, d time.Duration) {
	ch := make(chan struct{})
	go func() {
		wg.Wait()
		close(ch)
	}()
	select {
	case <-ch:
	case <-time.After(d):
	}
}

// The reaper waits for every child: the processes of the container and the
// orphans that the kernel hands to PID 1. A process registers before the
// reaper can see it exit.
var (
	reapMu  sync.Mutex
	waiters = map[int]chan syscall.WaitStatus{}
)

func spawn(path string, argv []string, attr *os.ProcAttr) (int, <-chan syscall.WaitStatus, error) {
	reapMu.Lock()
	defer reapMu.Unlock()
	p, err := os.StartProcess(path, argv, attr)
	if err != nil {
		return 0, nil, err
	}
	// Release sets p.Pid to -1, so keep the pid first. With -1, a kill of
	// the process group was kill(1): it hit init, not the command.
	pid := p.Pid
	ch := make(chan syscall.WaitStatus, 1)
	waiters[pid] = ch
	p.Release()
	return pid, ch, nil
}

func reaper() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGCHLD)
	for {
		reapMu.Lock()
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if err != nil || pid <= 0 {
				break
			}
			if ch, ok := waiters[pid]; ok {
				ch <- ws
				delete(waiters, pid)
			}
		}
		reapMu.Unlock()
		<-sig
	}
}

// openPTY opens a pair of the devpts instance of the container.
func openPTY(rows, cols uint16) (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/pts/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var unlock int32
	var n uint32
	if err = ioctl(master, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err == nil {
		err = ioctl(master, syscall.TIOCGPTN, unsafe.Pointer(&n))
	}
	if err == nil {
		slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	}
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("open pty: %w", err)
	}
	setSize(master, rows, cols)
	return master, slave, nil
}

func setSize(f *os.File, rows, cols uint16) {
	if rows == 0 || cols == 0 {
		return
	}
	ws := [4]uint16{rows, cols, 0, 0}
	ioctl(f, syscall.TIOCSWINSZ, unsafe.Pointer(&ws))
}

// ioctl works on the descriptor without taking it out of the poller, so
// that Close still ends a Read.
func ioctl(f *os.File, req uintptr, arg unsafe.Pointer) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}
