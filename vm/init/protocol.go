package main

import (
	"bufio"
	"encoding/binary"
	"io"
	"sync"
)

const execPort = 60250

// frames to the VM
const (
	inData   = 0
	inClose  = 1 // stdin is closed
	inResize = 2 // rows and cols, uint16 each
)

// frames from the VM
const (
	outStdout   = 1
	outStderr   = 2
	outExit     = 3 // exit code, int32
	outError    = 4 // the command did not start
	outStdinAck = 5 // the previous stdin frame was consumed
)

type frame struct {
	t byte
	b []byte
}

type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (f *frameWriter) write(t byte, b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	buf := make([]byte, 5+len(b))
	buf[0] = t
	binary.BigEndian.PutUint32(buf[1:], uint32(len(b)))
	copy(buf[5:], b)
	_, err := f.w.Write(buf)
	return err
}

func copyFrames(w *frameWriter, t byte, r io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 && w.write(t, buf[:n]) != nil {
			return
		}
		if err != nil {
			return
		}
	}
}

func readFrames(r *bufio.Reader, fn func(t byte, b []byte) bool) {
	var h [5]byte
	for {
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(h[1:])
		if n > 1<<20 {
			return
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			return
		}
		if !fn(h[0], b) {
			return
		}
	}
}
