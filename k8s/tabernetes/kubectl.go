package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall/js"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/rest"
	kubectlcmd "k8s.io/kubectl/pkg/cmd"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

type exitPanic struct {
	code int
	msg  string
}

var errCanceled = errors.New("canceled")

func init() {
	// kubectl calls os.Exit on fatal errors. Turn that into a panic that
	// runKubectl recovers in the goroutine of the invocation, so the process
	// survives and the message ends up in the right terminal stream.
	cmdutil.BehaviorOnFatal(func(msg string, c int) { panic(exitPanic{c, msg}) })
}

// jsWriter forwards writes to a JS callback.
type jsWriter struct{ fn js.Value }

func (w jsWriter) Write(b []byte) (int, error) {
	if w.fn.Type() != js.TypeFunction {
		return os.Stderr.Write(b)
	}
	w.fn.Invoke(string(b))
	return len(b), nil
}

// ctxWriter fails all writes once ctx is done. kubectl stops at the next
// write, which is the only way to stop a "kubectl get -w".
type ctxWriter struct {
	ctx context.Context
	w   io.Writer
}

func (w ctxWriter) Write(b []byte) (int, error) {
	if w.ctx.Err() != nil {
		return 0, errCanceled
	}
	return w.w.Write(b)
}

// runKubectl runs one kubectl invocation in-process and returns its exit code.
func runKubectl(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	if stdinFile(args) {
		// kubectl's resource builder reads "-f -" from os.Stdin directly,
		// so hand stdin over as a file instead.
		b, _ := io.ReadAll(stdin)
		f, err := os.CreateTemp("", "stdin-*.yaml")
		if err == nil {
			f.Write(b)
			f.Close()
			defer os.Remove(f.Name())
			for i, a := range args {
				switch {
				case a == "-" && i > 0 && (args[i-1] == "-f" || args[i-1] == "--filename"):
					args[i] = f.Name()
				case a == "-f-" || a == "--filename=-":
					args[i] = "--filename=" + f.Name()
				}
			}
		}
		stdin = strings.NewReader("")
	}
	out, errOut := ctxWriter{ctx, stdout}, ctxWriter{ctx, stderr}
	streams := genericiooptions.IOStreams{In: stdin, Out: out, ErrOut: errOut}

	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(exitPanic); ok {
				code = e.code
				if e.msg != "" && !strings.HasSuffix(e.msg, "\n") {
					e.msg += "\n"
				}
				fmt.Fprint(errOut, e.msg)
			} else {
				fmt.Fprintf(errOut, "kubectl panicked: %v\n", r)
				code = 2
			}
		}
		if ctx.Err() != nil {
			code = 130
		}
	}()

	// kubectl installs its warning printer as the process wide default. Here
	// the controllers share the process, so while kubectl runs (kubectl edit
	// waits in vi) their deprecation warnings would land in its output.
	// kubectl gets its own printer on its clients instead, and the default
	// stays silent.
	warnings := rest.NewWarningWriter(errOut, rest.WarningWriterOptions{Deduplicate: true})
	flags := genericclioptions.NewConfigFlags(true).WithDiscoveryBurst(300).WithDiscoveryQPS(50.0).WithWarningPrinter(streams).
		WithWrapConfigFn(func(c *rest.Config) *rest.Config {
			c.WarningHandler = warnings
			return c
		})
	cmd := kubectlcmd.NewDefaultKubectlCommandWithArgs(kubectlcmd.KubectlOptions{
		Arguments:   append([]string{"kubectl"}, args...),
		ConfigFlags: flags,
		IOStreams:   streams,
	})
	if pre := cmd.PersistentPreRunE; pre != nil {
		cmd.PersistentPreRunE = func(c *cobra.Command, args []string) error {
			err := pre(c, args)
			rest.SetDefaultWarningHandler(rest.NoWarnings{})
			return err
		}
	}
	defer rest.SetDefaultWarningHandler(rest.NoWarnings{})
	cmd.SilenceErrors = true
	cmd.SetArgs(args)
	cmd.SetIn(stdin)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	// port-forward, logs -f and wait stop with the context: Ctrl-C, kill %1.
	if err := cmd.ExecuteContext(ctx); err != nil {
		if ctx.Err() == nil {
			cmdutil.CheckErr(err)
		}
		return 1
	}
	return 0
}

func stdinFile(args []string) bool {
	for i, a := range args {
		if a == "-f-" || a == "--filename=-" || (a == "-" && i > 0 && (args[i-1] == "-f" || args[i-1] == "--filename")) {
			return true
		}
	}
	return false
}
