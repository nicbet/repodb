package repository

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A transaction prepared but never committed is rejected even when the
// journal has been compacted, and IDs newer than the anchor's history that the
// journal doesn't contain are rejected, not unknown.
func TestRecoverTransactionPreparedWithoutCommitAfterCompaction(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "--quiet", "-b", "main", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	repo, err := Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	w, err := OpenWorkingState(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(w.JournalPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	const prepared = "7-00000000000000000000000000000007"
	var journal []byte
	for _, record := range []journalRecord{
		{Version: workingFormatVersion, Kind: "checkpoint", Generation: 6, GitCommit: "c", CheckpointedTxIDs: []string{"6-00000000000000000000000000000006"}, FromGeneration: 4},
		{Version: workingFormatVersion, Kind: "typed-prepare", TxID: prepared, Generation: 7, TypedEdits: []TypedTableEdit{{Table: "t"}}},
	} {
		frame, err := encodeFrame(record)
		if err != nil {
			t.Fatal(err)
		}
		journal = append(journal, frame...)
	}
	if err := os.WriteFile(w.JournalPath(), journal, 0o600); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]CommitOutcome{
		prepared:                             OutcomeRejected,  // prepared, no commit
		"6-00000000000000000000000000000006": OutcomeCommitted, // listed in the anchor
		"5-00000000000000000000000000000005": OutcomeRejected,  // within the anchor's history, not listed
		"8-00000000000000000000000000000008": OutcomeRejected,  // current interval, never written
		"4-00000000000000000000000000000004": OutcomeUnknown,   // older than the anchor's history
	} {
		result, err := w.RecoverTransaction(ctx, id)
		if result.Outcome != want || (want == OutcomeUnknown) != (err != nil) {
			t.Errorf("%s: outcome %s, err %v; want %s", id, result.Outcome, err, want)
		}
	}
}
