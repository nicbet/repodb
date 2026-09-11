package repository_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	repodbgit "github.com/nicbet/repodb/common/git"
	"github.com/nicbet/repodb/common/prolly"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/common/storage"
	"golang.org/x/sys/unix"
)

func TestSnapshotRoundTripTransferAndCleanSourceState(t *testing.T) {
	ctx := context.Background()
	root := initRepository(t)
	beforeHead := git(t, root, "rev-parse", "HEAD")
	beforeStatus := git(t, root, "status", "--porcelain=v1")

	repo, err := repository.Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := repo.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]prolly.Entry, 300)
	for i := range entries {
		entries[i] = prolly.Entry{
			Key:   []byte(fmt.Sprintf("issue-%04d", i)),
			Value: []byte(fmt.Sprintf("title-%04d", i)),
		}
	}
	tree, err := prolly.Build(ctx, writer, entries, prolly.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := writer.Put(ctx, []byte(`{"columns":["id","title"]}`))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := writer.Commit(ctx, repository.Manifest{
		DefaultDatabase: "repodb",
		Tables: map[string]repository.Table{
			"issues": {SchemaRoot: schema, DataRoot: tree.Root()},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSourceState(t, root, beforeHead, beforeStatus)

	if err := os.MkdirAll(repo.CacheDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.CacheDir(), "discardable"), []byte("cache"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(repo.CacheDir()); err != nil {
		t.Fatal(err)
	}
	git(t, root, "gc", "--prune=now")

	reopened, err := repository.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	gotSnapshot, err := reopened.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gotSnapshot.Commit != snapshot.Commit {
		t.Fatalf("reopened commit = %s, want %s", gotSnapshot.Commit, snapshot.Commit)
	}
	readTree, err := prolly.Open(gotSnapshot.Store(), gotSnapshot.Manifest.Tables["issues"].DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	value, err := readTree.Get(ctx, []byte("issue-0123"))
	if err != nil {
		t.Fatal(err)
	}
	if string(value) != "title-0123" {
		t.Fatalf("value = %q", value)
	}

	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, root, "init", "--bare", bare)
	git(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	git(t, root, "remote", "add", "origin", bare)
	git(t, root, "push", "origin", "main", repository.DataRef+":"+repository.DataRef)
	clone := filepath.Join(t.TempDir(), "clone")
	git(t, root, "clone", bare, clone)
	git(t, clone, "fetch", "origin", repository.DataRef+":"+repository.DataRef)
	transferred, err := repository.Open(ctx, clone)
	if err != nil {
		t.Fatal(err)
	}
	transferredSnapshot, err := transferred.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	transferredTree, err := prolly.Open(transferredSnapshot.Store(), transferredSnapshot.Manifest.Tables["issues"].DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	value, err = transferredTree.Get(ctx, []byte("issue-0299"))
	if err != nil || string(value) != "title-0299" {
		t.Fatalf("transferred value = %q, err = %v", value, err)
	}
	if status := git(t, clone, "status", "--porcelain=v1"); status != "" {
		t.Fatalf("transferred source worktree is dirty: %s", status)
	}
}

func TestStaleWriterCannotExposeItsSnapshot(t *testing.T) {
	ctx := context.Background()
	root := initRepository(t)
	repo, err := repository.Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := repo.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := repo.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	winnerRoot, _ := first.Put(ctx, []byte("winner"))
	winner, err := first.Commit(ctx, repository.Manifest{
		Tables: map[string]repository.Table{"issues": {DataRoot: winnerRoot}},
	})
	if err != nil {
		t.Fatal(err)
	}
	loserRoot, _ := stale.Put(ctx, []byte("loser"))
	if _, err := stale.Commit(ctx, repository.Manifest{
		Tables: map[string]repository.Table{"issues": {DataRoot: loserRoot}},
	}); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("stale commit error = %v, want ErrConflict", err)
	}
	current, err := repo.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if current.Commit != winner.Commit || current.Manifest.Tables["issues"].DataRoot != winnerRoot {
		t.Fatal("failed publication changed the live snapshot")
	}
}

func TestConcurrentWritersPublishExactlyOneSnapshot(t *testing.T) {
	ctx := context.Background()
	root := initRepository(t)
	repo, err := repository.Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	writers := make([]*repository.Writer, 2)
	roots := make([]storage.Hash, 2)
	for i := range writers {
		writers[i], err = repo.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		roots[i], err = writers[i].Put(ctx, []byte(fmt.Sprintf("writer-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	errs := make([]error, 2)
	var wait sync.WaitGroup
	for i := range writers {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			<-start
			_, errs[i] = writers[i].Commit(ctx, repository.Manifest{
				Tables: map[string]repository.Table{"issues": {DataRoot: roots[i]}},
			})
		}(i)
	}
	close(start)
	wait.Wait()
	successes, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, repository.ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent publication error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes = %d, conflicts = %d", successes, conflicts)
	}
}

func TestPublicationOutcomeAndRecovery(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name    string
		point   repository.PublicationPoint
		outcome repository.CommitOutcome
		liveNew bool
	}{
		{"before", repository.BeforeRefPublication, repository.OutcomeRejected, false},
		{"during", repository.DuringRefPublication, repository.OutcomeUnknown, false},
		{"after", repository.AfterRefPublication, repository.OutcomeCommitted, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := initRepository(t)
			repo, err := repository.Init(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			old, _ := repo.Current(ctx)
			writer, _ := repo.Begin(ctx)
			rootHash, _ := writer.Put(ctx, []byte("candidate"))
			repo.SetPublicationFaultInjector(func(point repository.PublicationPoint) error {
				if point == test.point {
					return errors.New("injected failure")
				}
				return nil
			})
			result, err := writer.CommitWithOutcome(ctx, repository.Manifest{Tables: map[string]repository.Table{"t": {DataRoot: rootHash}}})
			if err == nil || result.Outcome != test.outcome {
				t.Fatalf("result = %#v, error = %v", result, err)
			}
			repo.SetPublicationFaultInjector(nil)
			current, err := repo.Current(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if (current.Commit == result.Commit) != test.liveNew {
				t.Fatalf("candidate live = %v, want %v", current.Commit == result.Commit, test.liveNew)
			}
			if !test.liveNew && current.Commit != old.Commit {
				t.Fatal("failed publication changed live state")
			}
			if result.Commit != "" {
				if test.liveNew {
					successor, _ := repo.Begin(ctx)
					nextHash, _ := successor.Put(ctx, []byte("successor"))
					if _, err := successor.Commit(ctx, repository.Manifest{Tables: map[string]repository.Table{"t": {DataRoot: nextHash}}}); err != nil {
						t.Fatal(err)
					}
				}
				recovered, err := repo.RecoverCommit(ctx, result.Commit)
				if err != nil {
					t.Fatal(err)
				}
				want := repository.OutcomeRejected
				if test.liveNew {
					want = repository.OutcomeCommitted
				}
				if recovered.Outcome != want {
					t.Fatalf("recovery outcome = %v, want %v", recovered.Outcome, want)
				}
			}
		})
	}
}

func TestLockCancellationIsDefiniteRejection(t *testing.T) {
	ctx := context.Background()
	root := initRepository(t)
	repo, err := repository.Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := repo.Current(ctx)
	lockPath := filepath.Join(repo.CommonDir, "repodb", "locks", "publish.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	writer, _ := repo.Begin(ctx)
	hash, _ := writer.Put(ctx, []byte("blocked"))
	canceled, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	result, err := writer.CommitWithOutcome(canceled, repository.Manifest{Tables: map[string]repository.Table{"t": {DataRoot: hash}}})
	if !errors.Is(err, context.DeadlineExceeded) || result.Outcome != repository.OutcomeRejected {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	current, _ := repo.Current(ctx)
	if current.Commit != old.Commit {
		t.Fatal("canceled lock wait changed live state")
	}
}

func TestSeparateProcessWritersAcrossLinkedWorktrees(t *testing.T) {
	if os.Getenv("REPODB_WRITER_HELPER") != "" {
		t.Skip("parent only")
	}
	root := initRepository(t)
	if _, err := repository.Init(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	git(t, root, "worktree", "add", "-b", "writer-linked", linked)
	barrier := t.TempDir()
	commands := make([]*exec.Cmd, 2)
	outputs := make([]bytes.Buffer, 2)
	for i, dir := range []string{root, linked} {
		commands[i] = exec.Command(os.Args[0], "-test.run=^TestPublicationProcessHelper$")
		commands[i].Env = append(os.Environ(), "REPODB_WRITER_HELPER=1", "REPODB_WRITER_ROOT="+dir, fmt.Sprintf("REPODB_WRITER_ID=%d", i), "REPODB_WRITER_BARRIER="+barrier)
		commands[i].Stdout, commands[i].Stderr = &outputs[i], &outputs[i]
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	for i := range commands {
		for {
			if _, err := os.Stat(filepath.Join(barrier, fmt.Sprintf("ready-%d", i))); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("writer %d did not become ready: %s", i, outputs[i].String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := os.WriteFile(filepath.Join(barrier, "go"), []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	successes, conflicts := 0, 0
	for i, cmd := range commands {
		err := cmd.Wait()
		if err == nil {
			successes++
			continue
		}
		if strings.Contains(outputs[i].String(), repository.ErrConflict.Error()) {
			conflicts++
			continue
		}
		t.Fatalf("writer %d failed unexpectedly: %v\n%s", i, err, outputs[i].String())
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestPublicationProcessHelper(t *testing.T) {
	if os.Getenv("REPODB_WRITER_HELPER") == "" {
		t.Skip("helper process")
	}
	ctx := context.Background()
	repo, err := repository.Open(ctx, os.Getenv("REPODB_WRITER_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	writer, err := repo.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rootHash, err := writer.Put(ctx, []byte("writer-"+os.Getenv("REPODB_WRITER_ID")))
	if err != nil {
		t.Fatal(err)
	}
	barrier := os.Getenv("REPODB_WRITER_BARRIER")
	if err := os.WriteFile(filepath.Join(barrier, "ready-"+os.Getenv("REPODB_WRITER_ID")), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(barrier, "go")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("barrier timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, err = writer.Commit(ctx, repository.Manifest{Tables: map[string]repository.Table{"t": {DataRoot: rootHash}}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestKilledWriterLeavesOldSnapshotAndReleasesLock(t *testing.T) {
	if os.Getenv("REPODB_WRITER_HELPER") != "" {
		t.Skip("parent only")
	}
	root := initRepository(t)
	repo, err := repository.Init(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := repo.Current(context.Background())
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestKilledWriterHelper$")
	cmd.Env = append(os.Environ(), "REPODB_KILL_HELPER=1", "REPODB_WRITER_ROOT="+root, "REPODB_KILL_READY="+ready)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("helper timeout: %s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	current, err := repo.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if current.Commit != old.Commit {
		t.Fatal("terminated pre-publication writer changed live state")
	}
	writer, err := repo.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := writer.Put(context.Background(), []byte("recovered"))
	if _, err := writer.Commit(context.Background(), repository.Manifest{Tables: map[string]repository.Table{"t": {DataRoot: hash}}}); err != nil {
		t.Fatalf("write after terminated lock holder: %v", err)
	}
}

func TestKilledWriterHelper(t *testing.T) {
	if os.Getenv("REPODB_KILL_HELPER") == "" {
		t.Skip("helper process")
	}
	repo, err := repository.Open(context.Background(), os.Getenv("REPODB_WRITER_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	writer, _ := repo.Begin(context.Background())
	hash, _ := writer.Put(context.Background(), []byte("never published"))
	repo.SetPublicationFaultInjector(func(point repository.PublicationPoint) error {
		if point == repository.BeforeRefPublication {
			if err := os.WriteFile(os.Getenv("REPODB_KILL_READY"), []byte("ready"), 0o600); err != nil {
				return err
			}
			select {}
		}
		return nil
	})
	_, _ = writer.Commit(context.Background(), repository.Manifest{Tables: map[string]repository.Table{"t": {DataRoot: hash}}})
}

func TestLinkedWorktreeSharesRepositoryIdentity(t *testing.T) {
	ctx := context.Background()
	root := initRepository(t)
	repo, err := repository.Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	git(t, root, "worktree", "add", "-b", "linked", linked)
	fromLinked, err := repository.Open(ctx, linked)
	if err != nil {
		t.Fatal(err)
	}
	if fromLinked.CommonDir != repo.CommonDir {
		t.Fatalf("common dir = %q, want %q", fromLinked.CommonDir, repo.CommonDir)
	}
	if fromLinked.CacheDir() != repo.CacheDir() {
		t.Fatalf("cache dir = %q, want %q", fromLinked.CacheDir(), repo.CacheDir())
	}
}

func TestSHA256GitRepositoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cmd := exec.Command("git", "init", "--quiet", "--object-format=sha256", "-b", "main")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("installed Git does not support SHA-256 repositories: %v: %s", err, output)
	}
	repo, err := repository.Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if repo.ObjectFormat != "sha256" {
		t.Fatalf("object format = %q", repo.ObjectFormat)
	}
	writer, err := repo.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rootHash, err := writer.Put(ctx, []byte("sha256 Git repository"))
	if err != nil {
		t.Fatal(err)
	}
	written, err := writer.Commit(ctx, repository.Manifest{
		Tables: map[string]repository.Table{"objects": {DataRoot: rootHash}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(written.Commit) != 64 {
		t.Fatalf("Git SHA-256 commit length = %d", len(written.Commit))
	}
	data, err := written.Store().Get(ctx, rootHash)
	if err != nil || string(data) != "sha256 Git repository" {
		t.Fatalf("round-trip data = %q, err = %v", data, err)
	}
}

func TestAbsentLegacyUnsupportedAndCorruptRepositories(t *testing.T) {
	ctx := context.Background()

	absent := initRepository(t)
	if _, err := repository.Open(ctx, absent); !errors.Is(err, repository.ErrNotInitialized) {
		t.Fatalf("absent error = %v", err)
	}

	legacy := initRepository(t)
	writeLegacyLayout(t, legacy)
	legacyStatus := git(t, legacy, "status", "--porcelain=v1")
	if _, err := repository.Init(ctx, legacy); !errors.Is(err, repository.ErrLegacyLayout) {
		t.Fatalf("legacy init error = %v", err)
	}
	repo, imported, err := repository.ImportLegacy(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(legacy, ".repodb", "manifest.json")); err != nil {
		t.Fatalf("legacy files were removed: %v", err)
	}
	if status := git(t, legacy, "status", "--porcelain=v1"); status != legacyStatus {
		t.Fatalf("legacy import changed source state: %q != %q", status, legacyStatus)
	}
	rootHash := imported.Manifest.Tables["issues"].DataRoot
	if data, err := imported.Store().Get(ctx, rootHash); err != nil || string(data) != "legacy rows" {
		t.Fatalf("imported object = %q, err = %v", data, err)
	}

	unsupported := []byte("{\"format_version\":99,\"default_database\":\"repodb\",\"tables\":{},\"objects\":[]}\n")
	installRawManifest(t, repo, imported.Commit, unsupported)
	if _, err := repository.Open(ctx, legacy); err == nil || !strings.Contains(err.Error(), "unsupported RepoDB format 99") {
		t.Fatalf("unsupported format error = %v", err)
	}

	corruptRoot := initRepository(t)
	corruptRepo, err := repository.Init(ctx, corruptRoot)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := corruptRepo.Current(ctx)
	fake := strings.Repeat("a", 64)
	corrupt := []byte(fmt.Sprintf("{\"format_version\":1,\"default_database\":\"repodb\",\"tables\":{},\"objects\":[%q]}\n", fake))
	installRawManifest(t, corruptRepo, base.Commit, corrupt)
	if _, err := repository.Open(ctx, corruptRoot); !errors.Is(err, repository.ErrCorrupt) {
		t.Fatalf("corrupt snapshot error = %v", err)
	}
}

func installRawManifest(t *testing.T, repo *repository.Repository, old string, manifest []byte) {
	t.Helper()
	ctx := context.Background()
	cli := repodbgit.CLI{}
	oid, err := cli.HashObject(ctx, repo.Root, manifest)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := cli.WriteTree(ctx, repo.Root, repo.CommonDir, []repodbgit.TreeEntry{{Path: "manifest.json", ObjectID: oid}})
	if err != nil {
		t.Fatal(err)
	}
	commit, err := cli.CommitTree(ctx, repo.Root, tree, old, "invalid test snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.UpdateRef(ctx, repo.Root, repository.DataRef, commit, old); err != nil {
		t.Fatal(err)
	}
}

func writeLegacyLayout(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, ".repodb")
	store := storage.NewFilesystem(filepath.Join(dir, "objects", "sha256"))
	hash, err := store.Put(context.Background(), []byte("legacy rows"))
	if err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, "config.json"), map[string]any{
		"format_version": 1, "default_database": "repodb",
	})
	writeJSON(t, filepath.Join(dir, "manifest.json"), map[string]any{
		"format_version": 1,
		"tables":         map[string]repository.Table{"issues": {DataRoot: hash}},
	})
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func initRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	git(t, root, "init", "--quiet", "-b", "main")
	git(t, root, "config", "user.name", "RepoDB Test")
	git(t, root, "config", "user.email", "test@repodb.invalid")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "README.md")
	git(t, root, "commit", "--quiet", "-m", "source")
	return root
}

func assertSourceState(t *testing.T, root, head, status string) {
	t.Helper()
	if got := git(t, root, "rev-parse", "HEAD"); got != head {
		t.Fatalf("source HEAD = %s, want %s", got, head)
	}
	if got := git(t, root, "status", "--porcelain=v1"); got != status {
		t.Fatalf("source status = %q, want %q", got, status)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}
