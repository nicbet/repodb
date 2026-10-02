package engine_test

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	repodbgit "github.com/nicbet/repodb/common/git"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/common/storage"
	"github.com/nicbet/repodb/engine"
)

// objectOID returns the Git object ID commit stores hash under.
func objectOID(t *testing.T, repo *repository.Repository, commit string, hash storage.Hash) string {
	t.Helper()
	entries, err := repodbgit.CLI{}.ListTreeObjects(context.Background(), repo.Root, commit)
	if err != nil {
		t.Fatal(err)
	}
	oid := entries[treeObjectPath(hash)]
	if oid == "" {
		t.Fatalf("commit %s has no object %s", commit, hash)
	}
	return oid
}

// looseObjectPath is where Git stores oid as a loose object.
func looseObjectPath(repo *repository.Repository, oid string) string {
	return filepath.Join(repo.CommonDir, "objects", oid[:2], oid[2:])
}

// corruptLooseObject replaces the content of the loose blob oid on disk,
// leaving its name, as a failing disk would.
func corruptLooseObject(t *testing.T, repo *repository.Repository, oid string, data []byte) {
	t.Helper()
	var buf bytes.Buffer
	writer := zlib.NewWriter(&buf)
	fmt.Fprintf(writer, "blob %d\x00", len(data))
	writer.Write(data)
	writer.Close()
	path := looseObjectPath(repo, oid)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
}

func checkProblems(t *testing.T, report engine.CheckReport) []string {
	t.Helper()
	var problems []string
	for _, commit := range report.Commits {
		problems = append(problems, commit.Problems...)
	}
	return problems
}

func hasProblem(problems []string, substring string) bool {
	return slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, substring) })
}

func TestCheckHealthyRepository(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, _ := openJournalEngine(t, ctx, root)
	execAll(t, eng,
		"CREATE TABLE a (id BIGINT PRIMARY KEY, v VARCHAR(20))",
		"CREATE TABLE b (id BIGINT PRIMARY KEY, v VARCHAR(20))",
		"CREATE INDEX b_v ON b (v)",
		"INSERT INTO a VALUES (1, 'a')",
		"INSERT INTO b VALUES (1, 'b')",
	)
	first, err := eng.Checkpoint(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	execAll(t, eng, "INSERT INTO a VALUES (2, 'a')")
	second, err := eng.Checkpoint(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	repo := eng.Repository()

	report, err := engine.Check(ctx, repo, engine.CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Commits) != 1 || report.Commits[0].Commit != second.Commit || report.Problems() != 0 || len(report.Commits[0].Warnings) != 0 {
		t.Fatalf("check head = %+v", report)
	}
	report, err = engine.Check(ctx, repo, engine.CheckOptions{Revision: first.Commit})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Commits) != 1 || report.Commits[0].Commit != first.Commit || report.Problems() != 0 {
		t.Fatalf("check revision = %+v", report)
	}
	report, err = engine.Check(ctx, repo, engine.CheckOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	// Init, first and second.
	if len(report.Commits) != 3 || report.Problems() != 0 {
		t.Fatalf("check all = %+v", report)
	}
	if _, err := engine.Check(ctx, repo, engine.CheckOptions{Revision: "no-such-revision"}); err == nil {
		t.Fatal("check of an unknown revision succeeded")
	}
}

// Check reports every corrupt and missing object of a commit whose objects
// the process has already read and validated, with every table they break.
func TestCheckFindsCorruptionBehindWarmCaches(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, _ := openJournalEngine(t, ctx, root)
	execAll(t, eng,
		// Distinct columns give each table its own schema object.
		"CREATE TABLE a (id BIGINT PRIMARY KEY, v VARCHAR(20))",
		"CREATE TABLE b (id BIGINT PRIMARY KEY, u VARCHAR(20))",
		"CREATE TABLE c (id BIGINT PRIMARY KEY, w VARCHAR(20))",
		"INSERT INTO a VALUES (1, 'a')",
		"INSERT INTO b VALUES (1, 'b')",
		"INSERT INTO c VALUES (1, 'c')",
	)
	result, err := eng.Checkpoint(ctx, "checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	repo := eng.Repository()
	tables := result.Snapshot.Manifest.Tables
	corrupt, missing := tables["a"].DataRoot, tables["b"].SchemaRoot
	corruptOID, missingOID := objectOID(t, repo, result.Commit, corrupt), objectOID(t, repo, result.Commit, missing)
	corruptLooseObject(t, repo, corruptOID, []byte("bit rot"))
	if err := os.Remove(looseObjectPath(repo, missingOID)); err != nil {
		t.Fatal(err)
	}

	// The engine holds the commit's snapshot, so an ordinary open reuses its
	// checked objects, and validation trusts the earlier result.
	snapshot, err := repo.SnapshotCommit(ctx, result.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ValidateSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("warm validation err = %v, want the cached result", err)
	}

	report, err := engine.Check(ctx, repo, engine.CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	problems := checkProblems(t, report)
	for _, want := range []string{
		fmt.Sprintf("object %s: content does not match its name", corrupt),
		fmt.Sprintf("object %s: missing Git object %s", missing, missingOID),
		"validate SQL table a",
		"validate SQL table b",
	} {
		if !hasProblem(problems, want) {
			t.Errorf("problems %q lack %q", problems, want)
		}
	}
	if hasProblem(problems, "table c") {
		t.Errorf("problems %q name the intact table c", problems)
	}
	if report.FailedCommits() != 1 {
		t.Errorf("failed commits = %d, want 1", report.FailedCommits())
	}
}

// A tree that disagrees with its manifest is reported once, without object or
// SQL checks against the untrustworthy inventory.
func TestCheckReportsInventoryMismatch(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, _ := openJournalEngine(t, ctx, root)
	execAll(t, eng, "CREATE TABLE t (id BIGINT PRIMARY KEY)", "INSERT INTO t VALUES (1)")
	result, err := eng.Checkpoint(ctx, "checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	repo := eng.Repository()
	cli := repodbgit.CLI{}
	entries, err := cli.ListTreeObjects(ctx, repo.Root, result.Commit)
	if err != nil {
		t.Fatal(err)
	}
	delete(entries, "manifest.json")
	stray, err := cli.HashObject(ctx, repo.Root, []byte("stray"))
	if err != nil {
		t.Fatal(err)
	}
	entries["stray"] = stray
	commit := fakeCommit(t, repo, result.Commit, result.Snapshot.Manifest, entries)

	report, err := engine.Check(ctx, repo, engine.CheckOptions{Revision: commit})
	if err != nil {
		t.Fatal(err)
	}
	if problems := checkProblems(t, report); len(problems) != 1 || !strings.Contains(problems[0], "unlisted tree entry stray") {
		t.Fatalf("problems = %q, want one unlisted-entry problem", problems)
	}
}

// fakeCommitWith writes a commit of base's tables and objects plus extra
// blobs, each listed under its SHA-256 name, after edit adjusts the manifest.
func fakeCommitWith(t *testing.T, repo *repository.Repository, base *repository.Snapshot, extra [][]byte, edit func(*repository.Manifest, []storage.Hash)) string {
	t.Helper()
	ctx := context.Background()
	cli := repodbgit.CLI{}
	entries, err := cli.ListTreeObjects(ctx, repo.Root, base.Commit)
	if err != nil {
		t.Fatal(err)
	}
	delete(entries, "manifest.json")
	manifest := base.Manifest
	manifest.Tables = make(map[string]repository.Table, len(base.Manifest.Tables))
	for name, table := range base.Manifest.Tables {
		manifest.Tables[name] = table
	}
	manifest.Objects = slices.Clone(base.Manifest.Objects)
	hashes := make([]storage.Hash, len(extra))
	for i, data := range extra {
		oid, err := cli.HashObject(ctx, repo.Root, data)
		if err != nil {
			t.Fatal(err)
		}
		hashes[i] = storage.Sum(data)
		entries[treeObjectPath(hashes[i])] = oid
		manifest.Objects = append(manifest.Objects, hashes[i])
	}
	slices.Sort(manifest.Objects)
	edit(&manifest, hashes)
	return fakeCommit(t, repo, base.Commit, manifest, entries)
}

// A table that fails SQL validation is reported, and the other tables are
// still checked. An unreferenced object is only a warning.
func TestCheckReportsBadTableAndUnreferencedObject(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, _ := openJournalEngine(t, ctx, root)
	execAll(t, eng,
		"CREATE TABLE a (id BIGINT PRIMARY KEY)",
		"CREATE TABLE b (id BIGINT PRIMARY KEY)",
		"INSERT INTO a VALUES (1)",
		"INSERT INTO b VALUES (1)",
	)
	result, err := eng.Checkpoint(ctx, "checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	repo := eng.Repository()

	// Table a's data root is a well-named blob that is not a Prolly node.
	badTable := fakeCommitWith(t, repo, result.Snapshot, [][]byte{[]byte("not a node")}, func(manifest *repository.Manifest, hashes []storage.Hash) {
		table := manifest.Tables["a"]
		table.DataRoot = hashes[0]
		manifest.Tables["a"] = table
	})
	report, err := engine.Check(ctx, repo, engine.CheckOptions{Revision: badTable})
	if err != nil {
		t.Fatal(err)
	}
	problems := checkProblems(t, report)
	if len(problems) != 1 || !strings.Contains(problems[0], "validate SQL table a") {
		t.Fatalf("problems = %q, want one for table a", problems)
	}

	unreferenced := fakeCommitWith(t, repo, result.Snapshot, [][]byte{[]byte("nobody's")}, func(*repository.Manifest, []storage.Hash) {})
	report, err = engine.Check(ctx, repo, engine.CheckOptions{Revision: unreferenced})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("unreferenced object %s", storage.Sum([]byte("nobody's")))
	if report.Problems() != 0 || !slices.Equal(report.Commits[0].Warnings, []string{want}) {
		t.Fatalf("report = %+v, want only the warning %q", report, want)
	}
}

// Checking all commits finds corruption in an object only an old commit
// lists, and validates a table unchanged between commits once.
func TestCheckAllFindsCorruptionInOldCommit(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, _ := openJournalEngine(t, ctx, root)
	execAll(t, eng,
		"CREATE TABLE a (id BIGINT PRIMARY KEY, v VARCHAR(20))",
		"CREATE TABLE b (id BIGINT PRIMARY KEY, v VARCHAR(20))",
		"INSERT INTO a VALUES (1, 'old')",
		"INSERT INTO b VALUES (1, 'b')",
	)
	old, err := eng.Checkpoint(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	execAll(t, eng, "UPDATE a SET v = 'new' WHERE id = 1")
	head, err := eng.Checkpoint(ctx, "new")
	if err != nil {
		t.Fatal(err)
	}
	repo := eng.Repository()
	victim := old.Snapshot.Manifest.Tables["a"].DataRoot
	if slices.Contains(head.Snapshot.Manifest.Objects, victim) {
		t.Fatal("old data root is still listed at head")
	}
	corruptLooseObject(t, repo, objectOID(t, repo, old.Commit, victim), []byte("bit rot"))

	report, err := engine.Check(ctx, repo, engine.CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Problems() != 0 {
		t.Fatalf("head check problems = %q", checkProblems(t, report))
	}
	engine.ResetPerformanceCounters()
	report, err = engine.Check(ctx, repo, engine.CheckOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Commits) != 3 || report.FailedCommits() != 1 {
		t.Fatalf("check all = %+v", report)
	}
	for _, commit := range report.Commits {
		if (len(commit.Problems) != 0) != (commit.Commit == old.Commit) {
			t.Fatalf("commit %s problems = %q", commit.Commit, commit.Problems)
		}
	}
	if !hasProblem(checkProblems(t, report), fmt.Sprintf("object %s: content does not match its name", victim)) {
		t.Fatalf("problems = %q", checkProblems(t, report))
	}
	// Head validates a and b; the old commit only its own a.
	if got := engine.ReadPerformanceCounters().TablesValidated; got != 3 {
		t.Fatalf("validated %d tables, want 3", got)
	}
}
