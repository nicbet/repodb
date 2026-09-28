//go:build !unix

package main

// peakRSS is unavailable on this platform.
func peakRSS() (int64, bool) { return 0, false }
