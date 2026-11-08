package main

import (
	"io"
	"sync"
	"sync/atomic"
)

// tty is the terminal for full screen programs (vi, kubectl edit). Normally
// the browser does the line editing and the shell gets complete lines. In raw
// mode every key goes straight to the running program instead.
var tty = &terminal{keys: make(chan string, 256)}

type terminal struct {
	mu   sync.Mutex
	out  io.Writer // the terminal itself, also when stdout is redirected
	held []byte    // output of everything else while a raw mode program runs
	keys chan string
	raw  atomic.Bool
	cols atomic.Int32
	rows atomic.Int32
}

// resizeKey is sent to a raw mode program when the terminal changes size.
const resizeKey = "\x00resize"

func (t *terminal) setOutput(w io.Writer) {
	t.mu.Lock()
	t.out = w
	t.mu.Unlock()
}

func (t *terminal) Write(b []byte) (int, error) {
	t.mu.Lock()
	w := t.out
	t.mu.Unlock()
	if w == nil {
		return len(b), nil
	}
	return w.Write(b)
}

// holdWriter is the terminal for everything but the raw mode program. Its
// output (a warning of kubectl, for example) waits while the program owns the
// screen, so it does not end up in the middle of vi.
type holdWriter struct{ t *terminal }

func (h holdWriter) Write(b []byte) (int, error) {
	h.t.mu.Lock()
	if h.t.raw.Load() {
		h.t.held = append(h.t.held, b...)
		h.t.mu.Unlock()
		return len(b), nil
	}
	h.t.mu.Unlock()
	return h.t.Write(b)
}

// input takes what the browser terminal sends while a command runs.
func (t *terminal) input(s string) {
	if !t.raw.Load() {
		return
	}
	select {
	case t.keys <- s:
	default: // the program does not read; drop instead of blocking the browser
	}
}

func (t *terminal) resize(cols, rows int) {
	t.cols.Store(int32(cols))
	t.rows.Store(int32(rows))
	t.input(resizeKey)
}

func (t *terminal) size() (cols, rows int) {
	cols, rows = int(t.cols.Load()), int(t.rows.Load())
	if cols < 20 {
		cols = 80
	}
	if rows < 5 {
		rows = 24
	}
	return cols, rows
}

// enterRaw switches to raw mode and the alternate screen. The returned
// function switches back.
func (t *terminal) enterRaw() func() {
	for len(t.keys) > 0 {
		<-t.keys
	}
	t.raw.Store(true)
	io.WriteString(t, "\x1b[?1049h\x1b[H\x1b[2J")
	return func() {
		io.WriteString(t, "\x1b[?1049l")
		t.mu.Lock()
		t.raw.Store(false)
		held := t.held
		t.held = nil
		t.mu.Unlock()
		t.Write(held)
	}
}

// key is one key press: a printable rune or the name of a special key.
type key struct {
	r    rune
	name string // "esc", "enter", "backspace", "tab", "up", "down", "left", "right", "home", "end", "pgup", "pgdown", "delete", "ctrl-x", "resize"
}

// parseKeys splits a chunk of terminal input into keys.
func parseKeys(s string) []key {
	if s == resizeKey {
		return []key{{name: "resize"}}
	}
	var keys []key
	r := []rune(s)
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c == 0x1b:
			if i+2 < len(r) && (r[i+1] == '[' || r[i+1] == 'O') {
				j := i + 2
				for j < len(r) && (r[j] >= '0' && r[j] <= '9' || r[j] == ';') {
					j++
				}
				if j < len(r) {
					name := map[string]string{
						"A": "up", "B": "down", "C": "right", "D": "left", "H": "home", "F": "end",
						"1~": "home", "4~": "end", "7~": "home", "8~": "end",
						"3~": "delete", "5~": "pgup", "6~": "pgdown",
					}[string(r[i+2:j+1])]
					if name != "" {
						keys = append(keys, key{name: name})
					}
					i = j
					continue
				}
			}
			keys = append(keys, key{name: "esc"})
		case c == '\r' || c == '\n':
			keys = append(keys, key{name: "enter"})
			if c == '\r' && i+1 < len(r) && r[i+1] == '\n' {
				i++
			}
		case c == 0x7f || c == 0x08:
			keys = append(keys, key{name: "backspace"})
		case c == '\t':
			keys = append(keys, key{name: "tab"})
		case c < 0x20:
			keys = append(keys, key{name: "ctrl-" + string(rune('a'+c-1))})
		default:
			keys = append(keys, key{r: c})
		}
	}
	return keys
}
