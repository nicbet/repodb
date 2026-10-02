package repository

import (
	"context"
	"errors"
	"os"
	"testing"
)

func put(key string) TypedRowEdit { return TypedRowEdit{Key: []byte(key), Value: []byte("v" + key)} }

func claim(index, key string) IndexClaim { return IndexClaim{Index: index, Key: []byte(key)} }

// rebaseFixture returns a journal holding tables t (row k0) and u, and the
// snapshot after that commit.
func rebaseFixture(t *testing.T) (*Repository, *WorkingState, *Snapshot) {
	t.Helper()
	ctx := context.Background()
	repo := durabilityTestRepo(t)
	w, err := OpenWorkingState(repo)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := w.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base, _, err := w.CommitTypedEdits(ctx, initial, []TypedTableEdit{
		{Table: "t", Schema: []byte("schema t"), Edits: []TypedRowEdit{put("k0")}},
		{Table: "u", Schema: []byte("schema u"), Edits: []TypedRowEdit{put("u0")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return repo, w, base
}

func pendingValue(t *testing.T, snapshot *Snapshot, table, key string) string {
	t.Helper()
	edit, ok := snapshot.PendingEdits()[table].Get([]byte(key))
	if !ok || edit.Delete {
		return ""
	}
	return string(edit.Value)
}

// Two transactions from one snapshot: the second commits when its writes are
// disjoint from the first's, and conflicts otherwise.
func TestTypedCommitConflictsOnlyOnOverlappingWrites(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		first, second []TypedTableEdit
		conflict      bool
	}{
		{"disjoint rows", []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k1")}}}, []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k2")}}}, false},
		{"same row", []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k0")}}}, []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k0")}}}, true},
		{"delete and update", []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{{Key: []byte("k0"), Delete: true}}}}, []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k0")}}}, true},
		{"same unique key, different rows", []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k1")}, Claims: []IndexClaim{claim("email", "a")}}}, []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k2")}, Claims: []IndexClaim{claim("email", "a")}}}, true},
		{"same key, different unique indexes", []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k1")}, Claims: []IndexClaim{claim("email", "a")}}}, []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k2")}, Claims: []IndexClaim{claim("login", "a")}}}, false},
		{"same unique key, different tables", []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k1")}, Claims: []IndexClaim{claim("email", "a")}}}, []TypedTableEdit{{Table: "u", Edits: []TypedRowEdit{put("u1")}, Claims: []IndexClaim{claim("email", "a")}}}, false},
		{"schema changed under a writer", []TypedTableEdit{{Table: "t", Schema: []byte("schema t2")}}, []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k2")}}}, true},
		{"schema change after a writer", []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k1")}}}, []TypedTableEdit{{Table: "t", Schema: []byte("schema t2")}}, true},
		{"dropped under a writer", []TypedTableEdit{{Table: "t", Drop: true}}, []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k2")}}}, true},
		{"drop after a writer", []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k1")}}}, []TypedTableEdit{{Table: "t", Drop: true}}, true},
		{"both create a table", []TypedTableEdit{{Table: "v", Schema: []byte("schema v")}}, []TypedTableEdit{{Table: "v", Schema: []byte("schema v")}}, true},
		{"create another table", []TypedTableEdit{{Table: "v", Schema: []byte("schema v")}}, []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k2")}}}, false},
		{"drop another table", []TypedTableEdit{{Table: "u", Drop: true}}, []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k2")}}}, false},
		{"alter another table", []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k1")}}}, []TypedTableEdit{{Table: "u", Schema: []byte("schema u2")}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, w, base := rebaseFixture(t)
			if _, _, err := w.CommitTypedEdits(ctx, base, tc.first); err != nil {
				t.Fatal(err)
			}
			_, _, err := w.CommitTypedEdits(ctx, base, tc.second)
			if tc.conflict != errors.Is(err, ErrConflict) || (!tc.conflict && err != nil) {
				t.Fatalf("second commit = %v, want conflict %v", err, tc.conflict)
			}
		})
	}
}

// A rebased commit lands on top of every commit since its base, and a fresh
// process (a full replay, which rebuilds the write log) sees the same state
// and keeps detecting conflicts against it.
func TestTypedCommitRebasesAcrossGenerationsAndProcesses(t *testing.T) {
	ctx := context.Background()
	repo, w, base := rebaseFixture(t)
	other, err := OpenWorkingState(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"k1", "k2", "k3"} {
		if _, _, err := other.CommitTypedEdits(ctx, mustCurrent(t, other), []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put(key)}, Claims: []IndexClaim{claim("email", key)}}}); err != nil {
			t.Fatal(err)
		}
	}
	// w learns of other's commits by replaying them incrementally.
	if _, _, err := w.CommitTypedEdits(ctx, base, []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k2")}}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("row written two generations later = %v, want conflict", err)
	}
	if _, _, err := w.CommitTypedEdits(ctx, base, []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k9")}, Claims: []IndexClaim{claim("email", "k3")}}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("unique key claimed later = %v, want conflict", err)
	}
	rebased, _, err := w.CommitTypedEdits(ctx, base, []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k4")}}})
	if err != nil {
		t.Fatal(err)
	}
	if rebased.Generation() != base.Generation()+4 {
		t.Fatalf("rebased generation = %d, want %d", rebased.Generation(), base.Generation()+4)
	}
	fresh, err := OpenWorkingState(repo)
	if err != nil {
		t.Fatal(err)
	}
	replayed := mustCurrent(t, fresh)
	for _, key := range []string{"k0", "k1", "k2", "k3", "k4"} {
		if pendingValue(t, replayed, "t", key) != "v"+key {
			t.Fatalf("replayed row %s missing", key)
		}
	}
	if _, _, err := fresh.CommitTypedEdits(ctx, base, []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k4")}}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("after full replay, row written by a rebased commit = %v, want conflict", err)
	}
	if _, _, err := fresh.CommitTypedEdits(ctx, base, []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k5")}}}); err != nil {
		t.Fatalf("after full replay, disjoint commit = %v", err)
	}
}

// A base older than the write log (a checkpoint or whole-manifest commit came
// in between) always conflicts.
func TestWriteConflictRequiresCoverage(t *testing.T) {
	edits := []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k")}}}
	for _, tc := range []struct {
		name string
		view workingView
		base uint64
	}{
		{"no log", workingView{generation: 5}, 4},
		{"base below log", workingView{generation: 5, writes: (*writeLog)(nil).recorded(nil, 4)}, 2},
		{"base ahead of view", workingView{generation: 5, writes: (*writeLog)(nil).recorded(nil, 1)}, 6},
	} {
		if err := tc.view.writeConflict(tc.base, edits); !errors.Is(err, ErrConflict) {
			t.Errorf("%s: writeConflict = %v, want conflict", tc.name, err)
		}
	}
	covered := workingView{generation: 5, writes: (*writeLog)(nil).recorded(nil, 3)}
	if err := covered.writeConflict(2, edits); err != nil {
		t.Errorf("covered base: writeConflict = %v", err)
	}
}

// A rebased commit is recovered like any other journal commit.
func TestRebasedCommitFaultsRecover(t *testing.T) {
	ctx := context.Background()
	stale := func(t *testing.T) (*Repository, *WorkingState, *Snapshot) {
		repo, w, base := rebaseFixture(t)
		if _, _, err := w.CommitTypedEdits(ctx, base, []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k1")}}}); err != nil {
			t.Fatal(err)
		}
		return repo, w, base
	}
	rebase := []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k2")}}}

	t.Run("before append", func(t *testing.T) {
		repo, w, base := stale(t)
		w.SetFaultInjector(func(point WorkingFaultPoint) error {
			if point == BeforeJournalAppend {
				return errors.New("injected")
			}
			return nil
		})
		_, txid, err := w.CommitTypedEdits(ctx, base, rebase)
		var commitErr *WorkingCommitError
		if !errors.As(err, &commitErr) || commitErr.Outcome != OutcomeRejected {
			t.Fatalf("commit = %v, want rejected", err)
		}
		fresh, _ := OpenWorkingState(repo)
		if result, err := fresh.RecoverTransaction(ctx, txid); err != nil || result.Outcome != OutcomeRejected {
			t.Fatalf("recover = %+v, %v", result, err)
		}
		if pendingValue(t, mustCurrent(t, fresh), "t", "k2") != "" {
			t.Fatal("rejected rebased row is visible")
		}
	})

	t.Run("after flush", func(t *testing.T) {
		repo, w, base := stale(t)
		w.SetFaultInjector(func(point WorkingFaultPoint) error {
			if point == AfterJournalFlush {
				return errors.New("injected")
			}
			return nil
		})
		_, txid, err := w.CommitTypedEdits(ctx, base, rebase)
		var commitErr *WorkingCommitError
		if !errors.As(err, &commitErr) || commitErr.Outcome != OutcomeCommitted {
			t.Fatalf("commit = %v, want committed", err)
		}
		fresh, _ := OpenWorkingState(repo)
		if result, err := fresh.RecoverTransaction(ctx, txid); err != nil || result.Outcome != OutcomeCommitted {
			t.Fatalf("recover = %+v, %v", result, err)
		}
		current := mustCurrent(t, fresh)
		if pendingValue(t, current, "t", "k1") != "vk1" || pendingValue(t, current, "t", "k2") != "vk2" {
			t.Fatal("recovered journal lacks the rebased commit or the one before it")
		}
	})

	t.Run("torn tail", func(t *testing.T) {
		repo, w, base := stale(t)
		if _, _, err := w.CommitTypedEdits(ctx, base, rebase); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(w.JournalPath(), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte("RDBJ\x00\x00")); err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
		fresh, _ := OpenWorkingState(repo)
		if pendingValue(t, mustCurrent(t, fresh), "t", "k2") != "vk2" {
			t.Fatal("rebased row lost behind a torn tail")
		}
		if _, _, err := fresh.CommitTypedEdits(ctx, base, []TypedTableEdit{{Table: "t", Edits: []TypedRowEdit{put("k3")}}}); err != nil {
			t.Fatalf("rebased commit after a torn tail = %v", err)
		}
	})
}

func mustCurrent(t *testing.T, w *WorkingState) *Snapshot {
	t.Helper()
	snapshot, err := w.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
