package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall/js"
	"time"
)

// A session is one terminal tab: its shell, its terminal, its jobs and its
// node (tab-<n>). Where the browser has a SharedWorker, the control plane
// runs there (web/src/cluster.js), and so do the sessions of all tabs: a tab
// then runs no Go at all. It only shows the terminal, runs the VMs of its
// pods (a shared worker cannot start workers) and holds the WebRTC
// connections. Without a SharedWorker, the tab runs the control plane and
// its one session itself.
//
// host is what the tab gives the session, in the tab itself or as stubs in
// the worker that post to the tab (web/src/bridge.js):
//
//	status(text)   a status line in the terminal of the tab
//	vm             the container runtime, web/src/vm.js (optional)
//	gpu            WebGPU for llm, web/src/gpu.js (optional)
type session struct {
	name, ip string
	host     js.Value
	term     *terminal
	sh       *shell
	jobs     *jobTable

	nodeMu sync.Mutex
	node   *tabNode
	closed bool
}

var (
	sessionsMu sync.Mutex
	sessions   = map[string]*session{}
	nextTab    = 1
)

type sessionKey struct{}

// sessionFrom returns the session of a command line, or nil.
func sessionFrom(ctx context.Context) *session {
	s, _ := ctx.Value(sessionKey{}).(*session)
	return s
}

// termFrom returns the terminal of a command line. Without a session (in
// tests) it is a terminal that drops what is written.
func termFrom(ctx context.Context) *terminal {
	if s := sessionFrom(ctx); s != nil {
		return s.term
	}
	return newTerminal()
}

// openTab adds a tab: it gets the next name and address and a shell.
// Its node registers in the background: the shell does not need it, and on
// a new cluster the node waits seconds for its pod CIDR.
func openTab(host js.Value) (*session, error) {
	if current == nil {
		return nil, errors.New("the cluster is not ready yet")
	}
	sessionsMu.Lock()
	n := nextTab
	if n > 250 {
		sessionsMu.Unlock()
		return nil, errors.New("too many tabs")
	}
	nextTab++
	s := &session{
		name: fmt.Sprintf("tab-%d", n),
		ip:   fmt.Sprintf("10.0.1.%d", n),
		host: host,
		term: newTerminal(),
		jobs: &jobTable{},
	}
	s.sh = newShell(s)
	sessions[s.name] = s
	sessionsMu.Unlock()
	go s.register()
	return s, nil
}

// status writes a line to the status of the tab.
func (s *session) status(text string) {
	if f := s.host.Get("status"); f.Type() == js.TypeFunction {
		f.Invoke(text)
	}
}

// register makes the tab a node. A tab without WebGPU is a node too,
// without tabernetes.dev/webgpu; a failure costs the tab only its node.
func (s *session) register() {
	s.status("registering this tab as the node " + s.name)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := s.startNode(ctx); err != nil {
		s.status("this tab is no node: " + err.Error())
	}
}

// startNode registers the node of the tab, once. After a failure, the next
// call tries again (llm, after a reload of the GPU driver).
func (s *session) startNode(ctx context.Context) (*tabNode, error) {
	s.nodeMu.Lock()
	defer s.nodeMu.Unlock()
	if s.closed {
		return nil, errors.New("the tab is closed")
	}
	if s.node != nil {
		return s.node, nil
	}
	n, err := newTabNode(ctx, s.name, s.ip, s.host)
	if err != nil {
		return nil, err
	}
	s.node = n
	return n, nil
}

// close ends the session of a tab that is gone: its jobs (a port-forward
// would listen on), the running command line and its node. dropNode
// deletes the node from the cluster.
func (s *session) close() {
	sessionsMu.Lock()
	delete(sessions, s.name)
	sessionsMu.Unlock()
	s.sh.stop()
	s.jobs.stopAll()
	s.nodeMu.Lock()
	s.closed = true
	n := s.node
	s.node = nil
	s.nodeMu.Unlock()
	if n != nil {
		n.close()
	}
}

// openSessions is the number of open sessions.
func openSessions() int {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	return len(sessions)
}

// api is the session for JavaScript (web/src/terminal.js, bridge.js).
func (s *session) api() js.Value {
	api := js.Global().Get("Object").New()
	set := func(name string, f func(a []js.Value) any) {
		api.Set(name, js.FuncOf(func(_ js.Value, a []js.Value) any { return f(a) }))
	}
	api.Set("name", s.name)
	api.Set("ip", s.ip)
	// shell(src, write: (s) => void, builtins: {[name]: (args, {write, stdin}) => Promise}): Promise<number>
	set("shell", func(a []js.Value) any {
		src, out, builtins := a[0].String(), a[1], js.Undefined()
		if len(a) > 2 {
			builtins = a[2]
		}
		return promise(func() (any, error) { return s.sh.run(src, out, builtins), nil })
	})
	// incomplete(src): true while src needs more lines
	set("incomplete", func(a []js.Value) any { return incomplete(a[0].String()) })
	// commands(): {tools, builtins} for the help text
	set("commands", func([]js.Value) any { return commands() })
	// key(data): a key while a command runs. A full screen program (vi,
	// kubectl edit, exec -it) gets every key, and key returns true. Otherwise
	// the terminal handles it (Ctrl-C) and key returns false.
	set("key", func(a []js.Value) any {
		if !s.term.raw.Load() {
			return false
		}
		s.term.input(a[0].String())
		return true
	})
	// resize(cols, rows) tells programs the terminal size
	set("resize", func(a []js.Value) any { s.term.resize(a[0].Int(), a[1].Int()); return nil })
	// complete(line, builtins: string[]): Promise<{word, items, noSpace}>, tab completion
	set("complete", func(a []js.Value) any {
		line := a[0].String()
		var builtins []string
		if len(a) > 1 && a[1].Type() == js.TypeObject {
			for i := range a[1].Length() {
				builtins = append(builtins, a[1].Index(i).String())
			}
		}
		return promise(func() (any, error) {
			c := s.sh.complete(line, builtins)
			items := make([]any, len(c.items))
			for i, it := range c.items {
				items[i] = it
			}
			return map[string]any{"word": c.word, "items": items, "noSpace": c.noSpace}, nil
		})
	})
	// cwd(): the working directory of the shell
	set("cwd", func([]js.Value) any { return s.sh.dir() })
	// cancel() stops the running command line (Ctrl-C)
	set("cancel", func([]js.Value) any { s.sh.stop(); return nil })
	return api
}
