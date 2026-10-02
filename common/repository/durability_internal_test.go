package repository

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

type flushRecord struct {
	level Durability
	head  string // the data head when the flush ran
}

// recordFlushes replaces flushFile with a recorder that still flushes for
// real, and restores it when the test ends.
func recordFlushes(t *testing.T, repo *Repository) *[]flushRecord {
	t.Helper()
	var records []flushRecord
	previous := flushFile
	flushFile = func(file *os.File, level Durability) error {
		head, _ := repo.Head(context.Background())
		records = append(records, flushRecord{level: level, head: head})
		return previous(file, level)
	}
	t.Cleanup(func() { flushFile = previous })
	return &records
}

func durabilityTestRepo(t *testing.T) *Repository {
	t.Helper()
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "--quiet", "-b", "main", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	repo, err := Init(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

// commitToJournal appends one manifest-based transaction to the journal.
func commitToJournal(t *testing.T, w *WorkingState, value string) {
	t.Helper()
	ctx := context.Background()
	base, err := w.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := w.repo.BeginSnapshot(base)
	if err != nil {
		t.Fatal(err)
	}
	root, err := writer.Put(ctx, []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.Commit(ctx, writer, Manifest{Tables: map[string]Table{"t": {DataRoot: root}}}); err != nil {
		t.Fatal(err)
	}
}

func TestJournalCommitFlushesAtConfiguredDurability(t *testing.T) {
	for _, tc := range []struct {
		level Durability
		want  []Durability
	}{
		{DurabilityFull, []Durability{DurabilityFull}},
		{DurabilityNormal, []Durability{DurabilityNormal}},
		{"", []Durability{DurabilityNormal}}, // zero value is normal
		{DurabilityOff, nil},
	} {
		t.Run(fmt.Sprintf("%q", tc.level), func(t *testing.T) {
			repo := durabilityTestRepo(t)
			w, err := OpenWorkingStateWithOptions(repo, WorkingOptions{Durability: tc.level})
			if err != nil {
				t.Fatal(err)
			}
			records := recordFlushes(t, repo)
			commitToJournal(t, w, "row")
			var got []Durability
			for _, r := range *records {
				got = append(got, r.level)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("flushes = %v, want %v", got, tc.want)
			}
		})
	}
}

// Whatever the commit durability, a checkpoint forces the journal to stable
// storage before the data head moves: the published Git commit contains the
// journal's transactions and must not outlive them in a power loss.
func TestCheckpointFlushesJournalFullyBeforePublishing(t *testing.T) {
	for _, level := range []Durability{DurabilityNormal, DurabilityOff} {
		t.Run(string(level), func(t *testing.T) {
			ctx := context.Background()
			repo := durabilityTestRepo(t)
			w, err := OpenWorkingStateWithOptions(repo, WorkingOptions{Durability: level})
			if err != nil {
				t.Fatal(err)
			}
			commitToJournal(t, w, "row")
			before, _ := repo.Head(ctx)
			records := recordFlushes(t, repo)
			result, err := w.Checkpoint(ctx, "checkpoint")
			if err != nil {
				t.Fatal(err)
			}
			if result.Commit == before {
				t.Fatal("checkpoint did not publish")
			}
			if len(*records) == 0 {
				t.Fatal("checkpoint flushed nothing")
			}
			first := (*records)[0]
			if first.level != DurabilityFull || first.head != before {
				t.Fatalf("first checkpoint flush = %+v, want a full flush while the head is still %s", first, before)
			}
		})
	}
}

// Every level's real flush works on this platform (F_BARRIERFSYNC on macOS,
// fdatasync on Linux, FlushFileBuffers on Windows).
func TestFlushPrimitivesSucceed(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "journal"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, level := range []Durability{DurabilityFull, DurabilityNormal, DurabilityOff} {
		if _, err := file.Write([]byte("record")); err != nil {
			t.Fatal(err)
		}
		if err := flushFile(file, level); err != nil {
			t.Errorf("flush %s: %v", level, err)
		}
	}
}

func TestParseDurability(t *testing.T) {
	for in, want := range map[string]Durability{"": DurabilityNormal, "normal": DurabilityNormal, "full": DurabilityFull, "off": DurabilityOff} {
		if got, err := ParseDurability(in); err != nil || got != want {
			t.Errorf("ParseDurability(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseDurability("fast"); err == nil {
		t.Error("ParseDurability(\"fast\") succeeded")
	}
	if _, err := OpenWorkingStateWithOptions(&Repository{}, WorkingOptions{Durability: "fast"}); err == nil {
		t.Error("OpenWorkingStateWithOptions accepted an unknown durability")
	}
}
