package robustio

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// Rename is os.Rename, retried while the destination is held open elsewhere.
func Rename(oldpath, newpath string) error {
	return retry(func() error { return os.Rename(oldpath, newpath) }, isTransient)
}

// Remove is os.Remove, retried while the file is held open elsewhere.
func Remove(path string) error {
	return retry(func() error { return os.Remove(path) }, isTransient)
}

func isTransient(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == windows.ERROR_ACCESS_DENIED || errno == windows.ERROR_SHARING_VIOLATION
}
