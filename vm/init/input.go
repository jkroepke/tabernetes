package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"net"
	"os"
	"sync"
	"syscall"
)

// Each session owns its stdin descriptor, so disconnecting an attach can
// cancel a blocked write without closing the main process's stdin.
func duplicateInput(f *os.File) (*os.File, error) {
	raw, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var fd int
	var dupErr error
	err = raw.Control(func(original uintptr) {
		fd, dupErr = syscall.Dup(int(original))
		if dupErr == nil {
			syscall.CloseOnExec(fd)
		}
	})
	if err != nil {
		return nil, err
	}
	if dupErr != nil {
		return nil, dupErr
	}
	return os.NewFile(uintptr(fd), f.Name()), nil
}

type inputStream struct {
	stdin   *os.File
	end     func()
	resize  func(uint16, uint16)
	release func()
}

// read acknowledges each stdin frame after its write. The sender waits for
// that acknowledgement, leaving the reader free to detect a disconnect.
func (s *inputStream) read(conn net.Conn, r *bufio.Reader, w *frameWriter) (<-chan struct{}, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	queue := make(chan frame, 1)
	finished := make(chan struct{})
	stop := sync.OnceFunc(func() {
		cancel()
		conn.Close()
		if s.stdin != nil {
			s.stdin.Close()
		}
		if s.release != nil {
			s.release()
		}
	})
	go func() {
		defer close(finished)
		for {
			select {
			case <-ctx.Done():
				return
			case f := <-queue:
				if f.t == inClose {
					if s.end != nil {
						s.end()
					}
					continue
				}
				if s.stdin != nil {
					s.stdin.Write(f.b)
				}
				if ctx.Err() != nil {
					return
				}
				if w.write(outStdinAck, nil) != nil {
					conn.Close()
					return
				}
			}
		}
	}()
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		defer func() { stop(); <-finished }()
		readFrames(r, func(t byte, b []byte) bool {
			switch t {
			case inData, inClose:
				select {
				case queue <- frame{t: t, b: b}:
				default:
					return false // the peer did not wait for the acknowledgement
				}
			case inResize:
				if s.resize != nil && len(b) == 4 {
					s.resize(binary.BigEndian.Uint16(b), binary.BigEndian.Uint16(b[2:]))
				}
			}
			return ctx.Err() == nil
		})
	}()
	return gone, func() { stop(); <-gone }
}
