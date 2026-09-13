package main

import (
	"errors"
	"testing"

	"github.com/nicbet/repodb/common/repository"
)

func TestReportCountsRejectedAttemptsSeparately(t *testing.T) {
	r := &report{}
	err := r.measure(t.TempDir(), 100, "mixed", 2, 3, true, func(_, i int) error {
		if i == 1 {
			return repository.ErrConflict
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	m := r.Results[0]
	if m.Success != 4 || m.Conflicts != 2 || len(m.Durations) != 4 || len(m.Rejected) != 2 || len(m.Errors) != 0 {
		t.Fatalf("incorrect accounting: %+v", m)
	}
}

func TestFailureRetainsPartialReport(t *testing.T) {
	r := &report{}
	err := r.measure(t.TempDir(), 100, "broken", 1, 5, false, func(_, i int) error {
		if i == 1 {
			return errors.New("failed verification")
		}
		return nil
	})
	if err == nil || len(r.Results) != 1 {
		t.Fatalf("missing failure/report: %v %+v", err, r)
	}
	m := r.Results[0]
	if m.Success != 1 || len(m.Errors) != 1 {
		t.Fatalf("incorrect failure accounting: %+v", m)
	}
}

func TestPercentilesUseIndividualRequests(t *testing.T) {
	values := make([]float64, 100)
	for i := range values {
		values[i] = float64(i + 1)
	}
	if percentile(values, .5) != 50 || percentile(values, .95) != 95 || percentile(values, .99) != 99 || percentile(nil, .5) != 0 {
		t.Fatal("incorrect nearest-rank percentiles")
	}
}

func TestRejectInvalidMatrix(t *testing.T) {
	for _, s := range []string{"", "0", "-1", "1,no", "1,1"} {
		if integers(s) != nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
