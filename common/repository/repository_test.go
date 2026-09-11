package repository_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nicbet/repodb/common/repository"
)

func TestInitCreatesTrackedLayout(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("git", "init", "--quiet")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	repo, err := repository.Init(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	gotInfo, err := os.Stat(repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	wantInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(gotInfo, wantInfo) {
		t.Fatalf("root = %q, want same directory as %q", repo.Root, root)
	}
	for _, name := range []string{"config.json", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(root, ".repodb", name)); err != nil {
			t.Errorf("%s was not created: %v", name, err)
		}
	}
	manifest, err := repo.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.FormatVersion != repository.FormatVersion || manifest.Tables == nil {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}
}
