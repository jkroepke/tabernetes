package main

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestServeLogsFinishes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		alreadyEnded bool
	}{
		{name: "completed", alreadyEnded: true},
		{name: "following"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			logs := newLogBuffer()
			logs.Write([]byte("hello"))
			if tc.alreadyEnded {
				logs.finish()
			}
			tp := &tabPod{name: "pod", namespace: "default", container: corev1.Container{Name: "main"}, ctx: ctx, logs: logs}
			n := &tabNode{pods: map[types.UID]*tabPod{"pod": tp}}
			r := httptest.NewRequest("GET", "/containerLogs/default/pod/main?follow=true", nil).WithContext(ctx)
			r.SetPathValue("ns", "default")
			r.SetPathValue("pod", "pod")
			r.SetPathValue("container", "main")
			w := &logRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{}, 1)}
			done := make(chan struct{})
			go func() { defer close(done); n.serveLogs(w, r) }()
			t.Cleanup(func() { cancel(); <-done })
			if !tc.alreadyEnded {
				select {
				case <-w.flushed:
				case <-time.After(time.Second):
					t.Fatal("the log follower did not start")
				}
				logs.finish()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("follow did not finish with the container")
			}
			if got := w.Body.String(); got != "hello" {
				t.Fatalf("logs = %q, want %q", got, "hello")
			}
		})
	}
}

type logRecorder struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
}

func (w *logRecorder) Flush() {
	w.ResponseRecorder.Flush()
	select {
	case w.flushed <- struct{}{}:
	default:
	}
}

func TestLineDiscipline(t *testing.T) {
	for in, want := range map[string]struct{ out, echo string }{
		"ab\x7fc\r":         {"ac\n", "ab\b \bc\n"},
		"hi\x1b[Dyo\rbye\r": {"hiyo\nbye\n", "hiyo\nbye\n"},
		"x\x15y\r\x04":      {"y\n", "x\b \by\n"},
		"q\x03rest\r":       {"\x03\n", "q^C\n"},
	} {
		var echo bytes.Buffer
		out, _ := io.ReadAll(lineDiscipline(strings.NewReader(in), &echo))
		if string(out) != want.out || echo.String() != want.echo {
			t.Errorf("%q: got %q, echo %q; want %q, echo %q", in, out, echo.String(), want.out, want.echo)
		}
	}
}

func TestCRLFWriter(t *testing.T) {
	var b bytes.Buffer
	w := &crlfWriter{w: &b}
	if n, _ := w.Write([]byte("a\nb\n")); n != 4 || b.String() != "a\r\nb\r\n" {
		t.Errorf("got %d %q", n, b.String())
	}
}
