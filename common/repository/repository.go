// Package repository defines the on-disk, Git-tracked RepoDB layout.
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	repodbgit "github.com/nicbet/repodb/common/git"
	"github.com/nicbet/repodb/common/storage"
)

const FormatVersion = 1

type Config struct {
	FormatVersion int    `json:"format_version"`
	DefaultDB     string `json:"default_database"`
}

type Table struct {
	SchemaRoot storage.Hash `json:"schema_root,omitempty"`
	DataRoot   storage.Hash `json:"data_root,omitempty"`
}

type Manifest struct {
	FormatVersion int              `json:"format_version"`
	UpdatedAt     time.Time        `json:"updated_at"`
	Tables        map[string]Table `json:"tables"`
}

type Repository struct {
	Root string
	Dir  string
}

func Init(ctx context.Context, start string) (*Repository, error) {
	root, err := (repodbgit.CLI{}).TopLevel(ctx, start)
	if err != nil {
		return nil, err
	}
	repo := &Repository{Root: root, Dir: filepath.Join(root, ".repodb")}
	if err := os.MkdirAll(filepath.Join(repo.Dir, "objects", "sha256"), 0o755); err != nil {
		return nil, err
	}
	if err := writeJSONIfMissing(filepath.Join(repo.Dir, "config.json"), Config{
		FormatVersion: FormatVersion,
		DefaultDB:     "repodb",
	}); err != nil {
		return nil, err
	}
	if err := writeJSONIfMissing(filepath.Join(repo.Dir, "manifest.json"), Manifest{
		FormatVersion: FormatVersion,
		UpdatedAt:     time.Now().UTC(),
		Tables:        map[string]Table{},
	}); err != nil {
		return nil, err
	}
	return repo, nil
}

func Open(ctx context.Context, start string) (*Repository, error) {
	root, err := (repodbgit.CLI{}).TopLevel(ctx, start)
	if err != nil {
		return nil, err
	}
	repo := &Repository{Root: root, Dir: filepath.Join(root, ".repodb")}
	var config Config
	if err := readJSON(filepath.Join(repo.Dir, "config.json"), &config); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("RepoDB is not initialized; run repodb init")
		}
		return nil, err
	}
	if config.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("unsupported RepoDB format %d", config.FormatVersion)
	}
	return repo, nil
}

func (r *Repository) Store() storage.Store {
	return storage.NewFilesystem(filepath.Join(r.Dir, "objects", "sha256"))
}

func (r *Repository) LoadManifest() (Manifest, error) {
	var manifest Manifest
	err := readJSON(filepath.Join(r.Dir, "manifest.json"), &manifest)
	return manifest, err
}

func (r *Repository) SaveManifest(manifest Manifest) error {
	manifest.FormatVersion = FormatVersion
	manifest.UpdatedAt = time.Now().UTC()
	if manifest.Tables == nil {
		manifest.Tables = map[string]Table{}
	}
	return writeJSON(filepath.Join(r.Dir, "manifest.json"), manifest)
}

func writeJSONIfMissing(path string, value any) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeJSON(path, value)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".repodb-manifest-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}
