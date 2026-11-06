//go:build js

package term

import (
	"errors"
	"io"
	"os"
)

var ErrInvalidState = errors.New("Invalid terminal state")

type terminalState struct{}
type Termios struct{}

func stdStreams() (io.ReadCloser, io.Writer, io.Writer) { return os.Stdin, os.Stdout, os.Stderr }
func getFdInfo(in interface{}) (uintptr, bool) {
	if f, ok := in.(*os.File); ok {
		return f.Fd(), false
	}
	return 0, false
}
func getWinsize(fd uintptr) (*Winsize, error)        { return &Winsize{Height: 24, Width: 120}, nil }
func setWinsize(fd uintptr, ws *Winsize) error       { return nil }
func isTerminal(fd uintptr) bool                     { return false }
func restoreTerminal(fd uintptr, state *State) error { return nil }
func saveState(fd uintptr) (*State, error)           { return &State{}, nil }
func disableEcho(fd uintptr, state *State) error     { return nil }
func setRawTerminal(fd uintptr) (*State, error)      { return &State{}, nil }
func setRawTerminalOutput(uintptr) (*State, error)   { return &State{}, nil }
func makeRaw(fd uintptr) (*State, error)             { return &State{}, nil }
