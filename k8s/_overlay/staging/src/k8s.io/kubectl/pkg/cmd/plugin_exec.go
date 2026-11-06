//go:build !js

package cmd

import "syscall"

var syscallExec = syscall.Exec
