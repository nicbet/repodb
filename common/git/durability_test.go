package git

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDurabilityAuditSeparatesCommandsAndHardensReference(t *testing.T) {
	for _, method := range []FsyncMethod{FsyncMethodFsync, FsyncMethodBatch} {
		t.Run(string(method), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "repo.git")
			if output, err := exec.Command("git", "init", "--bare", root).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, output)
			}
			ctx, audit, err := WithDurabilityAudit(context.Background(), root, method)
			if err != nil {
				t.Fatal(err)
			}
			cli := CLI{}
			blobs, err := cli.HashObjects(ctx, root, filepath.Join(root, "repodb", "tmp"), [][]byte{[]byte("durable payload one"), []byte("durable payload two")})
			if err != nil {
				t.Fatal(err)
			}
			tree, err := cli.WriteTree(ctx, root, root, []TreeEntry{{Path: "objects/sha256/aa/value", ObjectID: blobs[0]}})
			if err != nil {
				t.Fatal(err)
			}
			commit, err := cli.CommitTree(ctx, root, tree, "", "durability audit")
			if err != nil {
				t.Fatal(err)
			}
			if err := cli.UpdateRef(ctx, root, "refs/repodb/data", commit, ""); err != nil {
				t.Fatal(err)
			}
			metrics := audit.Metrics()
			for name, metric := range map[string]DurabilityCommandMetrics{
				"hash-object": metrics.HashObject,
				"write-tree":  metrics.WriteTree,
				"commit-tree": metrics.CommitTree,
				"update-ref":  metrics.UpdateRef,
			} {
				if metric.Invocations != 1 {
					t.Fatalf("%s invocations = %d, want 1", name, metric.Invocations)
				}
				if metric.HardwareFlushes == 0 {
					t.Fatalf("%s did not report a hardware flush", name)
				}
			}
			if metrics.HashObject.ObjectsCreated != 2 || metrics.CommitTree.ObjectsCreated != 1 {
				t.Fatalf("unexpected object accounting: %+v", metrics)
			}
			if metrics.WriteTree.ObjectsCreated < 3 {
				t.Fatalf("nested path created only %d tree objects", metrics.WriteTree.ObjectsCreated)
			}
			if metrics.HashObject.HardwareFlushes != metrics.HashObject.ObjectsCreated || metrics.HashObject.WriteoutRequests != 0 {
				t.Fatalf("hash-object did not use one full flush per new blob: %+v", metrics.HashObject)
			}
			if method == FsyncMethodFsync {
				if metrics.WriteTree.HardwareFlushes != metrics.WriteTree.ObjectsCreated || metrics.WriteTree.WriteoutRequests != 0 {
					t.Fatalf("fsync write-tree counters = %+v", metrics.WriteTree)
				}
			} else if metrics.WriteTree.HardwareFlushes != 1 || metrics.WriteTree.WriteoutRequests != metrics.WriteTree.ObjectsCreated {
				t.Fatalf("batch write-tree counters = %+v", metrics.WriteTree)
			}
			if metrics.CommitTree.HardwareFlushes != 1 || metrics.UpdateRef.HardwareFlushes != 1 {
				t.Fatalf("commit/ref hardening missing: commit=%+v ref=%+v", metrics.CommitTree, metrics.UpdateRef)
			}
		})
	}
}
