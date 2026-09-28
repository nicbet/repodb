//go:build !windows

package robustio

import "os"

// Rename is os.Rename; rename over an open file cannot fail transiently here.
func Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

// Remove is os.Remove; removing an open file cannot fail transiently here.
func Remove(path string) error { return os.Remove(path) }
