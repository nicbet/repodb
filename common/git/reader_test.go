package git

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newReaderTestRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if output, err := exec.Command("git", "init", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	t.Cleanup(func() { CloseReaders(root) })
	return root
}

func writeBlobs(t *testing.T, root string, blobs ...[]byte) []string {
	t.Helper()
	oids, err := CLI{}.HashObjects(context.Background(), root, filepath.Join(root, ".git", "repodb", "tmp"), blobs)
	if err != nil {
		t.Fatal(err)
	}
	return oids
}

func TestReaderReusesOneProcess(t *testing.T) {
	ctx := context.Background()
	root := newReaderTestRepository(t)
	oids := writeBlobs(t, root, []byte("one"), []byte("two"))
	before := ProcessCount()
	for range 3 {
		objects, err := CLI{}.ReadObjects(ctx, root, oids)
		if err != nil {
			t.Fatal(err)
		}
		if string(objects[oids[0]]) != "one" || string(objects[oids[1]]) != "two" {
			t.Fatalf("objects = %q", objects)
		}
	}
	if started := ProcessCount() - before; started != 1 {
		t.Fatalf("three batches started %d processes, want 1", started)
	}
}

func TestReaderMissingObjectKeepsReader(t *testing.T) {
	ctx := context.Background()
	root := newReaderTestRepository(t)
	oids := writeBlobs(t, root, []byte("present"))
	missing := strings.Repeat("0", len(oids[0]))
	if _, err := (CLI{}).ReadObjects(ctx, root, []string{oids[0], missing}); !errors.Is(err, ErrObjectMissing) {
		t.Fatalf("read with missing object: err = %v, want ErrObjectMissing", err)
	}
	before := ProcessCount()
	if _, err := (CLI{}).ReadObjects(ctx, root, oids); err != nil {
		t.Fatal(err)
	}
	if started := ProcessCount() - before; started != 0 {
		t.Fatalf("read after a missing object started %d processes, want 0", started)
	}
}

func TestReaderSeesObjectsWrittenAfterStart(t *testing.T) {
	ctx := context.Background()
	root := newReaderTestRepository(t)
	first := writeBlobs(t, root, []byte("first"))
	if _, err := (CLI{}).ReadObjects(ctx, root, first); err != nil {
		t.Fatal(err)
	}
	later := writeBlobs(t, root, []byte("later"))
	objects, err := CLI{}.ReadObjects(ctx, root, later)
	if err != nil {
		t.Fatal(err)
	}
	if string(objects[later[0]]) != "later" {
		t.Fatalf("object = %q", objects[later[0]])
	}
}

func TestReaderRestartsAfterKill(t *testing.T) {
	ctx := context.Background()
	root := newReaderTestRepository(t)
	oids := writeBlobs(t, root, []byte("value"))
	if _, err := (CLI{}).ReadObjects(ctx, root, oids); err != nil {
		t.Fatal(err)
	}
	reader := readerFor(root)
	reader.mu.Lock()
	if err := reader.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = reader.cmd.Wait()
	reader.mu.Unlock()
	if _, err := (CLI{}).ReadObjects(ctx, root, oids); err != nil {
		t.Fatalf("read after the reader was killed: %v", err)
	}
}

func TestReaderCancelledMidReadThenRecovers(t *testing.T) {
	root := newReaderTestRepository(t)
	big := writeBlobs(t, root, bytes.Repeat([]byte("x"), 8<<20))
	small := writeBlobs(t, root, []byte("small"))
	requests := make([]string, 200)
	for i := range requests {
		requests[i] = big[0]
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if _, err := (CLI{}).ReadObjects(ctx, root, requests); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: err = %v, want context.Canceled", err)
	}
	if readerFor(root).running() {
		t.Fatal("a cancelled read left its process running")
	}
	objects, err := CLI{}.ReadObjects(context.Background(), root, small)
	if err != nil || string(objects[small[0]]) != "small" {
		t.Fatalf("read after cancellation = %q, %v", objects[small[0]], err)
	}
}

func TestReaderClosesWhenIdleAndOnRequest(t *testing.T) {
	saved := readerIdleTimeout
	readerIdleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { readerIdleTimeout = saved })
	ctx := context.Background()
	root := newReaderTestRepository(t)
	oids := writeBlobs(t, root, []byte("value"))
	reader := readerFor(root)

	if _, err := (CLI{}).ReadObjects(ctx, root, oids); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for reader.running() {
		if time.Now().After(deadline) {
			t.Fatal("reader still running after its idle timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}

	readerIdleTimeout = time.Hour
	if _, err := (CLI{}).ReadObjects(ctx, root, oids); err != nil {
		t.Fatal(err)
	}
	if !reader.running() {
		t.Fatal("reader not running after a read")
	}
	CloseReaders(root)
	if reader.running() {
		t.Fatal("CloseReaders left the reader running")
	}
}

func TestReadTreeFileThroughReader(t *testing.T) {
	ctx := context.Background()
	root := newReaderTestRepository(t)
	cli := CLI{}
	oids := writeBlobs(t, root, []byte("manifest"))
	tree, err := cli.WriteTree(ctx, root, filepath.Join(root, ".git"), []TreeEntry{{Path: "manifest.json", ObjectID: oids[0]}})
	if err != nil {
		t.Fatal(err)
	}
	commit, err := cli.CommitTree(ctx, root, tree, "", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	data, err := cli.ReadTreeFile(ctx, root, commit, "manifest.json")
	if err != nil || string(data) != "manifest" {
		t.Fatalf("ReadTreeFile = %q, %v", data, err)
	}
	if _, err := cli.ReadTreeFile(ctx, root, commit, "absent.json"); !errors.Is(err, ErrObjectMissing) {
		t.Fatalf("ReadTreeFile(absent) err = %v, want ErrObjectMissing", err)
	}
	if present, err := cli.HasCommit(ctx, root, commit); err != nil || !present {
		t.Fatalf("HasCommit(commit) = %v, %v", present, err)
	}
	if present, err := cli.HasCommit(ctx, root, oids[0]); err != nil || present {
		t.Fatalf("HasCommit(blob) = %v, %v; a blob is not a commit", present, err)
	}
}
