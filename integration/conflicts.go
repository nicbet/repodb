package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

var ErrNoConflicts = errors.New("no persisted RepoDB conflicts")

type ConflictSet struct {
	Version     int                          `json:"version"`
	Remote      string                       `json:"remote"`
	BaseHead    string                       `json:"base_head"`
	LocalHead   string                       `json:"local_head"`
	RemoteHead  string                       `json:"remote_head"`
	Conflicts   []engine.MergeConflict       `json:"conflicts"`
	Resolutions map[string]engine.Resolution `json:"resolutions,omitempty"`
}

type MergeConflictError struct {
	Set ConflictSet
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("RepoDB merge has %d unresolved conflict(s); inspect with 'repodb conflicts --remote %s' and resolve with 'repodb resolve --remote %s --id <id> --take <local|remote|base|delete>'", len(e.Set.Unresolved()), e.Set.Remote, e.Set.Remote)
}

func (s ConflictSet) Unresolved() []engine.MergeConflict {
	out := make([]engine.MergeConflict, 0, len(s.Conflicts))
	for _, conflict := range s.Conflicts {
		if _, ok := s.Resolutions[conflict.ID]; !ok {
			out = append(out, conflict)
		}
	}
	return out
}

func Conflicts(ctx context.Context, start, remote string) (ConflictSet, error) {
	_, info, remote, _, err := resolveRemote(ctx, start, remote)
	if err != nil {
		return ConflictSet{}, err
	}
	repo, err := repository.Discover(ctx, info.TopLevel)
	if err != nil {
		return ConflictSet{}, err
	}
	return loadConflictSet(repo, remote)
}

// Resolve records one durable whole-row or whole-schema choice. Once all
// conflicts have choices, it resumes synchronization using the recorded heads.
func Resolve(ctx context.Context, start, remote, id string, choice engine.Resolution) (Status, error) {
	_, info, remote, _, err := resolveRemote(ctx, start, remote)
	if err != nil {
		return Status{}, err
	}
	if choice != engine.TakeBase && choice != engine.TakeLocal && choice != engine.TakeRemote && choice != engine.TakeDelete {
		return Status{}, fmt.Errorf("invalid resolution %q", choice)
	}
	repo, err := repository.Discover(ctx, info.TopLevel)
	if err != nil {
		return Status{}, err
	}
	set, err := loadConflictSet(repo, remote)
	if err != nil {
		return Status{}, err
	}
	found := false
	for _, conflict := range set.Conflicts {
		if conflict.ID == id {
			found = true
			break
		}
	}
	if !found {
		return Status{}, fmt.Errorf("unknown conflict %q", id)
	}
	if set.Resolutions == nil {
		set.Resolutions = map[string]engine.Resolution{}
	}
	set.Resolutions[id] = choice
	if err := saveConflictSet(repo, set); err != nil {
		return Status{}, err
	}
	if len(set.Unresolved()) != 0 {
		return Status{Remote: remote, TrackingRef: mustTracking(remote), LocalHead: set.LocalHead, RemoteHead: set.RemoteHead, Action: "conflicts-pending"}, &MergeConflictError{Set: set}
	}
	return Sync(ctx, start, remote)
}

func conflictPath(repo *repository.Repository, remote string) string {
	return filepath.Join(repo.CommonDir, "repodb", "conflicts", remote+".json")
}

func loadConflictSet(repo *repository.Repository, remote string) (ConflictSet, error) {
	data, err := os.ReadFile(conflictPath(repo, remote))
	if errors.Is(err, os.ErrNotExist) {
		return ConflictSet{}, ErrNoConflicts
	}
	if err != nil {
		return ConflictSet{}, err
	}
	var set ConflictSet
	if err := json.Unmarshal(data, &set); err != nil {
		return ConflictSet{}, fmt.Errorf("decode persisted conflicts: %w", err)
	}
	if set.Version != 1 || set.Remote != remote {
		return ConflictSet{}, errors.New("invalid persisted RepoDB conflict record")
	}
	if set.Resolutions == nil {
		set.Resolutions = map[string]engine.Resolution{}
	}
	return set, nil
}

func saveConflictSet(repo *repository.Repository, set ConflictSet) error {
	path := conflictPath(repo, set.Remote)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".conflicts-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
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
	return os.Rename(name, path)
}

func clearConflictSet(repo *repository.Repository, remote string) error {
	err := os.Remove(conflictPath(repo, remote))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func mustTracking(remote string) string { ref, _ := TrackingRef(remote); return ref }
