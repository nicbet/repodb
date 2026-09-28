//go:build unix

package integration_test

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// peakRSS returns the process's peak resident set size in bytes.
func peakRSS() (int64, bool) {
	var usage unix.Rusage
	if unix.Getrusage(unix.RUSAGE_SELF, &usage) != nil {
		return 0, false
	}
	peak := int64(usage.Maxrss)
	if runtime.GOOS != "darwin" {
		peak *= 1024
	}
	return peak, true
}
