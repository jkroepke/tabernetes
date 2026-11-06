//go:build js

package mount

func accessExists(path string) bool { return false }
func unameRelease() ([]byte, bool)  { return nil, false }
