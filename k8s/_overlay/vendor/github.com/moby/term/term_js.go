//go:build js

package term

import (
	"errors"
	"io"
	"os"
)

var ErrInvalidState = errors.New("Invalid terminal state")

// The terminal of the page (cmd/tabernetes/tty.go). It has no file
// descriptor: its streams report TerminalFd from Fd(), and the hooks below
// do what the ioctls of a tty would. kubectl exec -t, attach -t and run -t
// then work as on a real terminal.
var (
	TerminalFd uintptr = 0x7ab

	// TerminalStreams returns stdin, stdout and stderr of the terminal.
	TerminalStreams func() (io.ReadCloser, io.Writer, io.Writer)
	// TerminalSize returns the size of the terminal.
	TerminalSize func() (width, height uint16)
	// TerminalRaw switches the terminal to raw mode until leave is called.
	TerminalRaw func() (leave func())
)

type terminalState struct{ leave func() }
type Termios struct{}

func stdStreams() (io.ReadCloser, io.Writer, io.Writer) {
	if TerminalStreams != nil {
		return TerminalStreams()
	}
	return os.Stdin, os.Stdout, os.Stderr
}

func getFdInfo(in interface{}) (uintptr, bool) {
	if f, ok := in.(interface{ Fd() uintptr }); ok {
		return f.Fd(), isTerminal(f.Fd())
	}
	return 0, false
}

func getWinsize(fd uintptr) (*Winsize, error) {
	if fd == TerminalFd && TerminalSize != nil {
		w, h := TerminalSize()
		return &Winsize{Height: h, Width: w}, nil
	}
	return &Winsize{Height: 24, Width: 120}, nil
}

func setWinsize(fd uintptr, ws *Winsize) error { return nil }
func isTerminal(fd uintptr) bool               { return fd == TerminalFd && TerminalRaw != nil }

func restoreTerminal(fd uintptr, state *State) error {
	if state != nil && state.leave != nil {
		state.leave()
		state.leave = nil
	}
	return nil
}

func saveState(fd uintptr) (*State, error)       { return &State{}, nil }
func disableEcho(fd uintptr, state *State) error { return nil }

func setRawTerminal(fd uintptr) (*State, error) { return makeRaw(fd) }

func setRawTerminalOutput(uintptr) (*State, error) { return &State{}, nil }

func makeRaw(fd uintptr) (*State, error) {
	if !isTerminal(fd) {
		return &State{}, nil
	}
	return &State{leave: TerminalRaw()}, nil
}
