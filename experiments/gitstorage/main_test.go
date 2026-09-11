package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestM0GitStorageExperiment(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := filepath.Join(t.TempDir(), "repodb-m0")
	var output bytes.Buffer
	if err := run(root, &output); err != nil {
		t.Fatalf("experiment failed: %v\n%s", err, output.String())
	}
}
