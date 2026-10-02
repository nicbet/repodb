package engine

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os/exec"
	"testing"

	"github.com/nicbet/repodb/common/prolly"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/common/storage"
)

// TestCheckpointIndexEditsMatchRebuild checks the incremental index
// checkpoint against a full rebuild. Random inserts, updates (including
// unique values moving between rows and NULLs) and deletes, sometimes of every
// row, run between checkpoints; after each one, every index root must equal
// the root rebuilt from the checkpointed data tree.
func TestCheckpointIndexEditsMatchRebuild(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if output, err := exec.Command("git", "-C", root, "init", "--quiet", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, err := OpenWithOptions(ctx, root, Options{Persistence: PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	exec := func(stmt string) error {
		session, err := eng.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		return session.Exec(ctx, stmt)
	}
	if err := exec("CREATE TABLE t (id BIGINT PRIMARY KEY, u BIGINT, n VARCHAR(20), c BIGINT, UNIQUE KEY u_idx (u), KEY n_idx (n), KEY nc_idx (n, c))"); err != nil {
		t.Fatal(err)
	}

	rng := rand.New(rand.NewPCG(3, 4))
	value := func() string {
		if rng.IntN(6) == 0 {
			return "NULL"
		}
		return fmt.Sprint(rng.IntN(50))
	}
	for round := 0; round < 40; round++ {
		for step := rng.IntN(25); step >= 0; step-- {
			id := rng.IntN(40)
			var stmt string
			switch op := rng.IntN(20); {
			case op < 8:
				stmt = fmt.Sprintf("INSERT INTO t VALUES (%d, %s, 'n%d', %s)", id, value(), rng.IntN(6), value())
			case op < 13:
				stmt = fmt.Sprintf("UPDATE t SET u = %s, n = 'n%d' WHERE id = %d", value(), rng.IntN(6), id)
			case op < 15:
				stmt = fmt.Sprintf("UPDATE t SET c = %s WHERE id = %d", value(), id)
			case op < 17:
				// Move a unique value from whichever row holds it to another.
				v := rng.IntN(50)
				stmt = fmt.Sprintf("UPDATE t SET u = NULL WHERE u = %d", v)
				if err := exec(stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
				stmt = fmt.Sprintf("UPDATE t SET u = %d WHERE id = %d", v, id)
			case op < 19:
				stmt = fmt.Sprintf("DELETE FROM t WHERE id = %d", id)
			default:
				if rng.IntN(4) == 0 {
					stmt = "DELETE FROM t"
				} else {
					stmt = fmt.Sprintf("DELETE FROM t WHERE id < %d", id)
				}
			}
			_ = exec(stmt) // duplicate keys are expected rejections
		}
		result, err := eng.Checkpoint(ctx, fmt.Sprint("round ", round))
		if err != nil {
			t.Fatalf("round %d checkpoint: %v", round, err)
		}
		if result.Snapshot == nil {
			continue
		}
		assertIndexesMatchRebuild(t, result.Snapshot, round)
	}
}

func assertIndexesMatchRebuild(t *testing.T, snapshot *repository.Snapshot, round int) {
	t.Helper()
	ctx := context.Background()
	table := snapshot.Manifest.Tables["t"]
	schemaData, err := snapshot.Store().Get(ctx, table.SchemaRoot)
	if err != nil {
		t.Fatal(err)
	}
	schema, _, indexes, err := decodeSchema(schemaData)
	if err != nil {
		t.Fatal(err)
	}
	dataTree, err := prolly.Open(snapshot.Store(), table.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	want, err := rebuildIndexesFromTree(ctx, storage.NewMemory(), schema, indexes, dataTree)
	if err != nil {
		t.Fatal(err)
	}
	if len(table.Indexes) != len(want) {
		t.Fatalf("round %d: index roots %v, rebuild %v", round, table.Indexes, want)
	}
	for name, root := range want {
		if table.Indexes[name] != root {
			t.Fatalf("round %d: index %s root %s, rebuild %s", round, name, table.Indexes[name], root)
		}
	}
}
