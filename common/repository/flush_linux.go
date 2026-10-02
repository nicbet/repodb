package repository

import (
	"errors"
	"os"
	"syscall"
)

// fullFlush and normalFlush are the same on Linux: fdatasync reaches stable
// storage, flushing the drive's volatile cache where it has one.
func fullFlush(file *os.File) error { return fdatasync(file) }

func normalFlush(file *os.File) error { return fdatasync(file) }

func fdatasync(file *os.File) error {
	for {
		err := syscall.Fdatasync(int(file.Fd()))
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
