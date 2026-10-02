//go:build !darwin && !linux

package repository

import "os"

// On other systems File.Sync is the only flush: fsync on Unix,
// FlushFileBuffers on Windows. Both reach stable storage.
func fullFlush(file *os.File) error { return file.Sync() }

func normalFlush(file *os.File) error { return file.Sync() }
