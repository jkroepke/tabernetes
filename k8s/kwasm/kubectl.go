package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"syscall/js"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/rest"
	kubectlcmd "k8s.io/kubectl/pkg/cmd"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

type exitPanic struct{ code int }

var errCanceled = errors.New("canceled")

// jsWriter forwards writes to a JS callback until it gets canceled.
type jsWriter struct {
	fn       js.Value
	canceled *atomic.Bool
}

func (w jsWriter) Write(b []byte) (int, error) {
	if w.canceled.Load() {
		return 0, errCanceled
	}
	if w.fn.Type() != js.TypeFunction {
		return os.Stderr.Write(b)
	}
	w.fn.Invoke(string(b))
	return len(b), nil
}

var (
	kubectlCancel atomic.Pointer[atomic.Bool]
)

// runKubectl runs one kubectl invocation in-process and returns its exit code.
func runKubectl(args []string, stdin string, out js.Value) (code int) {
	if stdin != "" {
		// kubectl's resource builder reads "-f -" from os.Stdin directly,
		// so hand the heredoc over as a file instead.
		f, err := os.CreateTemp("", "stdin-*.yaml")
		if err == nil {
			f.WriteString(stdin)
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
	}
	canceled := &atomic.Bool{}
	kubectlCancel.Store(canceled)
	w := jsWriter{fn: out, canceled: canceled}
	streams := genericiooptions.IOStreams{In: strings.NewReader(stdin), Out: w, ErrOut: w}

	cmdutil.BehaviorOnFatal(func(msg string, c int) {
		if msg != "" {
			if !strings.HasSuffix(msg, "\n") {
				msg += "\n"
			}
			fmt.Fprint(w, msg)
		}
		panic(exitPanic{c})
	})
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(exitPanic); ok {
				code = e.code
				return
			}
			fmt.Fprintf(w, "kubectl panicked: %v\n", r)
			code = 2
		}
		if canceled.Load() {
			code = 130
		}
	}()

	cmd := kubectlcmd.NewDefaultKubectlCommandWithArgs(kubectlcmd.KubectlOptions{
		Arguments:   append([]string{"kubectl"}, args...),
		ConfigFlags: genericclioptions.NewConfigFlags(true).WithDiscoveryBurst(300).WithDiscoveryQPS(50.0).WithWarningPrinter(streams),
		IOStreams:   streams,
	})
	// kubectl installs its warning printer as process wide default; reset it so
	// warnings of the in-process controllers do not end up in the terminal.
	defer rest.SetDefaultWarningHandler(rest.NewWarningWriter(os.Stderr, rest.WarningWriterOptions{}))
	cmd.SilenceErrors = true
	cmd.SetArgs(args)
	cmd.SetOut(w)
	cmd.SetErr(w)
	if err := cmd.Execute(); err != nil {
		if !canceled.Load() {
			cmdutil.CheckErr(err)
		}
		return 1
	}
	return 0
}

func cancelKubectl() {
	if c := kubectlCancel.Load(); c != nil {
		c.Store(true)
	}
}
