package main

import (
	"fmt"
	"testing"
)

func TestLogBuffer(t *testing.T) {
	b := newLogBuffer()
	fmt.Fprint(b, "one\ntw")
	lines, next, more := b.since(0)
	if len(lines) != 1 || lines[0].text != "one" || next != 1 {
		t.Fatalf("since(0) = %v, %d", lines, next)
	}
	fmt.Fprint(b, "o\n")
	select {
	case <-more:
	default:
		t.Fatal("no wake after a new line")
	}
	if lines, next, _ = b.since(next); len(lines) != 1 || lines[0].text != "two" || next != 2 {
		t.Fatalf("since(1) = %v, %d", lines, next)
	}
	// Old lines go, the numbers stay.
	for i := range 6000 {
		fmt.Fprintln(b, i)
	}
	lines, next, _ = b.since(next)
	if next != 6002 || len(lines) != 4000 || lines[len(lines)-1].text != "5999" {
		t.Fatalf("after 6000 lines: %d lines, next %d", len(lines), next)
	}
	if lines, _, _ = b.since(6001); len(lines) != 1 || lines[0].text != "5999" {
		t.Fatalf("since(6001) = %v", lines)
	}
}
