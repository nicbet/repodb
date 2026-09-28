package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestFilesystemRoundTrip(t *testing.T) {
	ctx := context.Background()
	fs := NewFilesystem(t.TempDir())
	data := []byte("round trip")
	hash, err := fs.Put(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fs.Get(ctx, hash)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get = %q, %v; want %q", got, err, data)
	}
}

func TestFilesystemConcurrentPutSameObject(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fs := NewFilesystem(root)
	data := []byte("shared object")
	want := Sum(data)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hash, err := fs.Put(ctx, data)
			if err == nil && hash != want {
				err = errors.New("hash mismatch: " + string(hash))
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fs.Get(ctx, want); err != nil {
		t.Fatal(err)
	}
	assertNoTempFiles(t, root)
}

func TestFilesystemPutLostRaceSucceeds(t *testing.T) {
	root := t.TempDir()
	fs := NewFilesystem(root)
	data := []byte("raced object")
	defer func(orig func(string, string) error) { rename = orig }(rename)
	rename = func(oldpath, newpath string) error {
		// Another writer lands the object, then our rename fails.
		if err := os.WriteFile(newpath, data, 0o644); err != nil {
			return err
		}
		return errors.New("access denied")
	}
	hash, err := fs.Put(context.Background(), data)
	if err != nil || hash != Sum(data) {
		t.Fatalf("Put = %s, %v; want %s, nil", hash, err, Sum(data))
	}
	assertNoTempFiles(t, root)
}

func TestFilesystemPutRenameFailure(t *testing.T) {
	root := t.TempDir()
	fs := NewFilesystem(root)
	failure := errors.New("disk on fire")
	defer func(orig func(string, string) error) { rename = orig }(rename)
	rename = func(string, string) error { return failure }
	if _, err := fs.Put(context.Background(), []byte("doomed")); !errors.Is(err, failure) {
		t.Fatalf("Put err = %v; want %v", err, failure)
	}
	assertNoTempFiles(t, root)
}

func TestFilesystemObjectsReadOnlyOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("objects are left writable on Windows")
	}
	root := t.TempDir()
	fs := NewFilesystem(root)
	hash, err := fs.Put(context.Background(), []byte("immutable"))
	if err != nil {
		t.Fatal(err)
	}
	path, _ := fs.path(hash)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o444 {
		t.Fatalf("object mode = %o; want 444", mode)
	}
}

func assertNoTempFiles(t *testing.T, root string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, "*", ".repodb-object-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) > 0 {
		t.Fatalf("leftover temp files: %v", matches)
	}
}
