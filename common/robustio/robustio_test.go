package robustio

import (
	"errors"
	"testing"
	"time"
)

var (
	errBusy  = errors.New("busy")
	errFatal = errors.New("fatal")
)

func isBusy(err error) bool { return errors.Is(err, errBusy) }

func TestRetrySucceedsAfterTransientFailures(t *testing.T) {
	calls := 0
	err := retry(func() error {
		calls++
		if calls < 4 {
			return errBusy
		}
		return nil
	}, isBusy)
	if err != nil || calls != 4 {
		t.Fatalf("err=%v calls=%d, want nil after 4 calls", err, calls)
	}
}

func TestRetryStopsOnNonTransientError(t *testing.T) {
	calls := 0
	err := retry(func() error { calls++; return errFatal }, isBusy)
	if !errors.Is(err, errFatal) || calls != 1 {
		t.Fatalf("err=%v calls=%d, want errFatal after 1 call", err, calls)
	}
}

func TestRetryGivesUpAfterTimeout(t *testing.T) {
	defer func(d time.Duration) { timeout = d }(timeout)
	timeout = 50 * time.Millisecond
	calls := 0
	start := time.Now()
	err := retry(func() error { calls++; return errBusy }, isBusy)
	elapsed := time.Since(start)
	if !errors.Is(err, errBusy) {
		t.Fatalf("err=%v, want errBusy", err)
	}
	if calls < 2 {
		t.Fatalf("calls=%d, want retries before giving up", calls)
	}
	if elapsed > 5*timeout {
		t.Fatalf("retried for %v, want bounded by ~%v", elapsed, timeout)
	}
}
