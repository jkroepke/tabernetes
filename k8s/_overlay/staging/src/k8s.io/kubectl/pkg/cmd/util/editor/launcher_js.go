//go:build js

package editor

import (
	"fmt"
	"path/filepath"
	"strings"
)

// InProcess runs the editor inside the process: js/wasm cannot start one.
// tabernetes sets it to its own vi. args is the editor command line, the file last.
var InProcess func(args []string) error

func (e Editor) args(path string) []string {
	return append(append([]string{}, e.Args...), path)
}

// Launch opens path with the in-process editor.
func (e Editor) Launch(path string) error {
	if len(e.Args) == 0 {
		return fmt.Errorf("no editor defined, can't open %s", path)
	}
	if InProcess == nil {
		return fmt.Errorf("unable to launch the editor %q", strings.Join(e.Args, " "))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	return InProcess(e.args(abs))
}
