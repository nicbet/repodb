//go:build !windows

package repository

import "os"

// syncDir flushes a directory so a rename inside it survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
