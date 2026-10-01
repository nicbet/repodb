package engine_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

type journalEntry struct {
	Kind              string   `json:"kind"`
	TxID              string   `json:"tx_id"`
	Generation        uint64   `json:"generation"`
	CheckpointedTxIDs []string `json:"checkpointed_tx_ids"`
}

// journalEntries parses the journal's complete frames.
func journalEntries(t *testing.T, path string) []journalEntry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entries []journalEntry
	for len(data) >= 12 {
		length := int(binary.BigEndian.Uint32(data[4:8]))
		if len(data) < 12+length {
			break
		}
		var entry journalEntry
		if err := json.Unmarshal(data[12:12+length], &entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
		data = data[12+length:]
	}
	return entries
}

func lastCommittedTxID(t *testing.T, path string) string {
	t.Helper()
	entries := journalEntries(t, path)
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Kind == "commit" {
			return entries[i].TxID
		}
	}
	t.Fatal("no committed transaction in the journal")
	return ""
}

func openJournalEngine(t *testing.T, ctx context.Context, root string) (*engine.Engine, *engine.Session) {
	t.Helper()
	eng, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	s, err := eng.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return eng, s
}

func countRows(t *testing.T, ctx context.Context, s *engine.Session, table string) int64 {
	t.Helper()
	result, err := s.Query(ctx, "SELECT COUNT(*) FROM "+table)
	if err != nil {
		t.Fatal(err)
	}
	return result.Rows[0][0].(int64)
}

func initJournalRepo(t *testing.T, ctx context.Context) string {
	t.Helper()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	return root
}

// Each checkpoint rewrites the journal to a single anchor frame, so its size
// tracks the work since the last checkpoint, not the repository's age.
func TestJournalStaysBoundedAcrossCheckpoints(t *testing.T) {
	ctx := context.Background()
	root := initJournalRepo(t, ctx)
	eng, s := openJournalEngine(t, ctx, root)
	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, v TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	path := eng.WorkingState().JournalPath()
	const rounds, perRound = 20, 50
	var sizes []int64
	for round := 0; round < rounds; round++ {
		for i := 0; i < perRound; i++ {
			if err := s.Exec(ctx, "INSERT INTO t VALUES (?, 'row')", int64(round*perRound+i)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := eng.Checkpoint(ctx, fmt.Sprintf("round %d", round)); err != nil {
			t.Fatal(err)
		}
		entries := journalEntries(t, path)
		if len(entries) != 1 || entries[0].Kind != "checkpoint" {
			t.Fatalf("round %d: journal has %d frames, want one checkpoint anchor", round, len(entries))
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, info.Size())
	}
	// Every anchor lists one round's transactions, so sizes stay flat.
	if sizes[rounds-1] > sizes[1]*11/10 {
		t.Fatalf("journal grew with checkpoints: %v", sizes)
	}
	if got := eng.WorkingState().Metrics().JournalCompactions; got != rounds {
		t.Fatalf("JournalCompactions = %d, want %d", got, rounds)
	}

	reopened, rs := openJournalEngine(t, ctx, root)
	if got := countRows(t, ctx, rs, "t"); got != rounds*perRound {
		t.Fatalf("rows after reopen = %d", got)
	}
	status, err := reopened.WorkingState().Status(ctx)
	if err != nil || status.Dirty || status.Generation != rounds*perRound+1 {
		t.Fatalf("status after reopen = %#v, %v", status, err)
	}
	// Writing after replaying a compacted journal continues the generations.
	if err := rs.Exec(ctx, "INSERT INTO t VALUES (-1, 'after')"); err != nil {
		t.Fatal(err)
	}
	if status, _ := reopened.WorkingState().Status(ctx); status.Generation != rounds*perRound+2 {
		t.Fatalf("generation after write = %d", status.Generation)
	}
}

// A second engine keeps working across the other engine's compactions: it
// notices the replaced journal, replays the anchor, and its generations stay
// in step, so no false conflicts.
func TestSecondEngineFollowsJournalCompaction(t *testing.T) {
	ctx := context.Background()
	root := initJournalRepo(t, ctx)
	a, sa := openJournalEngine(t, ctx, root)
	if err := sa.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	b, sb := openJournalEngine(t, ctx, root)
	id := int64(0)
	write := func(s *engine.Session) {
		t.Helper()
		id++
		if err := s.Exec(ctx, "INSERT INTO t VALUES (?)", id); err != nil {
			t.Fatalf("insert %d: %v", id, err)
		}
	}
	for round := 0; round < 5; round++ {
		write(sa)
		write(sb)
		if _, err := a.Checkpoint(ctx, "a"); err != nil {
			t.Fatal(err)
		}
		write(sb)
		write(sa)
		if _, err := b.Checkpoint(ctx, "b"); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []*engine.Session{sa, sb} {
		if got := countRows(t, ctx, s, "t"); got != id {
			t.Fatalf("rows = %d, want %d", got, id)
		}
	}
	statusA, errA := a.WorkingState().Status(ctx)
	statusB, errB := b.WorkingState().Status(ctx)
	if errA != nil || errB != nil || statusA != statusB || statusA.Generation != uint64(id)+1 {
		t.Fatalf("statuses = %#v, %#v (%v, %v)", statusA, statusB, errA, errB)
	}
}

// A cache whose inode matches but whose file is a different journal (the
// inode number was reused after compaction) must replay from the start.
func TestJournalCacheDetectsReusedInode(t *testing.T) {
	ctx := context.Background()
	root := initJournalRepo(t, ctx)
	a, sa := openJournalEngine(t, ctx, root)
	if err := sa.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 3; i++ {
		if err := sa.Exec(ctx, "INSERT INTO t VALUES (?)", i); err != nil {
			t.Fatal(err)
		}
	}
	path := a.WorkingState().JournalPath()
	if _, err := a.WorkingState().Status(ctx); err != nil { // cache at the end of this file
		t.Fatal(err)
	}
	// Keep the current inode alive under another name.
	oldInode := filepath.Join(filepath.Dir(path), "journal.old-inode")
	if err := os.Link(path, oldInode); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	// Another engine checkpoints (compacting into a new file) and writes
	// until the new journal is longer than a's cached offset.
	cachedSize, _ := os.Stat(path)
	b, sb := openJournalEngine(t, ctx, root)
	if _, err := b.Checkpoint(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	for i := int64(100); ; i++ {
		if err := sb.Exec(ctx, "INSERT INTO t VALUES (?)", i); err != nil {
			t.Fatal(err)
		}
		if info, _ := os.Stat(path); info.Size() > cachedSize.Size()+64 {
			break
		}
	}
	// Put the new journal's bytes into the old inode and move it back into
	// place: same inode as a's cache, different, longer file.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldInode, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(oldInode, path); err != nil {
		t.Fatal(err)
	}
	if got, want := countRows(t, ctx, sa, "t"), countRows(t, ctx, sb, "t"); got != want {
		t.Fatalf("engine a sees %d rows, b sees %d", got, want)
	}
	statusA, errA := a.WorkingState().Status(ctx)
	statusB, errB := b.WorkingState().Status(ctx)
	if errA != nil || errB != nil || statusA != statusB {
		t.Fatalf("statuses = %#v, %#v (%v, %v)", statusA, statusB, errA, errB)
	}
}

// A failed compaction never fails the published checkpoint: the old journal
// stays valid, and the next checkpoint compacts it.
func TestJournalCompactionFailureKeepsCheckpoint(t *testing.T) {
	for _, point := range []repository.WorkingFaultPoint{repository.BeforeJournalCompactionRename, repository.AfterJournalCompactionRename} {
		t.Run(string(point), func(t *testing.T) {
			ctx := context.Background()
			root := initJournalRepo(t, ctx)
			eng, s := openJournalEngine(t, ctx, root)
			working := eng.WorkingState()
			path := working.JournalPath()
			if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY)"); err != nil {
				t.Fatal(err)
			}
			if err := s.Exec(ctx, "INSERT INTO t VALUES (1)"); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected compaction fault")
			working.SetFaultInjector(func(p repository.WorkingFaultPoint) error {
				if p == point {
					return injected
				}
				return nil
			})
			if _, err := eng.Checkpoint(ctx, "faulted compaction"); err != nil {
				t.Fatalf("checkpoint failed because of compaction: %v", err)
			}
			working.SetFaultInjector(nil)
			if got := working.Metrics().JournalCompactionFailures; got != 1 {
				t.Fatalf("JournalCompactionFailures = %d", got)
			}
			if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temporary journal left behind: %v", err)
			}
			frames := len(journalEntries(t, path))
			if point == repository.BeforeJournalCompactionRename && frames < 3 {
				t.Fatalf("old journal replaced despite the fault: %d frames", frames)
			}
			if point == repository.AfterJournalCompactionRename && frames != 1 {
				t.Fatalf("renamed journal has %d frames", frames)
			}
			// The journal on disk is loadable and complete.
			_, fresh := openJournalEngine(t, ctx, root)
			if got := countRows(t, ctx, fresh, "t"); got != 1 {
				t.Fatalf("rows after faulted compaction = %d", got)
			}
			if err := s.Exec(ctx, "INSERT INTO t VALUES (2)"); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Checkpoint(ctx, "next"); err != nil {
				t.Fatal(err)
			}
			if frames := len(journalEntries(t, path)); frames != 1 {
				t.Fatalf("next checkpoint left %d frames", frames)
			}
		})
	}
}

// RecoverTransaction never reports a committed transaction as rejected after
// compaction: it answers from the anchor for the previous checkpoint interval
// and says unknown beyond it.
func TestRecoverTransactionAfterCompaction(t *testing.T) {
	ctx := context.Background()
	root := initJournalRepo(t, ctx)
	eng, s := openJournalEngine(t, ctx, root)
	working := eng.WorkingState()
	path := working.JournalPath()
	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	insert := func(id int64) string {
		t.Helper()
		if err := s.Exec(ctx, "INSERT INTO t VALUES (?)", id); err != nil {
			t.Fatal(err)
		}
		return lastCommittedTxID(t, path)
	}
	checkpoint := func() {
		t.Helper()
		if _, err := eng.Checkpoint(ctx, "checkpoint"); err != nil {
			t.Fatal(err)
		}
	}
	recover := func(id string) (repository.CommitOutcome, error) {
		t.Helper()
		result, err := working.RecoverTransaction(ctx, id)
		return result.Outcome, err
	}

	a := insert(1)
	if outcome, err := recover(a); outcome != repository.OutcomeCommitted || err != nil {
		t.Fatalf("a before compaction = %s, %v", outcome, err)
	}
	checkpoint()
	if outcome, err := recover(a); outcome != repository.OutcomeCommitted || err != nil {
		t.Fatalf("a after one compaction = %s, %v", outcome, err)
	}
	b := insert(2)
	checkpoint()
	c := insert(3)
	if outcome, err := recover(b); outcome != repository.OutcomeCommitted || err != nil {
		t.Fatalf("b, previous interval = %s, %v", outcome, err)
	}
	if outcome, err := recover(c); outcome != repository.OutcomeCommitted || err != nil {
		t.Fatalf("c, current interval = %s, %v", outcome, err)
	}
	if outcome, err := recover(a); outcome != repository.OutcomeUnknown || !errors.Is(err, repository.ErrWorkingHistoryTruncated) {
		t.Fatalf("a, two intervals back = %s, %v; want unknown", outcome, err)
	}
	status, _ := working.Status(ctx)
	never := fmt.Sprintf("%d-%032x", status.Generation+1, 0)
	if outcome, err := recover(never); outcome != repository.OutcomeRejected || err != nil {
		t.Fatalf("never written, current generation = %s, %v; want rejected", outcome, err)
	}
	if outcome, err := recover("1-" + fmt.Sprintf("%032x", 0)); outcome != repository.OutcomeUnknown || !errors.Is(err, repository.ErrWorkingHistoryTruncated) {
		t.Fatalf("unknown ID from truncated history = %s, %v; want unknown", outcome, err)
	}
	if outcome, err := recover("not-a-generation"); outcome != repository.OutcomeUnknown || !errors.Is(err, repository.ErrWorkingHistoryTruncated) {
		t.Fatalf("unparseable ID = %s, %v; want unknown", outcome, err)
	}
}
