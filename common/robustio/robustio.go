// Package robustio wraps file operations that fail transiently on Windows.
//
// Go on Windows opens files without FILE_SHARE_DELETE, so renaming onto or
// removing a path fails while another process briefly has it open. Unix has
// no such window: rename replaces the directory entry and readers keep the
// old inode. On Windows, Rename and Remove retry for a bounded time; on every
// other platform they are exactly os.Rename and os.Remove.
package robustio

import (
	"math/rand"
	"time"
)

// timeout bounds the total time spent retrying one operation. It matches the
// Go toolchain's cmd/internal/robustio and is a variable only for tests.
var timeout = 2 * time.Second

// retry runs op until it succeeds, fails with an error transient rejects, or
// the next attempt would start after timeout. It returns op's last error.
func retry(op func() error, transient func(error) bool) error {
	var start time.Time
	sleep := time.Millisecond
	for {
		err := op()
		if err == nil || !transient(err) {
			return err
		}
		if start.IsZero() {
			start = time.Now()
		} else if time.Since(start)+sleep >= timeout {
			return err
		}
		time.Sleep(sleep)
		sleep += time.Duration(rand.Int63n(int64(sleep)))
	}
}
