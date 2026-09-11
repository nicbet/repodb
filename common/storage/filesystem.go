package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Filesystem stores immutable objects beneath a Git-tracked directory. Object
// paths are split by their first two hex digits, like loose Git objects.
type Filesystem struct {
	root string
}

func NewFilesystem(root string) *Filesystem {
	return &Filesystem{root: root}
}

func (f *Filesystem) Get(_ context.Context, hash Hash) ([]byte, error) {
	path, err := f.path(hash)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if Sum(data) != hash {
		return nil, fmt.Errorf("object %s failed integrity check", hash)
	}
	return data, nil
}

func (f *Filesystem) Put(_ context.Context, data []byte) (Hash, error) {
	hash := Sum(data)
	path, err := f.path(hash)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err == nil {
		return hash, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".repodb-object-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o444); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", err
	}
	return hash, nil
}

func (f *Filesystem) path(hash Hash) (string, error) {
	if !hash.Valid() {
		return "", fmt.Errorf("invalid object hash %q", hash)
	}
	return filepath.Join(f.root, string(hash[:2]), string(hash[2:])), nil
}
