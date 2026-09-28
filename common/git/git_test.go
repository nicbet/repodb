package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// RepoDB data pushes are internal bookkeeping. They must not run the host
// repository's pre-push hook, which may itself trigger a RepoDB sync.
func TestDataPushesSkipPrePushHook(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	root := filepath.Join(dir, "local")
	remote := filepath.Join(dir, "remote.git")
	marker := filepath.Join(dir, "hook-ran")
	for _, args := range [][]string{
		{"init", "--bare", remote},
		{"init", root},
		{"-C", root, "remote", "add", "origin", remote},
	} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	hook := "#!/bin/sh\necho ran >> '" + filepath.ToSlash(marker) + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(root, ".git", "hooks", "pre-push"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	cli := CLI{}
	commit := func(parent, payload string) string {
		t.Helper()
		blobs, err := cli.HashObjects(ctx, root, filepath.Join(root, ".git", "repodb", "tmp"), [][]byte{[]byte(payload)})
		if err != nil {
			t.Fatal(err)
		}
		tree, err := cli.WriteTree(ctx, root, filepath.Join(root, ".git"), []TreeEntry{{Path: "value", ObjectID: blobs[0]}})
		if err != nil {
			t.Fatal(err)
		}
		oid, err := cli.CommitTree(ctx, root, tree, parent, payload)
		if err != nil {
			t.Fatal(err)
		}
		return oid
	}
	const ref = "refs/repodb/data"
	assertRemote := func(want string) {
		t.Helper()
		got, err := cli.RemoteRef(ctx, root, "origin", ref)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("remote %s = %s, want %s", ref, got, want)
		}
	}

	first := commit("", "first")
	if err := cli.UpdateRef(ctx, root, ref, first, ""); err != nil {
		t.Fatal(err)
	}
	if err := cli.PushRef(ctx, root, "origin", ref, ref); err != nil {
		t.Fatalf("PushRef: %v", err)
	}
	assertRemote(first)

	second := commit(first, "second")
	if err := cli.PushCommit(ctx, root, "origin", second, ref); err != nil {
		t.Fatalf("PushCommit: %v", err)
	}
	assertRemote(second)

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("pre-push hook ran during a RepoDB data push (stat err: %v)", err)
	}
}
