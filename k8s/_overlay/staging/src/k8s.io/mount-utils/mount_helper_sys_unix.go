//go:build !windows && !js

package mount

import (
	"bytes"
	"syscall"

	"golang.org/x/sys/unix"
)

func accessExists(path string) bool { return syscall.Access(path, syscall.F_OK) == nil }

func unameRelease() ([]byte, bool) {
	uname := unix.Utsname{}
	if err := unix.Uname(&uname); err != nil {
		return nil, false
	}
	end := bytes.IndexByte(uname.Release[:], 0)
	return uname.Release[:end], true
}
