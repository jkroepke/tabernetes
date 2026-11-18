package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// waitOutput holds a writer while the tab is more than outputWindow behind,
// until an ack, the end of the command or the end of the tab.
func TestWaitOutput(t *testing.T) {
	waits := func(term *terminal, done <-chan struct{}) chan struct{} {
		ch := make(chan struct{})
		go func() { term.waitOutput(done); close(ch) }()
		return ch
	}
	returned := func(ch chan struct{}) bool {
		select {
		case <-ch:
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	}

	term := newTerminal()
	term.sent.Store(outputWindow)
	if !returned(waits(term, nil)) {
		t.Fatal("waits with a full window but not more")
	}

	term.sent.Store(outputWindow + 1)
	ch := waits(term, nil)
	if returned(ch) {
		t.Fatal("does not wait with more than the window on the way")
	}
	term.ack(1)
	if !returned(ch) {
		t.Fatal("still waits after an ack")
	}

	term.sent.Store(2*outputWindow + 1)
	done := make(chan struct{})
	ch = waits(term, done)
	close(done)
	if !returned(ch) {
		t.Fatal("still waits after Ctrl-C")
	}

	ch = waits(term, nil)
	term.close()
	if !returned(ch) {
		t.Fatal("still waits after the tab is gone")
	}
}

type failWriter struct{ n int }

func (w *failWriter) Write(b []byte) (int, error) {
	w.n++
	return 0, errors.New("closed")
}

// seq ends at the first failed write (Ctrl-C, or a pipe that is closed)
// instead of counting on to the end.
func TestSeqStopsOnWriteError(t *testing.T) {
	w := &failWriter{}
	hc := interp.HandlerContext{Stdout: w}
	if err := seq(context.Background(), hc, []string{"1000000"}, nil); err == nil {
		t.Fatal("seq succeeded on a failed write")
	}
	if w.n != 1 {
		t.Fatalf("seq wrote %d times after the first failure, want 1", w.n)
	}
}
