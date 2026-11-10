package main

import (
	"context"
	"io"
	"reflect"
	"strings"
	"time"
)

// kubectl exec -i without -t on the terminal (-t: ttyexec.go). The shell has
// no terminal device: commands get complete lines from the browser and stdin
// is empty. For exec with stdin, the terminal goes into line mode instead,
// edits each line itself and sends it to the pod when Enter is pressed.
// Ctrl-C goes to the pod as \x03 and ends stdin; so does Ctrl-D on an empty
// line.

// terminalStdin is the stdin of a command line that reads nothing else: no
// pipe, no redirect.
type terminalStdin struct{}

func (*terminalStdin) Read([]byte) (int, error) { return 0, io.EOF }

var shellStdin = &terminalStdin{}

// fromTerminal tells whether r is the stdin of the shell itself. The
// interpreter wraps it in a struct of its own, which embeds the reader.
func fromTerminal(r io.Reader) bool {
	if v := reflect.ValueOf(r); v.Kind() == reflect.Struct {
		if f := v.FieldByName("Reader"); f.IsValid() && f.CanInterface() {
			r, _ = f.Interface().(io.Reader)
		}
	}
	return r == io.Reader(shellStdin)
}

// interactiveExec returns the arguments for kubectl exec in line mode when
// args are "exec" with -i.
func interactiveExec(args []string) ([]string, bool) {
	if len(args) == 0 || args[0] != "exec" {
		return nil, false
	}
	out := []string{"exec"}
	stdin := false
	for i, a := range args[1:] {
		if a == "--" {
			out = append(out, args[1+i:]...)
			break
		}
		switch {
		case a == "-i" || a == "--stdin" || a == "--stdin=true":
			stdin = true
			continue
		case a == "-t" || a == "--tty" || a == "--tty=true":
			continue
		case len(a) > 2 && a[0] == '-' && a[1] != '-' && strings.Trim(a[1:], "it") == "":
			// -it, -ti
			stdin = stdin || strings.Contains(a, "i")
			continue
		}
		out = append(out, a)
	}
	if !stdin {
		return nil, false
	}
	// kubectl needs -i to send stdin at all
	return append([]string{"exec", "-i"}, out[1:]...), true
}

// execLines runs kubectl exec -i with the line editing of the terminal as
// stdin.
func execLines(ctx context.Context, args []string) int {
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stdin, toPod := io.Pipe()
	done := make(chan int, 1)
	go func() { done <- runKubectl(execCtx, args, stdin, tty, tty) }()

	leave := tty.enterLine()
	defer leave()
	lines := make(chan string, 16)
	go func() {
		for l := range lines {
			if _, err := io.WriteString(toPod, l); err != nil {
				return
			}
		}
		toPod.Close()
	}()
	var line []rune
	closed := false
	for {
		select {
		case code := <-done:
			if !closed {
				close(lines)
			}
			stdin.Close()
			return code
		case <-ctx.Done():
			// kubectl exec does not stop with the context: end its stdin.
			if !closed {
				lines <- "\x03\n"
				close(lines)
			}
			stdin.Close()
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
			return 130
		case s := <-tty.keys:
			for _, k := range parseKeys(s) {
				switch {
				case closed:
				case k.name == "ctrl-c":
					// as a tty would: the pod reads it from stdin
					io.WriteString(tty, "^C\n")
					closed = true
					lines <- "\x03\n"
					close(lines)
				case k.name == "ctrl-d" && len(line) == 0:
					io.WriteString(tty, "\n")
					closed = true
					close(lines)
				case k.name == "enter":
					io.WriteString(tty, "\n")
					lines <- string(line) + "\n"
					line = nil
				case k.name == "backspace":
					if len(line) > 0 {
						line = line[:len(line)-1]
						io.WriteString(tty, "\b \b")
					}
				case k.name == "" && k.r != 0:
					line = append(line, k.r)
					io.WriteString(tty, string(k.r))
				}
			}
		}
	}
}
