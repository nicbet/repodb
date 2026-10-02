package repository

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// fBarrierFsync is fcntl's F_BARRIERFSYNC, which x/sys/unix doesn't name.
const fBarrierFsync = 85

// fullFlush forces the drive's cache to stable media. Go's File.Sync issues
// F_FULLFSYNC on macOS.
func fullFlush(file *os.File) error { return file.Sync() }

// normalFlush hands the data to the drive with F_BARRIERFSYNC: writes before
// the barrier reach the device before writes after it, but the drive may still
// hold them in its cache. Filesystems that don't support it get a full flush.
func normalFlush(file *os.File) error {
	for {
		_, err := unix.FcntlInt(file.Fd(), fBarrierFsync, 0)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EINVAL):
			return file.Sync()
		default:
			return err
		}
	}
}
