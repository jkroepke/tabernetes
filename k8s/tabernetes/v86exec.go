package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
	utilexec "k8s.io/utils/exec"
)

// kubectl exec and attach for a pod in v86. Init of the VM listens on
// vmExecPort (vm/init/exec.go), and the kubelet connects to it through the
// network card of the VM, as port-forward does. The kubelet sends one line of
// JSON, then both sides send frames: one byte type, four bytes big-endian
// length, the payload.

const vmExecPort = 60250

// frames to the VM
const (
	vmStdin  = 0
	vmClose  = 1 // stdin ended
	vmResize = 2 // rows, cols: uint16 each
)

// frames from the VM
const (
	vmStdout = 1
	vmStderr = 2
	vmExit   = 3 // exit code: int32
	vmError  = 4 // the command did not start
)

// streamer is a workload with a terminal and an attachable main process.
type streamer interface {
	// stream runs cmd in the workload, or attaches to its main process.
	stream(ctx context.Context, attach bool, cmd []string, in io.Reader, out, errOut io.Writer, tty bool, resize <-chan remotecommand.TerminalSize) error
}

func (v *v86VM) exec(ctx context.Context, cmd []string, in io.Reader, out, errOut io.Writer) error {
	return v.stream(ctx, false, cmd, in, out, errOut, false, nil)
}

func (v *v86VM) stream(ctx context.Context, attach bool, cmd []string, in io.Reader, out, errOut io.Writer, tty bool, resize <-chan remotecommand.TerminalSize) error {
	conn, err := v.dial(vmExecPort)
	if err != nil {
		return fmt.Errorf("failed to connect to the container: %w", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	req := map[string]any{"cmd": cmd, "tty": tty, "stdin": in != nil, "attach": attach}
	if tty && resize != nil {
		// kubectl sends the size of its terminal right away
		select {
		case s, ok := <-resize:
			if ok {
				req["rows"], req["cols"] = s.Height, s.Width
			}
		case <-time.After(200 * time.Millisecond):
		}
	}
	line, _ := json.Marshal(req)
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return err
	}
	var mu sync.Mutex
	send := func(t byte, b []byte) error {
		mu.Lock()
		defer mu.Unlock()
		buf := make([]byte, 5+len(b))
		buf[0] = t
		binary.BigEndian.PutUint32(buf[1:], uint32(len(b)))
		copy(buf[5:], b)
		_, err := conn.Write(buf)
		return err
	}
	if in != nil {
		go func() {
			buf := make([]byte, 32<<10)
			for {
				n, err := in.Read(buf)
				if n > 0 && send(vmStdin, buf[:n]) != nil {
					return
				}
				if err != nil {
					send(vmClose, nil)
					return
				}
			}
		}()
	}
	if resize != nil {
		go func() {
			for {
				select {
				case s, ok := <-resize:
					if !ok {
						return
					}
					b := make([]byte, 4)
					binary.BigEndian.PutUint16(b, s.Height)
					binary.BigEndian.PutUint16(b[2:], s.Width)
					if send(vmResize, b) != nil {
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	r := bufio.NewReader(conn)
	var h [5]byte
	for {
		if _, err := io.ReadFull(r, h[:]); err != nil {
			if attach || ctx.Err() != nil {
				// the container ended, or kubectl went away
				return nil
			}
			return fmt.Errorf("the connection to the container broke: %w", err)
		}
		b := make([]byte, binary.BigEndian.Uint32(h[1:]))
		if _, err := io.ReadFull(r, b); err != nil {
			return err
		}
		switch h[0] {
		case vmStdout:
			out.Write(b)
		case vmStderr:
			errOut.Write(b)
		case vmExit:
			if len(b) != 4 {
				return errors.New("bad exit frame")
			}
			if code := int(int32(binary.BigEndian.Uint32(b))); code != 0 && !attach {
				return utilexec.CodeExitError{Err: fmt.Errorf("command terminated with exit code %d", code), Code: code}
			}
			return nil
		case vmError:
			if attach {
				return errors.New(string(b))
			}
			// what containerd and runc say
			return fmt.Errorf("failed to exec in container: failed to start exec %q: OCI runtime exec failed: exec failed: unable to start container process: %s: unknown", execID(), b)
		}
	}
}

func execID() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}
