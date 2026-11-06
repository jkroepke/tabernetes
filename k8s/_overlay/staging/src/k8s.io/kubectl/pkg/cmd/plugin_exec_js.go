//go:build js

package cmd

import "errors"

func syscallExec(string, []string, []string) error {
	return errors.New("plugins are not supported in the browser")
}
