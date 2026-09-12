package integration_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
	"github.com/nicbet/repodb/integration"
)

func TestEnableSyncFastForwardMergeConflictAndResolution(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	git(t, root, "init", "--bare", remote)
	git(t, root, "init", "--quiet", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", "README.md")
	git(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "source")
	git(t, seed, "remote", "add", "origin", remote)
	git(t, seed, "push", "origin", "main")
	git(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	git(t, root, "clone", "--quiet", remote, a)
	git(t, root, "clone", "--quiet", remote, b)

	if _, err := integration.Enable(ctx, a, ""); !errors.Is(err, integration.ErrRemoteRequired) {
		t.Fatalf("missing remote error = %v", err)
	}
	firstEnable, err := integration.Enable(ctx, a, "origin")
	if err != nil {
		t.Fatal(err)
	}
	if firstEnable.Action != "initialized-empty" {
		t.Fatalf("enable action = %s", firstEnable.Action)
	}
	if _, err := integration.Enable(ctx, a, "origin"); err != nil {
		t.Fatal(err)
	}
	spec := "+refs/repodb/data:refs/repodb/remotes/origin/data"
	if got := git(t, a, "config", "--get-all", "remote.origin.fetch"); strings.Count(got, spec) != 1 {
		t.Fatalf("fetch specs = %q", got)
	}
	if got := gitAllowFailure(a, "config", "--get-all", "remote.origin.push"); got != "" {
		t.Fatalf("enable added push configuration: %q", got)
	}
	if _, err := os.Stat(filepath.Join(a, ".git", "hooks", "pre-push")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("enable installed a hook: %v", err)
	}

	engA, err := engine.Open(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer engA.Close()
	sessionA, _ := engA.NewSession()
	if err := sessionA.Exec(ctx, "CREATE TABLE issues (id BIGINT PRIMARY KEY, title TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if err := sessionA.Exec(ctx, "INSERT INTO issues VALUES (1, 'initial')"); err != nil {
		t.Fatal(err)
	}
	if status, err := integration.Sync(ctx, a, "origin"); err != nil || status.Action != "pushed" {
		t.Fatalf("initial sync = %#v, %v", status, err)
	}

	if _, err := integration.Sync(ctx, b, "origin"); !errors.Is(err, integration.ErrNotEnabled) {
		t.Fatalf("sync before enable error = %v", err)
	}
	enableB, err := integration.Enable(ctx, b, "origin")
	if err != nil {
		t.Fatal(err)
	}
	if enableB.Action != "adopted-remote" {
		t.Fatalf("clone enable action = %s", enableB.Action)
	}
	engB, err := engine.Open(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	defer engB.Close()
	pinnedSession, _ := engB.NewSession()
	pinned, err := pinnedSession.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old, err := pinned.Query(ctx, "SELECT title FROM issues WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if old.Rows[0][0] != "initial" {
		t.Fatalf("old row = %#v", old.Rows)
	}

	if err := sessionA.Exec(ctx, "UPDATE issues SET title = 'remote-new' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := integration.Sync(ctx, a, "origin"); err != nil {
		t.Fatal(err)
	}
	if status, err := integration.Sync(ctx, b, "origin"); err != nil || status.Action != "fast-forwarded-local" {
		t.Fatalf("incoming sync = %#v, %v", status, err)
	}
	stillOld, err := pinned.Query(ctx, "SELECT title FROM issues WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if stillOld.Rows[0][0] != "initial" {
		t.Fatalf("pinned transaction changed: %#v", stillOld.Rows)
	}
	if err := pinned.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	fresh, _ := engB.NewSession()
	newRow, err := fresh.Query(ctx, "SELECT title FROM issues WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if newRow.Rows[0][0] != "remote-new" {
		t.Fatalf("fresh row = %#v", newRow.Rows)
	}
	if status, err := integration.Sync(ctx, b, "origin"); err != nil || status.Action != "up-to-date" {
		t.Fatalf("idempotent sync = %#v, %v", status, err)
	}

	if err := fresh.Exec(ctx, "INSERT INTO issues VALUES (2, 'local-b')"); err != nil {
		t.Fatal(err)
	}
	if err := sessionA.Exec(ctx, "INSERT INTO issues VALUES (3, 'remote-a')"); err != nil {
		t.Fatal(err)
	}
	if _, err := integration.Sync(ctx, a, "origin"); err != nil {
		t.Fatal(err)
	}
	status, err := integration.Sync(ctx, b, "origin")
	if err != nil || status.Action != "merged" {
		t.Fatalf("disjoint merge = %#v, %v", status, err)
	}
	if parents := strings.Fields(git(t, b, "rev-list", "--parents", "-n", "1", status.LocalHead)); len(parents) != 3 {
		t.Fatalf("merge commit parents = %v", parents)
	}
	merged, err := fresh.Query(ctx, "SELECT id, title FROM issues ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Rows) != 3 || merged.Rows[1][1] != "local-b" || merged.Rows[2][1] != "remote-a" {
		t.Fatalf("merged rows = %#v", merged.Rows)
	}
	if status, err := integration.Sync(ctx, a, "origin"); err != nil || status.Action != "fast-forwarded-local" {
		t.Fatalf("converge first clone = %#v, %v", status, err)
	}

	if err := sessionA.Exec(ctx, "UPDATE issues SET title = 'choice-a' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if err := fresh.Exec(ctx, "UPDATE issues SET title = 'choice-b' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := integration.Sync(ctx, a, "origin"); err != nil {
		t.Fatal(err)
	}
	localBefore := git(t, b, "rev-parse", repository.DataRef)
	status, err = integration.Sync(ctx, b, "origin")
	var conflictErr *integration.MergeConflictError
	if !errors.As(err, &conflictErr) || len(conflictErr.Set.Unresolved()) != 1 {
		t.Fatalf("row conflict = %#v, %v", conflictErr, err)
	}
	if status.Action != "conflicts" || git(t, b, "rev-parse", repository.DataRef) != localBefore {
		t.Fatal("conflicting merge changed the live local history")
	}

	// Conflict details and a partial resolution are durable outside the process.
	persisted, err := integration.Conflicts(ctx, b, "origin")
	if err != nil || persisted.Conflicts[0].ID != conflictErr.Set.Conflicts[0].ID {
		t.Fatalf("persisted conflicts = %#v, %v", persisted, err)
	}
	status, err = integration.Resolve(ctx, b, "origin", persisted.Conflicts[0].ID, engine.TakeRemote)
	if err != nil || status.Action != "merged" {
		t.Fatalf("resolve conflict = %#v, %v", status, err)
	}
	if _, err := integration.Conflicts(ctx, b, "origin"); !errors.Is(err, integration.ErrNoConflicts) {
		t.Fatalf("completed conflict state remains: %v", err)
	}
	resolved, err := fresh.Query(ctx, "SELECT title FROM issues WHERE id = 1")
	if err != nil || resolved.Rows[0][0] != "choice-a" {
		t.Fatalf("resolved row = %#v, %v", resolved.Rows, err)
	}
	if status, err := integration.Sync(ctx, a, "origin"); err != nil || status.Action != "fast-forwarded-local" {
		t.Fatalf("pull resolution = %#v, %v", status, err)
	}
	afterMergeEngineA, err := engine.Open(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer afterMergeEngineA.Close()
	afterMergeA, _ := afterMergeEngineA.NewSession()
	if err := afterMergeA.Exec(ctx, "DELETE FROM repodb.issues WHERE id = 2"); err != nil {
		t.Fatal(err)
	}
	if err := fresh.Exec(ctx, "UPDATE issues SET title = 'updated-b' WHERE id = 2"); err != nil {
		t.Fatal(err)
	}
	if _, err := integration.Sync(ctx, a, "origin"); err != nil {
		t.Fatal(err)
	}
	_, err = integration.Sync(ctx, b, "origin")
	if !errors.As(err, &conflictErr) || len(conflictErr.Set.Unresolved()) != 1 {
		t.Fatalf("update/delete conflict = %#v, %v", conflictErr, err)
	}
	status, err = integration.Resolve(ctx, b, "origin", conflictErr.Set.Unresolved()[0].ID, engine.TakeRemote)
	if err != nil || status.Action != "merged" {
		t.Fatalf("resolve deletion = %#v, %v", status, err)
	}
	deleted, err := fresh.Query(ctx, "SELECT id FROM issues WHERE id = 2")
	if err != nil || len(deleted.Rows) != 0 {
		t.Fatalf("resolved deletion rows = %#v, %v", deleted.Rows, err)
	}
	head := git(t, b, "rev-parse", repository.DataRef)
	if status, err := integration.Sync(ctx, b, "origin"); err != nil || status.Action != "up-to-date" || git(t, b, "rev-parse", repository.DataRef) != head {
		t.Fatalf("repeat sync = %#v, %v", status, err)
	}
	for _, clone := range []string{a, b} {
		if got := git(t, clone, "status", "--porcelain=v1"); got != "" {
			t.Fatalf("source worktree dirty in %s: %q", clone, got)
		}
	}
}

func TestSyncReportsIncompatibleSchemaChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	git(t, root, "init", "--bare", remote)
	git(t, root, "init", "--quiet", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", "README.md")
	git(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "source")
	git(t, seed, "remote", "add", "origin", remote)
	git(t, seed, "push", "origin", "main")
	git(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	git(t, root, "clone", "--quiet", remote, a)
	git(t, root, "clone", "--quiet", remote, b)
	if _, err := integration.Enable(ctx, a, "origin"); err != nil {
		t.Fatal(err)
	}
	if _, err := integration.Sync(ctx, a, "origin"); err != nil {
		t.Fatal(err)
	}
	if _, err := integration.Enable(ctx, b, "origin"); err != nil {
		t.Fatal(err)
	}
	engA, _ := engine.Open(ctx, a)
	defer engA.Close()
	engB, _ := engine.Open(ctx, b)
	defer engB.Close()
	sa, _ := engA.NewSession()
	sb, _ := engB.NewSession()
	if err := sa.Exec(ctx, "CREATE TABLE settings (id BIGINT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if err := sb.Exec(ctx, "CREATE TABLE settings (id BIGINT PRIMARY KEY, enabled BOOLEAN NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := integration.Sync(ctx, a, "origin"); err != nil {
		t.Fatal(err)
	}
	_, err := integration.Sync(ctx, b, "origin")
	var conflictErr *integration.MergeConflictError
	if !errors.As(err, &conflictErr) || len(conflictErr.Set.Conflicts) != 1 || conflictErr.Set.Conflicts[0].Kind != "schema" {
		t.Fatalf("schema conflict = %#v, %v", conflictErr, err)
	}
}

func TestEnableRejectsInvalidFetchedSQLGraph(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	clone := filepath.Join(root, "clone")
	git(t, root, "init", "--bare", remote)
	git(t, root, "init", "--quiet", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", "README.md")
	git(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "source")
	git(t, seed, "remote", "add", "origin", remote)
	git(t, seed, "push", "origin", "main")
	git(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	repo, err := repository.Init(ctx, seed)
	if err != nil {
		t.Fatal(err)
	}
	writer, _ := repo.Begin(ctx)
	schema, _ := writer.Put(ctx, []byte("not a RepoDB SQL schema"))
	data, _ := writer.Put(ctx, []byte("not a Prolly tree"))
	if _, err := writer.Commit(ctx, repository.Manifest{Tables: map[string]repository.Table{"broken": {SchemaRoot: schema, DataRoot: data}}}); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "push", "origin", repository.DataRef+":"+repository.DataRef)
	git(t, root, "clone", "--quiet", remote, clone)
	if _, err := integration.Enable(ctx, clone, "origin"); err == nil || !strings.Contains(err.Error(), "validate fetched RepoDB SQL graph") {
		t.Fatalf("invalid graph enable error = %v", err)
	}
	if got := gitAllowFailure(clone, "rev-parse", "--verify", "--quiet", repository.DataRef); got != "" {
		t.Fatalf("invalid fetched snapshot became live: %s", got)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func gitAllowFailure(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, _ := cmd.CombinedOutput()
	return strings.TrimSpace(string(output))
}
