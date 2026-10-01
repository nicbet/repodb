package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestReportOmitsUnavailablePeakRSS(t *testing.T) {
	data, err := json.Marshal(report{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "process_peak_rss_bytes") {
		t.Fatalf("unavailable peak RSS serialized: %s", data)
	}
	peak := int64(42)
	data, err = json.Marshal(report{PeakRSS: &peak})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"process_peak_rss_bytes":42`) {
		t.Fatalf("measured peak RSS missing: %s", data)
	}
}

// Git's background auto-gc prunes loose objects while the harness measures
// fixture size; an entry listed before it vanished must count as zero.
func TestEntryBytesToleratesVanishedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loose-object")
	if err := os.WriteFile(path, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("read dir = %v, %v", entries, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if n, err := entryBytes(entries[0]); err != nil || n != 0 {
		t.Fatalf("entryBytes(vanished) = %d, %v; want 0, nil", n, err)
	}
	if n, err := directoryBytes(filepath.Join(dir, "missing-root")); err != nil || n != 0 {
		t.Fatalf("directoryBytes(missing root) = %d, %v; want 0, nil", n, err)
	}
}

// Fixture repositories, bare or not, never start Git housekeeping on their own.
func TestFixturesDisableAutomaticGitHousekeeping(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	if err := git(root, "init", "--bare", "--quiet", remote); err != nil {
		t.Fatal(err)
	}
	if err := configureFixture(remote); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(root, "local")
	if err := newRepo(local, remote); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{remote, local} {
		for key, want := range map[string]string{"gc.auto": "0", "maintenance.auto": "false", "receive.autogc": "false"} {
			out, err := exec.Command("git", "-C", repo, "config", "--get", key).Output()
			if got := strings.TrimSpace(string(out)); err != nil || got != want {
				t.Errorf("%s: %s = %q, %v; want %q", repo, key, got, err, want)
			}
		}
	}
}
