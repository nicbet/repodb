package main

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
	"github.com/nicbet/repodb/integration"
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

func TestStartPersistenceDefaultsToJournal(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want engine.PersistenceMode
	}{
		{nil, engine.PersistenceJournal},
		{[]string{"-persistence", "native-git"}, engine.PersistenceNativeGit},
	} {
		opts, err := parseStartOptions(tc.args)
		if err != nil || opts.persistence != tc.want {
			t.Errorf("parseStartOptions(%v) = %v, %v; want %s", tc.args, opts.persistence, err, tc.want)
		}
	}
}

// enabledRepoWithDirtyJournal returns an enabled repository whose journal has
// an uncheckpointed row, and its bare remote.
func enabledRepoWithDirtyJournal(t *testing.T) (root, remote string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	root, remote = filepath.Join(dir, "local"), filepath.Join(dir, "remote.git")
	for _, args := range [][]string{
		{"init", "--quiet", "--bare", remote},
		{"init", "--quiet", "-b", "main", root},
		{"-C", root, "remote", "add", "origin", remote},
	} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if err := run(ctx, []string{"enable", "-remote", "origin", "-repo", root}); err != nil {
		t.Fatal(err)
	}
	eng, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	session, _ := eng.NewSession()
	for _, stmt := range []string{"CREATE TABLE items (id BIGINT PRIMARY KEY)", "INSERT INTO items VALUES (1)"} {
		if err := session.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	_ = session.Close()
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	return root, remote
}

func TestSyncCommitFlagCheckpointsAndSyncs(t *testing.T) {
	ctx := context.Background()
	root, remote := enabledRepoWithDirtyJournal(t)
	if err := run(ctx, []string{"sync", "-commit", "-m", "from sync", "-repo", root}); err != nil {
		t.Fatalf("sync --commit: %v", err)
	}
	local := strings.TrimSpace(gitOutput(t, root, "rev-parse", repository.DataRef))
	pushed := strings.TrimSpace(gitOutput(t, remote, "rev-parse", repository.DataRef))
	if local != pushed {
		t.Fatalf("remote data ref = %s, local = %s", pushed, local)
	}
	if subject := strings.TrimSpace(gitOutput(t, root, "log", "-1", "--format=%s", repository.DataRef)); subject != "from sync" {
		t.Fatalf("data commit subject = %q", subject)
	}
}

func TestSyncWithoutCommitFlagRefusesDirtyJournal(t *testing.T) {
	ctx := context.Background()
	root, _ := enabledRepoWithDirtyJournal(t)
	// Tests don't run on a terminal, so there is no prompt.
	err := run(ctx, []string{"sync", "-repo", root})
	if !errors.Is(err, integration.ErrWorkingDirty) || !strings.Contains(err.Error(), "sync --commit -m") {
		t.Fatalf("sync = %v, want ErrWorkingDirty naming --commit -m", err)
	}
	if err := run(ctx, []string{"sync", "-commit", "-repo", root}); err == nil || !strings.Contains(err.Error(), "requires -m") {
		t.Fatalf("sync --commit without -m = %v", err)
	}
	if err := run(ctx, []string{"sync", "-m", "x", "-repo", root}); err == nil || !strings.Contains(err.Error(), "requires --commit") {
		t.Fatalf("sync -m without --commit = %v", err)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}

func TestStartDurabilityFlag(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want repository.Durability
	}{
		{nil, repository.DurabilityNormal},
		{[]string{"-durability", "full"}, repository.DurabilityFull},
		{[]string{"-durability", "off"}, repository.DurabilityOff},
	} {
		opts, err := parseStartOptions(tc.args)
		if err != nil || opts.durability != tc.want {
			t.Errorf("parseStartOptions(%v) = %q, %v; want %q", tc.args, opts.durability, err, tc.want)
		}
	}
	if _, err := parseStartOptions([]string{"-durability", "fast"}); err == nil {
		t.Error("parseStartOptions accepted -durability fast")
	}
}
