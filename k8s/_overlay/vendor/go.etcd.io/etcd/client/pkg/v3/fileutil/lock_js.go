//go:build js

package fileutil

import "os"

func TryLockFile(path string, flag int, perm os.FileMode) (*LockedFile, error) {
	f, err := os.OpenFile(path, flag, perm)
	if err != nil {
		return nil, err
	}
	return &LockedFile{f}, nil
}

func LockFile(path string, flag int, perm os.FileMode) (*LockedFile, error) {
	return TryLockFile(path, flag, perm)
}
