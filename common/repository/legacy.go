package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	repodbgit "github.com/nicbet/repodb/common/git"
	"github.com/nicbet/repodb/common/storage"
)

type legacyConfig struct {
	FormatVersion int    `json:"format_version"`
	DefaultDB     string `json:"default_database"`
}

type legacyManifest struct {
	FormatVersion int              `json:"format_version"`
	Tables        map[string]Table `json:"tables"`
}

// ImportLegacy publishes the tracked .repodb prototype layout to DataRef. It
// deliberately leaves every legacy file untouched for review or manual removal.
func ImportLegacy(ctx context.Context, start string) (*Repository, *Snapshot, error) {
	repo, err := discover(ctx, start)
	if err != nil {
		return nil, nil, err
	}
	if _, err := repo.git.ResolveRef(ctx, repo.Root, DataRef); err == nil {
		return nil, nil, errors.New("RepoDB data ref already exists; legacy import requires an uninitialized repository")
	} else if !errors.Is(err, repodbgit.ErrRefNotFound) {
		return nil, nil, err
	}
	var config legacyConfig
	if err := readLegacyJSON(filepath.Join(repo.Dir, "config.json"), &config); err != nil {
		return nil, nil, fmt.Errorf("read legacy config: %w", err)
	}
	var oldManifest legacyManifest
	if err := readLegacyJSON(filepath.Join(repo.Dir, "manifest.json"), &oldManifest); err != nil {
		return nil, nil, fmt.Errorf("read legacy manifest: %w", err)
	}
	if config.FormatVersion != FormatVersion || oldManifest.FormatVersion != FormatVersion {
		return nil, nil, fmt.Errorf("unsupported legacy RepoDB format (config %d, manifest %d)", config.FormatVersion, oldManifest.FormatVersion)
	}
	objects := make(map[storage.Hash][]byte)
	objectRoot := filepath.Join(repo.Dir, "objects", "sha256")
	if err := filepath.WalkDir(objectRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(objectRoot, path)
		if err != nil {
			return err
		}
		hash := storage.Hash(strings.ReplaceAll(relative, string(filepath.Separator), ""))
		if !hash.Valid() {
			return fmt.Errorf("invalid legacy object path %s", relative)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if storage.Sum(data) != hash {
			return fmt.Errorf("legacy object %s failed integrity check", hash)
		}
		objects[hash] = data
		return nil
	}); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	w := &Writer{repo: repo, objects: objects}
	snapshot, err := w.Commit(ctx, Manifest{
		DefaultDatabase: config.DefaultDB,
		Tables:          oldManifest.Tables,
	})
	if err != nil {
		return nil, nil, err
	}
	return repo, snapshot, nil
}

func readLegacyJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}
