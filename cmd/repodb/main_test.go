package main

import (
	"context"
	"os/exec"
	"testing"

	"github.com/nicbet/repodb/engine"
)

func TestCommitKeepsJournalRows(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if output, err := exec.Command("git", "-C", root, "init", "--quiet", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if err := run(ctx, []string{"init", root}); err != nil {
		t.Fatal(err)
	}

	eng, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	session, _ := eng.NewSession()
	for _, stmt := range []string{
		"CREATE TABLE items (id BIGINT PRIMARY KEY, name TEXT)",
		"INSERT INTO items VALUES (1, 'a'), (2, 'b')",
	} {
		if err := session.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	_ = session.Close()
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}

	if err := run(ctx, []string{"commit", "-m", "checkpoint", "-repo", root}); err != nil {
		t.Fatal(err)
	}

	reopened, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	status, err := reopened.WorkingState().Status(ctx)
	if err != nil || status.Dirty {
		t.Fatalf("status after commit = %#v, %v", status, err)
	}
	reader, _ := reopened.NewSession()
	defer reader.Close()
	result, err := reader.Query(ctx, "SELECT id, name FROM items ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 || result.Rows[0][1] != "a" || result.Rows[1][1] != "b" {
		t.Fatalf("rows after commit = %#v", result.Rows)
	}
}
