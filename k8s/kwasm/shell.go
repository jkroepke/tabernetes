package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall/js"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// shell is the POSIX shell of the browser terminal. It is mvdan.cc/sh, a shell
// interpreter written in Go, so it runs in the same wasm binary and sees the
// same in-memory file system as the control plane. There are no processes:
// every command is a Go function (kubectl, coreutils) or a JS function
// (join, accept, logs, ...).
type shell struct {
	mu     sync.Mutex
	runner *interp.Runner
	cancel context.CancelFunc

	// set for the duration of one run
	jsBuiltins js.Value
}

func newShell() *shell {
	s := &shell{}
	r, err := interp.New(
		interp.Interactive(true),
		interp.Dir("/root"),
		interp.Env(expand.ListEnviron(append(os.Environ(),
			"HOME=/root", "USER=root", "PATH=/usr/bin:/bin", "SHELL=/bin/sh", "TERM=xterm-256color",
			"KUBECONFIG=/root/.kube/config",
		)...)),
		interp.CallHandler(s.call),
		interp.OpenHandler(openHandler),
		interp.ExecHandlers(s.exec),
	)
	if err != nil {
		panic(err)
	}
	s.runner = r
	return s
}

// openHandler adds /dev/null, which the in-memory file system does not have.
func openHandler(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
	if path == "/dev/null" {
		return devNull{}, nil
	}
	return interp.DefaultOpenHandler()(ctx, path, flag, perm)
}

type devNull struct{}

func (devNull) Read([]byte) (int, error)    { return 0, io.EOF }
func (devNull) Write(b []byte) (int, error) { return len(b), nil }
func (devNull) Close() error                { return nil }

// incomplete reports whether src needs more lines, for example an open quote,
// a here-document or a loop without done.
func incomplete(src string) bool {
	_, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	// An open here-document is not "incomplete" for the parser, but for an
	// interactive shell it is: the body comes with the next lines.
	return syntax.IsIncomplete(err) || (err != nil && strings.Contains(err.Error(), "unclosed here-document"))
}

// run executes src and returns the exit status of the last command.
func (s *shell) run(src string, out js.Value, builtins js.Value) int {
	w := jsWriter{out}
	f, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		fmt.Fprintf(w, "kwasm: %v\n", err)
		return 2
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.cancel, s.jsBuiltins = cancel, builtins
	tty.setOutput(w)
	hw := holdWriter{tty}
	interp.StdIO(strings.NewReader(""), ctxWriter{ctx, hw}, ctxWriter{ctx, hw})(s.runner)

	err = s.runner.Run(ctx, f)
	code := 0
	if st, ok := interp.IsExitStatus(err); ok {
		code = int(st)
	} else if err != nil {
		if ctx.Err() == nil {
			fmt.Fprintf(w, "kwasm: %v\n", err)
		}
		code = 1
	}
	if ctx.Err() != nil {
		code = 130
	}
	if s.runner.Exited() {
		// "exit" starts a fresh shell, the cluster stays.
		s.runner.Reset()
	}
	return code
}

func (s *shell) dir() string { return s.runner.Dir }

// shellBuiltins are the builtins of mvdan.cc/sh worth listing in "help".
// interp.IsBuiltin filters out any that a newer version drops.
var shellBuiltins = []string{
	"alias", "cd", "echo", "eval", "exit", "export", "false", "printf", "pwd",
	"read", "set", "shift", "source", "test", "true", "type", "unset", "wait",
}

// commands lists the Go commands for the help text of the terminal.
func commands() map[string]any {
	var tools, builtins []any
	for _, name := range slices.Sorted(maps.Keys(coreutils)) {
		tools = append(tools, name)
	}
	for _, name := range shellBuiltins {
		if interp.IsBuiltin(name) {
			builtins = append(builtins, name)
		}
	}
	return map[string]any{"tools": tools, "builtins": builtins}
}

// stop cancels the running command line (Ctrl-C).
func (s *shell) stop() {
	if c := s.cancel; c != nil {
		c()
	}
}

// call renames the bare "help" so it reaches the terminal help instead of the
// bash builtin. "help <builtin>" still goes to the builtin.
func (s *shell) call(ctx context.Context, args []string) ([]string, error) {
	if len(args) == 1 && args[0] == "help" {
		return []string{"kwasm-help"}, nil
	}
	return args, nil
}

func (s *shell) exec(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	var self interp.ExecHandlerFunc
	self = func(ctx context.Context, args []string) error {
		hc := interp.HandlerCtx(ctx)
		name := args[0]
		switch {
		case name == "kwasm-help":
			return s.callJS(ctx, hc, "help", args[1:])
		case name == "kubectl" || name == "k":
			return s.kubectl(ctx, hc, args[1:])
		case interp.IsBuiltin(name):
			// reached through xargs, for example "xargs echo"
			if err := hc.Builtin(ctx, args); err != nil {
				var code int
				if _, scanErr := fmt.Sscanf(err.Error(), "builtin exit status %d", &code); scanErr == nil {
					return exitCode(code)
				}
				return err
			}
			return nil
		case coreutils[name] != nil:
			return coreutils[name](ctx, hc, args[1:], self)
		case s.jsBuiltins.Type() == js.TypeObject && s.jsBuiltins.Get(name).Type() == js.TypeFunction:
			return s.callJS(ctx, hc, name, args[1:])
		}
		fmt.Fprintf(hc.Stderr, "kwasm: %s: command not found (try \"help\")\n", args[0])
		return interp.ExitStatus(127)
	}
	return self
}

func (s *shell) kubectl(ctx context.Context, hc interp.HandlerContext, args []string) error {
	// kubectl resolves -f and --kubeconfig against the process directory.
	if err := os.Chdir(hc.Dir); err != nil {
		fmt.Fprintf(hc.Stderr, "kubectl: %v\n", err)
		return interp.ExitStatus(1)
	}
	done := make(chan int, 1)
	go func() { done <- runKubectl(ctx, args, hc.Stdin, hc.Stdout, hc.Stderr) }()
	select {
	case code := <-done:
		return exitCode(code)
	case <-ctx.Done():
		// kubectl notices on its next write; don't wait for that.
		return interp.ExitStatus(130)
	}
}

// callJS runs a builtin of the terminal: fn(args, {write, stdin}) -> Promise.
func (s *shell) callJS(ctx context.Context, hc interp.HandlerContext, name string, args []string) error {
	stdin := ""
	if hc.Stdin != nil {
		b, _ := io.ReadAll(hc.Stdin)
		stdin = string(b)
	}
	jsArgs := make([]any, len(args))
	for i, a := range args {
		jsArgs[i] = a
	}
	write := js.FuncOf(func(this js.Value, a []js.Value) any {
		if len(a) > 0 {
			io.WriteString(hc.Stdout, a[0].String())
		}
		return nil
	})
	c := js.Global().Get("Object").New()
	c.Set("write", write)
	c.Set("stdin", stdin)
	v, err := await(ctx, s.jsBuiltins.Get(name).Invoke(js.ValueOf(jsArgs), c))
	if !errors.Is(err, context.Canceled) {
		write.Release() // a canceled builtin may still write
	}
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintf(hc.Stderr, "%s: %v\n", name, err)
		}
		return interp.ExitStatus(1)
	}
	if v.Type() == js.TypeNumber {
		return exitCode(v.Int())
	}
	return nil
}

func exitCode(code int) error {
	if code == 0 {
		return nil
	}
	return interp.ExitStatus(uint8(code))
}

// await waits for v if it is a Promise.
func await(ctx context.Context, v js.Value) (js.Value, error) {
	if v.Type() != js.TypeObject || v.Get("then").Type() != js.TypeFunction {
		return v, nil
	}
	type result struct {
		v   js.Value
		err error
	}
	ch := make(chan result, 1)
	ok := js.FuncOf(func(this js.Value, a []js.Value) any {
		ch <- result{v: a[0]}
		return nil
	})
	fail := js.FuncOf(func(this js.Value, a []js.Value) any {
		msg := a[0].String()
		if a[0].Type() == js.TypeObject && a[0].Get("message").Type() == js.TypeString {
			msg = a[0].Get("message").String()
		}
		ch <- result{err: errors.New(msg)}
		return nil
	})
	v.Call("then", ok, fail)
	select {
	case r := <-ch:
		ok.Release()
		fail.Release()
		return r.v, r.err
	case <-ctx.Done():
		// The promise may still settle later, so the callbacks stay alive.
		return js.Undefined(), ctx.Err()
	}
}
