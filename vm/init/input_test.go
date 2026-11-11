package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func TestInputDisconnectWhileStdinIsBlocked(t *testing.T) {
	t.Parallel()
	r, original, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); original.Close() })
	// Fill the pipe without relying on its platform-specific capacity.
	if err := original.SetWriteDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	filled, err := original.Write(make([]byte, 1<<20))
	if err == nil || filled == 0 {
		t.Fatalf("filling stdin: wrote %d bytes, error %v", filled, err)
	}
	if err := original.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	stdin, err := duplicateInput(original)
	if err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })
	s := &inputStream{stdin: stdin}
	gone, stop := s.read(server, bufio.NewReader(server), &frameWriter{w: server})
	t.Cleanup(stop)
	if err := (&frameWriter{w: client}).write(inData, []byte("blocked")); err != nil {
		t.Fatal(err)
	}
	client.Close()
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect did not cancel the stdin writer")
	}
	// Detaching must leave the main process's original stdin usable.
	if _, err := io.CopyN(io.Discard, r, int64(filled)); err != nil {
		t.Fatal(err)
	}
	if _, err := original.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil || b[0] != 'x' {
		t.Fatalf("original stdin after detach: %q, %v", b, err)
	}
}

func TestInputAcknowledgesLargeStreams(t *testing.T) {
	t.Parallel()
	r, original, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); original.Close() })
	stdin, err := duplicateInput(original)
	if err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })
	client.SetDeadline(time.Now().Add(5 * time.Second))
	s := &inputStream{stdin: stdin, end: func() { stdin.Close(); original.Close() }}
	_, stop := s.read(server, bufio.NewReader(server), &frameWriter{w: server})
	t.Cleanup(stop)
	want := bytes.Repeat([]byte("input"), 256<<10)
	read := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(r); read <- b }()
	w := &frameWriter{w: client}
	for offset := 0; offset < len(want); offset += 32 << 10 {
		if err := w.write(inData, want[offset:min(offset+(32<<10), len(want))]); err != nil {
			t.Fatal(err)
		}
		var ack [5]byte
		if _, err := io.ReadFull(client, ack[:]); err != nil {
			t.Fatal(err)
		}
		if ack[0] != outStdinAck || binary.BigEndian.Uint32(ack[1:]) != 0 {
			t.Fatalf("unexpected stdin acknowledgement: %v", ack)
		}
	}
	if err := w.write(inClose, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-read:
		if !bytes.Equal(got, want) {
			t.Fatalf("read %d bytes, want %d", len(got), len(want))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stdin did not reach EOF")
	}
}
