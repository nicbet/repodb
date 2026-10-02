package repository

import (
	"fmt"
	"os"
)

// Durability selects how hard the journal flushes each committed transaction.
// The zero value is DurabilityNormal.
//
// The journal is append-only and every record is length-prefixed and
// checksummed, so a crash can only lose a suffix of it, never corrupt it: a
// partial final record is recognized and truncated on replay. The levels
// differ only in how many of the newest acknowledged commits a crash can take
// with it.
type Durability string

const (
	// DurabilityFull forces each commit to stable storage, so it survives
	// power loss: F_FULLFSYNC on macOS, fdatasync on Linux, FlushFileBuffers
	// on Windows.
	DurabilityFull Durability = "full"
	// DurabilityNormal survives process and OS crashes. On macOS it uses
	// F_BARRIERFSYNC, which hands the data to the drive in order without
	// forcing the drive's cache, so a power loss may drop the newest commits.
	// Elsewhere it is the same flush as DurabilityFull.
	DurabilityNormal Durability = "normal"
	// DurabilityOff skips the per-commit flush. Commits survive a process
	// crash (the OS still holds them); an OS crash or power loss may lose
	// commits the OS had not written back yet.
	DurabilityOff Durability = "off"
)

// ParseDurability accepts "full", "normal" and "off". The empty string means
// DurabilityNormal.
func ParseDurability(value string) (Durability, error) {
	switch Durability(value) {
	case "", DurabilityNormal:
		return DurabilityNormal, nil
	case DurabilityFull, DurabilityOff:
		return Durability(value), nil
	}
	return "", fmt.Errorf("unknown durability %q (want full, normal or off)", value)
}

func (d Durability) orDefault() Durability {
	if d == "" {
		return DurabilityNormal
	}
	return d
}

// flushFile flushes file at the given level. Tests replace it to record which
// flush ran and when.
var flushFile = func(file *os.File, level Durability) error {
	switch level {
	case DurabilityOff:
		return nil
	case DurabilityFull:
		return fullFlush(file)
	default:
		return normalFlush(file)
	}
}
