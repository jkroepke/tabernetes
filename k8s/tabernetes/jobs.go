package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Background jobs, such as "k port-forward deploy/web 8080:80 &".
//
// Every tab has its own jobs (session.go).
//
// mvdan.cc/sh hands a job the context of its command line, and that context
// ends with the line, so every job would stop right away. k8s/patch.go adds
// the hook interp.BackgroundContext, and a job gets its own context here:
// Ctrl-C leaves it alone, "kill %1" ends it.

type job struct {
	id     int
	cmd    string
	cancel context.CancelFunc
}

type jobTable struct {
	mu   sync.Mutex
	list []*job // by id
}

func init() {
	interp.BackgroundContext = func(ctx context.Context, stmt *syntax.Stmt) (context.Context, func()) {
		if s := sessionFrom(ctx); s != nil {
			return s.jobs.start(ctx, stmt)
		}
		return ctx, func() {}
	}
}

// start registers a job and returns its context and the func the shell calls
// when the job ends.
func (t *jobTable) start(ctx context.Context, stmt *syntax.Stmt) (context.Context, func()) {
	var cmd strings.Builder
	syntax.NewPrinter(syntax.SingleLine(true)).Print(&cmd, stmt)
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	t.mu.Lock()
	j := &job{id: 1, cmd: cmd.String(), cancel: cancel}
	if n := len(t.list); n > 0 {
		j.id = t.list[n-1].id + 1
	}
	t.list = append(t.list, j)
	t.mu.Unlock()
	fmt.Fprintf(holdWriter{termFrom(ctx)}, "[%d] %s\n", j.id, j.cmd)
	return ctx, func() { t.stop(j) }
}

func (t *jobTable) stop(j *job) {
	j.cancel()
	t.mu.Lock()
	t.list = slices.DeleteFunc(t.list, func(o *job) bool { return o == j })
	t.mu.Unlock()
}

// stopAll ends every job, for "exit".
func (t *jobTable) stopAll() {
	t.mu.Lock()
	list := slices.Clone(t.list)
	t.mu.Unlock()
	for _, j := range list {
		t.stop(j)
	}
}

// find resolves a job spec: %1, %% or %+ (the newest).
func (t *jobTable) find(spec string) *job {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.list) == 0 {
		return nil
	}
	if spec == "%%" || spec == "%+" {
		return t.list[len(t.list)-1]
	}
	id, err := strconv.Atoi(strings.TrimPrefix(spec, "%"))
	if err != nil || !strings.HasPrefix(spec, "%") {
		return nil
	}
	for _, j := range t.list {
		if j.id == id {
			return j
		}
	}
	return nil
}

func (t *jobTable) jobsCmd(hc interp.HandlerContext) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, j := range t.list {
		fmt.Fprintf(hc.Stdout, "[%d]  Running  %s\n", j.id, j.cmd)
	}
	return nil
}

// killCmd ends jobs. Signals do not exist here, so "-9" and the like are
// accepted and ignored.
func (t *jobTable) killCmd(hc interp.HandlerContext, args []string) error {
	var specs []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-s" || a == "-n":
			i++
		case strings.HasPrefix(a, "-") && len(a) > 1:
		default:
			specs = append(specs, a)
		}
	}
	if len(specs) == 0 {
		fmt.Fprintln(hc.Stderr, "usage: kill %<job> (see jobs)")
		return interp.ExitStatus(2)
	}
	code := 0
	for _, spec := range specs {
		j := t.find(spec)
		if j == nil {
			fmt.Fprintf(hc.Stderr, "kill: %s: no such job (see jobs)\n", spec)
			code = 1
			continue
		}
		t.stop(j)
	}
	return exitCode(code)
}
