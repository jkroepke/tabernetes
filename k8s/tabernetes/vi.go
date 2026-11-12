package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"mvdan.cc/sh/v3/interp"

	"k8s.io/kubectl/pkg/cmd/util/editor"
)

// A small vi for the browser terminal. It knows what you need to edit a
// manifest, not more: motions (hjkl w b e 0 ^ $ gg G, counts), insert (i a I
// A o O), changes (x dd dw D cc cw C r J p P), undo, search and :w :q :wq :x.
// It also backs "kubectl edit", see k8s/_overlay/.../editor/launcher_js.go.

type viMode int

const (
	viNormal viMode = iota
	viInsert
	viCmdline
)

type viState struct {
	lines [][]rune
	cy    int
	cx    int
}

type vi struct {
	tty       *terminal
	path      string
	lines     [][]rune
	cy, cx    int
	top, left int
	mode      viMode
	cmdline   []rune // with its ":" or "/"
	msg       string
	dirty     bool
	undo      []viState
	yank      [][]rune
	yankLines bool
	pending   string // operator or prefix waiting for the next key: d c y g r Z
	count     int
	search    string
	quit      bool
	paste     bool
	indented  bool // the last key auto-indented a new line
}

func viCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	if len(args) > 1 {
		return fail(hc, 2, "usage: vi [file]")
	}
	path := ""
	if len(args) == 1 {
		path = abs(hc, args[0])
	}
	if err := runVi(ctx, termFrom(ctx), path); err != nil {
		return fail(hc, 1, "vi: %v", err)
	}
	return nil
}

// runVi edits path until the user quits.
func runVi(ctx context.Context, tty *terminal, path string) error {
	e := &vi{tty: tty, path: path, lines: [][]rune{{}}}
	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			e.msg = fmt.Sprintf("%q [New]", filepath.Base(path))
		case err != nil:
			return err
		default:
			s := strings.TrimSuffix(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
			e.lines = nil
			for _, l := range strings.Split(s, "\n") {
				e.lines = append(e.lines, []rune(l))
			}
			e.msg = fmt.Sprintf("%q %dL, %dB", filepath.Base(path), len(e.lines), len(b))
		}
	}

	leave := tty.enterRaw()
	defer func() {
		io.WriteString(tty, "\x1b[2 q")
		leave()
	}()
	for !e.quit {
		e.draw()
		select {
		case s := <-tty.keys:
			// A paste comes as one chunk with line breaks: insert it as it
			// is, without auto-indent.
			e.paste = strings.ContainsAny(strings.TrimRight(s, "\r\n"), "\r\n")
			if l := e.line(); e.paste && e.indented && e.mode == viInsert && e.cx == len(l) && strings.TrimSpace(string(l)) == "" {
				// the paste brings its own indentation
				e.lines[e.cy], e.cx = nil, 0
			}
			for _, k := range parseKeys(s) {
				e.handle(k)
				if e.quit {
					break
				}
			}
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}

// ---- drawing ----

func (e *vi) draw() {
	cols, rows := e.tty.size()
	h := rows - 1
	if e.cy < e.top {
		e.top = e.cy
	}
	if e.cy >= e.top+h {
		e.top = e.cy - h + 1
	}
	if e.cx < e.left {
		e.left = e.cx
	}
	if e.cx >= e.left+cols {
		e.left = e.cx - cols + 1
	}

	var b strings.Builder
	b.WriteString("\x1b[?25l\x1b[H")
	for i := 0; i < h; i++ {
		if y := e.top + i; y < len(e.lines) {
			b.WriteString(highlight(e.lines[y], e.left, cols))
		} else {
			b.WriteString("\x1b[34m~\x1b[0m")
		}
		b.WriteString("\x1b[K\r\n")
	}

	// status line
	cursorRow, cursorCol := e.cy-e.top+1, e.cx-e.left+1
	switch {
	case e.mode == viCmdline:
		b.WriteString(string(e.cmdline))
		cursorRow, cursorCol = rows, len(e.cmdline)+1
	default:
		left := e.msg
		if e.mode == viInsert {
			left = "\x1b[1m-- INSERT --\x1b[0m"
		}
		name := "[No Name]"
		if e.path != "" {
			name = filepath.Base(e.path)
		}
		if e.dirty {
			name += " [+]"
		}
		right := fmt.Sprintf("%s  %d,%d", name, e.cy+1, e.cx+1)
		pad := cols - len([]rune(stripANSI(left))) - len([]rune(right)) - 1
		b.WriteString(left)
		if pad > 0 {
			b.WriteString(strings.Repeat(" ", pad) + right)
		}
	}
	b.WriteString("\x1b[K")
	shape := "\x1b[2 q" // block
	if e.mode == viInsert {
		shape = "\x1b[6 q" // bar
	}
	fmt.Fprintf(&b, "\x1b[%d;%dH%s\x1b[?25h", cursorRow, cursorCol, shape)
	io.WriteString(e.tty, b.String())
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// highlight renders the visible part of a line with a little YAML color:
// keys cyan, comments grey.
func highlight(l []rune, left, width int) string {
	color := make([]string, len(l))
	i := 0
	for i < len(l) && l[i] == ' ' {
		i++
	}
	if i+1 < len(l) && l[i] == '-' && l[i+1] == ' ' {
		i += 2
	}
	keyStart := i
	for i < len(l) && l[i] != ':' && l[i] != ' ' && l[i] != '#' {
		i++
	}
	if i < len(l) && l[i] == ':' && (i+1 == len(l) || l[i+1] == ' ') && i > keyStart {
		for j := keyStart; j < i; j++ {
			color[j] = "36"
		}
	}
	quote := rune(0)
	for j, c := range l {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' && (j == 0 || l[j-1] == ' '):
			for k := j; k < len(l); k++ {
				color[k] = "90"
			}
		}
		if color[j] == "90" {
			break
		}
	}

	var b strings.Builder
	cur := ""
	for j := left; j < len(l) && j < left+width; j++ {
		if color[j] != cur {
			b.WriteString("\x1b[0m")
			if color[j] != "" {
				b.WriteString("\x1b[" + color[j] + "m")
			}
			cur = color[j]
		}
		c := l[j]
		if c == '\t' || c < ' ' {
			c = ' '
		}
		b.WriteRune(c)
	}
	if cur != "" {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// ---- editing helpers ----

func (e *vi) line() []rune { return e.lines[e.cy] }

func (e *vi) snapshot() {
	st := viState{lines: make([][]rune, len(e.lines)), cy: e.cy, cx: e.cx}
	for i, l := range e.lines {
		st.lines[i] = slices.Clone(l)
	}
	e.undo = append(e.undo, st)
	if len(e.undo) > 200 {
		e.undo = e.undo[1:]
	}
	e.dirty = true
}

func (e *vi) clamp() {
	e.cy = max(0, min(e.cy, len(e.lines)-1))
	limit := len(e.line())
	if e.mode != viInsert {
		limit--
	}
	e.cx = max(0, min(e.cx, limit))
}

func firstNonBlank(l []rune) int {
	for i, c := range l {
		if c != ' ' && c != '\t' {
			return i
		}
	}
	return 0
}

func class(c rune) int {
	switch {
	case unicode.IsSpace(c):
		return 0
	case unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_':
		return 1
	}
	return 2
}

// wordForward returns the start of the next word on the same line, or the
// end of the line.
func wordForward(l []rune, x int) int {
	if x >= len(l) {
		return len(l)
	}
	c := class(l[x])
	for x < len(l) && class(l[x]) == c && c != 0 {
		x++
	}
	for x < len(l) && class(l[x]) == 0 {
		x++
	}
	return x
}

func wordEnd(l []rune, x int) int {
	x++
	for x < len(l) && class(l[x]) == 0 {
		x++
	}
	if x >= len(l) {
		return len(l) - 1
	}
	c := class(l[x])
	for x+1 < len(l) && class(l[x+1]) == c {
		x++
	}
	return x
}

func wordBack(l []rune, x int) int {
	if x <= 0 {
		return 0
	}
	x--
	for x > 0 && class(l[x]) == 0 {
		x--
	}
	c := class(l[x])
	for x > 0 && class(l[x-1]) == c {
		x--
	}
	return x
}

func (e *vi) deleteLines(from, n int) {
	to := min(from+n, len(e.lines))
	e.yank = cloneLines(e.lines[from:to])
	e.yankLines = true
	e.lines = slices.Delete(e.lines, from, to)
	if len(e.lines) == 0 {
		e.lines = [][]rune{{}}
	}
	e.cy = min(from, len(e.lines)-1)
	e.cx = firstNonBlank(e.line())
}

func cloneLines(ls [][]rune) [][]rune {
	out := make([][]rune, len(ls))
	for i, l := range ls {
		out[i] = slices.Clone(l)
	}
	return out
}

// deleteRange deletes [from, to) of the current line and yanks it.
func (e *vi) deleteRange(from, to int) {
	l := e.line()
	from, to = max(0, from), min(len(l), to)
	if from >= to {
		return
	}
	e.yank = [][]rune{slices.Clone(l[from:to])}
	e.yankLines = false
	e.lines[e.cy] = slices.Delete(l, from, to)
	e.cx = from
}

func (e *vi) insertMode() {
	e.mode = viInsert
	e.msg = ""
}

// ---- keys ----

func (e *vi) handle(k key) {
	if k.name == "resize" {
		return
	}
	e.indented = false
	switch e.mode {
	case viInsert:
		e.insertKey(k)
	case viCmdline:
		e.cmdlineKey(k)
	default:
		e.normalKey(k)
	}
	e.clamp()
}

func (e *vi) insertKey(k key) {
	l := e.line()
	switch k.name {
	case "esc":
		e.mode = viNormal
		e.cx--
	case "enter":
		indent := slices.Clone(l[:min(firstNonBlank(l), e.cx)])
		if strings.TrimSpace(string(l)) == "" {
			indent = slices.Clone(l[:min(len(l), e.cx)])
		}
		if e.paste {
			indent = nil
		}
		rest := append(indent, l[e.cx:]...)
		e.lines[e.cy] = slices.Clone(l[:e.cx])
		e.lines = slices.Insert(e.lines, e.cy+1, rest)
		e.cy++
		e.cx = len(indent)
		e.indented = len(indent) > 0
	case "backspace":
		switch {
		case e.cx > 0:
			e.lines[e.cy] = slices.Delete(l, e.cx-1, e.cx)
			e.cx--
		case e.cy > 0:
			prev := e.lines[e.cy-1]
			e.cx = len(prev)
			e.lines[e.cy-1] = append(prev, l...)
			e.lines = slices.Delete(e.lines, e.cy, e.cy+1)
			e.cy--
		}
	case "delete":
		switch {
		case e.cx < len(l):
			e.lines[e.cy] = slices.Delete(l, e.cx, e.cx+1)
		case e.cy+1 < len(e.lines):
			e.lines[e.cy] = append(l, e.lines[e.cy+1]...)
			e.lines = slices.Delete(e.lines, e.cy+1, e.cy+2)
		}
	case "tab":
		e.lines[e.cy] = slices.Insert(l, e.cx, ' ', ' ')
		e.cx += 2
	case "left":
		e.cx--
	case "right":
		e.cx++
	case "up":
		e.cy--
	case "down":
		e.cy++
	case "home":
		e.cx = 0
	case "end":
		e.cx = len(l)
	case "":
		e.lines[e.cy] = slices.Insert(l, e.cx, k.r)
		e.cx++
	}
}

func (e *vi) cmdlineKey(k key) {
	switch k.name {
	case "esc":
		e.mode = viNormal
	case "backspace":
		if len(e.cmdline) <= 1 {
			e.mode = viNormal
			return
		}
		e.cmdline = e.cmdline[:len(e.cmdline)-1]
	case "enter":
		e.mode = viNormal
		if e.cmdline[0] == '/' {
			if len(e.cmdline) > 1 {
				e.search = string(e.cmdline[1:])
			}
			e.find(true)
			return
		}
		e.ex(strings.TrimSpace(string(e.cmdline[1:])))
	case "":
		e.cmdline = append(e.cmdline, k.r)
	}
}

// find moves to the next match of the last search.
func (e *vi) find(forward bool) {
	if e.search == "" {
		e.msg = "No previous search"
		return
	}
	n := len(e.lines)
	for i := 0; i <= n; i++ {
		y := e.cy
		var from int
		if forward {
			y = (e.cy + i) % n
		} else {
			y = ((e.cy-i)%n + n) % n
		}
		l := string(e.lines[y])
		var at int
		if forward {
			if i == 0 {
				from = len(string(e.line()[:min(e.cx+1, len(e.line()))]))
			}
			at = strings.Index(l[from:], e.search)
			if at >= 0 {
				at += from
			}
		} else {
			end := len(l)
			if i == 0 {
				end = len(string(e.line()[:e.cx]))
			}
			at = strings.LastIndex(l[:end], e.search)
		}
		if at >= 0 {
			e.cy, e.cx = y, len([]rune(l[:at]))
			e.msg = "/" + e.search
			return
		}
	}
	e.msg = "Pattern not found: " + e.search
}

func (e *vi) write(path string) bool {
	if path == "" {
		path = e.path
	}
	if path == "" {
		e.msg = "No file name"
		return false
	}
	var b strings.Builder
	for _, l := range e.lines {
		b.WriteString(string(l))
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		e.msg = "\x1b[31m" + err.Error() + "\x1b[0m"
		return false
	}
	if e.path == "" {
		e.path = path
	}
	e.dirty = false
	e.msg = fmt.Sprintf("%q %dL, %dB written", filepath.Base(path), len(e.lines), b.Len())
	return true
}

// ex runs a ":" command.
func (e *vi) ex(c string) {
	name, arg, _ := strings.Cut(c, " ")
	arg = strings.TrimSpace(arg)
	if arg != "" && !filepath.IsAbs(arg) {
		if wd, err := os.Getwd(); err == nil {
			arg = filepath.Join(wd, arg)
		}
	}
	switch name {
	case "":
	case "w":
		e.write(arg)
	case "wq", "x":
		if !e.dirty && name == "x" && arg == "" {
			e.quit = true
		} else if e.write(arg) {
			e.quit = true
		}
	case "q":
		if e.dirty {
			e.msg = "\x1b[31mNo write since last change (add ! to override)\x1b[0m"
			return
		}
		e.quit = true
	case "q!", "qa!":
		e.quit = true
	default:
		if n, err := strconv.Atoi(name); err == nil {
			e.cy = n - 1
			e.cx = firstNonBlank(e.lines[max(0, min(e.cy, len(e.lines)-1))])
			return
		}
		e.msg = "\x1b[31mNot an editor command: " + c + "\x1b[0m"
	}
}

func (e *vi) normalKey(k key) {
	ch := k.r
	if k.name == "esc" {
		e.pending, e.count = "", 0
		return
	}
	// prefixes waiting for a second key
	if p := e.pending; p != "" {
		e.pending = ""
		n := max(e.count, 1)
		e.count = 0
		switch {
		case p == "r" && k.name == "":
			if e.cx < len(e.line()) {
				e.snapshot()
				e.line()[e.cx] = ch
			}
		case p == "g" && ch == 'g':
			e.cy = n - 1
			if n == 1 {
				e.cy = 0
			}
			e.cx = firstNonBlank(e.lines[min(e.cy, len(e.lines)-1)])
		case p == "Z" && ch == 'Z':
			e.ex("x")
		case p == "d" || p == "c" || p == "y":
			e.operator(p, k, n)
		}
		return
	}
	if k.name == "" && ch >= '0' && ch <= '9' && (ch != '0' || e.count > 0) {
		e.count = e.count*10 + int(ch-'0')
		return
	}
	n := max(e.count, 1)
	hadCount := e.count > 0
	e.count = 0
	_, rows := e.tty.size()
	page := max(1, rows-2)

	switch k.name {
	case "left", "backspace":
		ch = 'h'
	case "right":
		ch = 'l'
	case "up":
		ch = 'k'
	case "down":
		ch = 'j'
	case "enter":
		e.cy += n
		e.cx = firstNonBlank(e.lines[min(e.cy, len(e.lines)-1)])
		return
	case "home":
		ch = '0'
	case "end":
		ch = '$'
	case "delete":
		ch = 'x'
	case "pgdown", "ctrl-f":
		e.cy += page * n
		return
	case "pgup", "ctrl-b":
		e.cy -= page * n
		return
	case "ctrl-d":
		e.cy += page / 2
		return
	case "ctrl-u":
		e.cy -= page / 2
		return
	case "":
	default:
		return
	}

	l := e.line()
	switch ch {
	case 'h':
		e.cx -= n
	case 'l', ' ':
		e.cx += n
	case 'j':
		e.cy += n
	case 'k':
		e.cy -= n
	case 'w':
		for range n {
			nx := wordForward(e.line(), e.cx)
			if nx >= len(e.line()) && e.cy+1 < len(e.lines) {
				e.cy++
				nx = firstNonBlank(e.line())
			}
			e.cx = nx
		}
	case 'e':
		for range n {
			e.cx = wordEnd(l, e.cx)
		}
	case 'b':
		for range n {
			if e.cx == 0 && e.cy > 0 {
				e.cy--
				e.cx = len(e.line())
			}
			e.cx = wordBack(e.line(), e.cx)
		}
	case '0':
		e.cx = 0
	case '^':
		e.cx = firstNonBlank(l)
	case '$':
		e.cx = len(l)
	case 'G':
		e.cy = len(e.lines) - 1
		if hadCount {
			e.cy = n - 1
		}
		e.cx = firstNonBlank(e.lines[max(0, min(e.cy, len(e.lines)-1))])
	case 'g', 'r', 'Z', 'd', 'c', 'y':
		e.pending = string(ch)
		e.count = n
		if !hadCount {
			e.count = 0
		}
	case 'i':
		e.snapshot()
		e.insertMode()
	case 'a':
		e.snapshot()
		e.insertMode()
		e.cx = min(e.cx+1, len(l))
	case 'I':
		e.snapshot()
		e.insertMode()
		e.cx = firstNonBlank(l)
	case 'A':
		e.snapshot()
		e.insertMode()
		e.cx = len(l)
	case 'o', 'O':
		e.snapshot()
		indent := slices.Clone(l[:firstNonBlank(l)])
		at := e.cy + 1
		if ch == 'O' {
			at = e.cy
		}
		e.lines = slices.Insert(e.lines, at, indent)
		e.cy, e.cx = at, len(indent)
		e.indented = len(indent) > 0
		e.insertMode()
	case 'x':
		if len(l) > 0 {
			e.snapshot()
			e.deleteRange(e.cx, e.cx+n)
		}
	case 'X':
		if e.cx > 0 {
			e.snapshot()
			e.deleteRange(e.cx-n, e.cx)
		}
	case 's':
		e.snapshot()
		e.deleteRange(e.cx, e.cx+n)
		e.insertMode()
	case 'D':
		e.snapshot()
		e.deleteRange(e.cx, len(l))
	case 'C':
		e.snapshot()
		e.deleteRange(e.cx, len(l))
		e.insertMode()
	case 'S':
		e.operator("c", key{r: 'c'}, n)
	case 'Y':
		e.operator("y", key{r: 'y'}, n)
	case 'J':
		e.snapshot()
		for range n {
			if e.cy+1 >= len(e.lines) {
				break
			}
			next := []rune(strings.TrimLeft(string(e.lines[e.cy+1]), " \t"))
			cur := []rune(strings.TrimRight(string(e.line()), " \t"))
			e.cx = len(cur)
			if len(next) > 0 && len(cur) > 0 {
				cur = append(cur, ' ')
			}
			e.lines[e.cy] = append(cur, next...)
			e.lines = slices.Delete(e.lines, e.cy+1, e.cy+2)
		}
	case 'p', 'P':
		if e.yank == nil {
			return
		}
		e.snapshot()
		for range n {
			if e.yankLines {
				at := e.cy + 1
				if ch == 'P' {
					at = e.cy
				}
				e.lines = slices.Insert(e.lines, at, cloneLines(e.yank)...)
				e.cy = at
				e.cx = firstNonBlank(e.line())
				continue
			}
			at := e.cx
			if ch == 'p' && len(l) > 0 {
				at++
			}
			at = min(at, len(e.line()))
			e.lines[e.cy] = slices.Insert(e.line(), at, e.yank[0]...)
			e.cx = at + len(e.yank[0]) - 1
		}
	case 'u':
		if len(e.undo) == 0 {
			e.msg = "Already at oldest change"
			return
		}
		st := e.undo[len(e.undo)-1]
		e.undo = e.undo[:len(e.undo)-1]
		e.lines, e.cy, e.cx = st.lines, st.cy, st.cx
		e.dirty = true
	case ':', '/':
		e.mode = viCmdline
		e.cmdline = []rune{ch}
	case 'n':
		e.find(true)
	case 'N':
		e.find(false)
	}
}

// operator applies d, c or y with a motion: the same key again (whole
// lines), w, e, $, 0 or b.
func (e *vi) operator(op string, k key, n int) {
	l := e.line()
	if k.name == "" && string(k.r) == op {
		if op == "y" {
			e.yank = cloneLines(e.lines[e.cy:min(e.cy+n, len(e.lines))])
			e.yankLines = true
			e.msg = fmt.Sprintf("%d lines yanked", len(e.yank))
			return
		}
		e.snapshot()
		if op == "c" {
			indent := slices.Clone(l[:firstNonBlank(l)])
			e.deleteLines(e.cy, n)
			e.lines = slices.Insert(e.lines, min(e.cy, len(e.lines)), indent)
			if e.cy >= len(e.lines) {
				e.cy = len(e.lines) - 1
			}
			e.cx = len(indent)
			e.insertMode()
			return
		}
		e.deleteLines(e.cy, n)
		return
	}
	from, to := e.cx, e.cx
	switch {
	case k.r == 'w' && op == "c":
		to = e.cx
		for range n {
			to = wordEnd(l, to) + 1
		}
		if e.cx < len(l) && class(l[e.cx]) == 0 {
			to = wordForward(l, e.cx)
		}
	case k.r == 'w':
		for range n {
			to = wordForward(l, to)
		}
	case k.r == 'e':
		for range n {
			to = wordEnd(l, to) + 1
		}
	case k.r == '$' || k.name == "end":
		to = len(l)
	case k.r == '0' || k.name == "home":
		from, to = 0, e.cx
	case k.r == 'b':
		for range n {
			from = wordBack(l, from)
		}
	default:
		return
	}
	if op == "y" {
		e.yank = [][]rune{slices.Clone(l[max(0, from):min(len(l), to)])}
		e.yankLines = false
		e.cx = from
		return
	}
	e.snapshot()
	e.deleteRange(from, to)
	if op == "c" {
		e.insertMode()
	}
}

func init() {
	// "kubectl edit" runs vi in-process. EDITOR and KUBE_EDITOR may only name vi.
	editor.InProcess = func(args []string) error {
		switch filepath.Base(args[0]) {
		case "vi", "vim":
			t := editTerm.Load()
			if t == nil {
				return errors.New("unable to launch the editor: no terminal")
			}
			return runVi(context.Background(), t, args[len(args)-1])
		}
		return fmt.Errorf("unable to launch the editor %q: only vi works in the browser", strings.Join(args[:len(args)-1], " "))
	}
}
