package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

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
