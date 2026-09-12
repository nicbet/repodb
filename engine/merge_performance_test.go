package engine_test

import (
	"context"
	"os/exec"
	"testing"

	repodbgit "github.com/nicbet/repodb/common/git"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

func TestMergeRetainsSelectedRootsAndReusesLoadedSnapshots(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	repo, err := repository.Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	session, _ := eng.NewSession()
	for _, statement := range []string{
		"CREATE TABLE stable (id BIGINT PRIMARY KEY, value TEXT NOT NULL)",
		"CREATE TABLE issues (id BIGINT PRIMARY KEY, value TEXT NOT NULL)",
		"INSERT INTO stable VALUES (1, 'unchanged')",
		"INSERT INTO issues VALUES (1, 'base')",
	} {
		if err := session.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	base, err := repo.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Exec(ctx, "INSERT INTO issues VALUES (2, 'local')"); err != nil {
		t.Fatal(err)
	}
	local, err := repo.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	updateDataRef(t, root, base.Commit, local.Commit)
	if err := session.Exec(ctx, "CREATE TABLE remote_only (id BIGINT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if err := session.Exec(ctx, "INSERT INTO remote_only VALUES (1, 'remote')"); err != nil {
		t.Fatal(err)
	}
	if err := session.Exec(ctx, "INSERT INTO issues VALUES (3, 'remote')"); err != nil {
		t.Fatal(err)
	}
	remote, err := repo.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	updateDataRef(t, root, local.Commit, remote.Commit)
	for _, snapshot := range []*repository.Snapshot{base, local, remote} {
		if err := engine.ValidateSnapshot(ctx, snapshot); err != nil {
			t.Fatal(err)
		}
	}

	repodbgit.ResetProcessCount()
	writer, err := repo.BeginMergeSnapshots(local, remote)
	if err != nil {
		t.Fatal(err)
	}
	manifest, hashes, conflicts, err := engine.MergeSnapshots(ctx, writer, base, local, remote, nil)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("merge: err=%v conflicts=%v", err, conflicts)
	}
	if got := repodbgit.ProcessCount(); got != 0 {
		t.Fatalf("merge construction and computation launched %d Git processes", got)
	}
	if manifest.Tables["stable"] != local.Manifest.Tables["stable"] {
		t.Fatal("unchanged local table roots were rebuilt")
	}
	if manifest.Tables["remote_only"] != remote.Manifest.Tables["remote_only"] {
		t.Fatal("remote-selected table roots were rebuilt")
	}
	if err := writer.RetainOnly(hashes); err != nil {
		t.Fatal(err)
	}
	merged, err := writer.Commit(ctx, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Manifest.Tables["stable"] != local.Manifest.Tables["stable"] || merged.Manifest.Tables["remote_only"] != remote.Manifest.Tables["remote_only"] {
		t.Fatal("published snapshot did not retain selected roots")
	}
	if err := engine.ValidateSnapshot(ctx, merged); err != nil {
		t.Fatal(err)
	}
	_ = session.Close()
	_ = eng.Close()
	reopened, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reader, _ := reopened.NewSession()
	result, err := reader.Query(ctx, "SELECT value FROM remote_only WHERE id = 1")
	if err != nil || len(result.Rows) != 1 || result.Rows[0][0] != "remote" {
		t.Fatalf("remote-selected table after reopen = %#v, %v", result.Rows, err)
	}
}

func updateDataRef(t *testing.T, root, target, expected string) {
	t.Helper()
	cmd := exec.Command("git", "update-ref", repository.DataRef, target, expected)
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update data ref: %v: %s", err, output)
	}
}
