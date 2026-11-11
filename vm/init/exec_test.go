//go:build linux

package main

import (
	"encoding/json"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

var testReaper sync.Once

func TestExecDisconnectKillsBlockedProcess(t *testing.T) {
	testReaper.Do(func() { go reaper() })
	c := &process{env: os.Environ(), dir: t.TempDir()}
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); c.serve(server) }()
	t.Cleanup(func() {
		server.Close()
		client.Close()
		reapMu.Lock()
		for pid := range waiters {
			syscall.Kill(-pid, syscall.SIGKILL)
		}
		reapMu.Unlock()
	})
	client.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(client).Encode(execRequest{Cmd: []string{"sleep", "60"}, Stdin: true}); err != nil {
		t.Fatal(err)
	}
	if err := (&frameWriter{w: client}).write(inData, make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the exec process survived its disconnected client")
	}
}

func TestAttachDisconnectKeepsMainProcess(t *testing.T) {
	testReaper.Do(func() { go reaper() })
	console, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { console.Close() })
	c := &process{env: os.Environ(), dir: t.TempDir()}
	done, err := c.startMain("/bin/sleep", config{Args: []string{"sleep", "60"}, Stdin: true}, console)
	if err != nil {
		t.Fatal(err)
	}
	if c.main.pid <= 0 {
		t.Fatalf("the main process has no pid: %d", c.main.pid)
	}
	t.Cleanup(func() { syscall.Kill(-c.main.pid, syscall.SIGKILL); <-done })
	server, client := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })
	client.SetDeadline(time.Now().Add(5 * time.Second))
	attached := make(chan struct{})
	go func() { defer close(attached); c.serve(server) }()
	if err := json.NewEncoder(client).Encode(execRequest{Attach: true, Stdin: true}); err != nil {
		t.Fatal(err)
	}
	if err := (&frameWriter{w: client}).write(inData, make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	client.Close()
	select {
	case <-attached:
	case <-time.After(2 * time.Second):
		t.Fatal("attach did not finish after disconnect")
	}
	if err := syscall.Kill(c.main.pid, 0); err != nil {
		t.Fatalf("detach killed the main process: %v", err)
	}
}
