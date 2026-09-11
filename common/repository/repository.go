// Package repository stores RepoDB snapshots in Git objects reachable from a
// dedicated ref. Source worktrees, indexes, and branches are never involved.
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	repodbgit "github.com/nicbet/repodb/common/git"
	"github.com/nicbet/repodb/common/storage"
	"golang.org/x/sys/unix"
)

const (
	FormatVersion = 1
	DataRef       = "refs/repodb/data"
)

var (
	ErrNotInitialized = errors.New("RepoDB is not initialized")
	ErrLegacyLayout   = errors.New("legacy tracked .repodb layout found")
	ErrConflict       = errors.New("RepoDB data head changed")
	ErrCorrupt        = errors.New("corrupt RepoDB snapshot")
	ErrCommitUnknown  = errors.New("RepoDB commit outcome is unknown")
)

type CommitOutcome uint8

const (
	OutcomeRejected CommitOutcome = iota
	OutcomeCommitted
	OutcomeUnknown
)

type CommitResult struct {
	Outcome  CommitOutcome
	Commit   string
	Snapshot *Snapshot
}

type CommitError struct {
	Outcome CommitOutcome
	Commit  string
	Err     error
}

func (e *CommitError) Error() string { return e.Err.Error() }
func (e *CommitError) Unwrap() error { return e.Err }

type Table struct {
	SchemaRoot storage.Hash `json:"schema_root,omitempty"`
	DataRoot   storage.Hash `json:"data_root,omitempty"`
}

// Manifest is the authoritative catalog stored in every data commit. Objects is
// a complete, sorted inventory of the RepoDB-addressed blobs in the same tree.
type Manifest struct {
	FormatVersion   int              `json:"format_version"`
	DefaultDatabase string           `json:"default_database"`
	Tables          map[string]Table `json:"tables"`
	Objects         []storage.Hash   `json:"objects"`
}

type Repository struct {
	Root         string
	Dir          string // Legacy tracked layout; retained only for explicit import.
	CommonDir    string
	ObjectFormat string
	git          repodbgit.CLI
	fault        func(PublicationPoint) error
}

type PublicationPoint string

const (
	BeforeRefPublication PublicationPoint = "before-ref-publication"
	DuringRefPublication PublicationPoint = "during-ref-publication"
	AfterRefPublication  PublicationPoint = "after-ref-publication"
)

// SetPublicationFaultInjector installs a deterministic fault hook for tests.
// Passing nil disables it.
func (r *Repository) SetPublicationFaultInjector(inject func(PublicationPoint) error) {
	r.fault = inject
}

type Snapshot struct {
	repo       *Repository
	Commit     string
	Manifest   Manifest
	objectSet  map[storage.Hash]struct{}
	objectOIDs map[storage.Hash]string
	objectData map[storage.Hash][]byte
	mu         sync.RWMutex
}

type Writer struct {
	repo      *Repository
	base      *Snapshot
	expected  string
	objects   map[storage.Hash][]byte
	retained  map[storage.Hash]struct{}
	committed bool
	mu        sync.RWMutex
}

// Init creates the initial empty catalog commit. It is idempotent when the data
// ref already contains a valid snapshot and refuses to hide a legacy layout.
func Init(ctx context.Context, start string) (*Repository, error) {
	repo, err := discover(ctx, start)
	if err != nil {
		return nil, err
	}
	if _, err := repo.git.ResolveRef(ctx, repo.Root, DataRef); err == nil {
		if _, err := repo.Current(ctx); err != nil {
			return nil, err
		}
		return repo, nil
	} else if !errors.Is(err, repodbgit.ErrRefNotFound) {
		return nil, err
	}
	if legacyExists(repo.Dir) {
		return nil, fmt.Errorf("%w; run repodb import-legacy or move it aside before initialization", ErrLegacyLayout)
	}
	w := &Writer{repo: repo, objects: make(map[storage.Hash][]byte)}
	if _, err := w.Commit(ctx, Manifest{DefaultDatabase: "repodb", Tables: map[string]Table{}}); err != nil {
		if errors.Is(err, ErrConflict) {
			if _, openErr := repo.Current(ctx); openErr == nil {
				return repo, nil
			}
		}
		return nil, err
	}
	return repo, nil
}

func Open(ctx context.Context, start string) (*Repository, error) {
	repo, err := discover(ctx, start)
	if err != nil {
		return nil, err
	}
	if _, err := repo.git.ResolveRef(ctx, repo.Root, DataRef); err != nil {
		if errors.Is(err, repodbgit.ErrRefNotFound) {
			if legacyExists(repo.Dir) {
				return nil, fmt.Errorf("%w; run repodb import-legacy", ErrLegacyLayout)
			}
			return nil, ErrNotInitialized
		}
		return nil, err
	}
	if _, err := repo.Current(ctx); err != nil {
		return nil, err
	}
	return repo, nil
}

func discover(ctx context.Context, start string) (*Repository, error) {
	cli := repodbgit.CLI{}
	info, err := cli.Discover(ctx, start)
	if err != nil {
		return nil, err
	}
	if info.ObjectFormat != "sha1" && info.ObjectFormat != "sha256" {
		return nil, fmt.Errorf("unsupported Git object format %q", info.ObjectFormat)
	}
	return &Repository{
		Root:         info.TopLevel,
		Dir:          filepath.Join(info.TopLevel, ".repodb"),
		CommonDir:    info.CommonDir,
		ObjectFormat: info.ObjectFormat,
		git:          cli,
	}, nil
}

func (r *Repository) CacheDir() string {
	return filepath.Join(r.CommonDir, "repodb", "cache", fmt.Sprintf("v%d", FormatVersion))
}

func (r *Repository) Current(ctx context.Context) (*Snapshot, error) {
	commit, err := r.git.ResolveRef(ctx, r.Root, DataRef)
	if errors.Is(err, repodbgit.ErrRefNotFound) {
		return nil, ErrNotInitialized
	}
	if err != nil {
		return nil, err
	}
	return r.loadSnapshot(ctx, commit)
}

// Begin pins the current immutable snapshot. Reads through the writer see that
// snapshot plus blobs added to the writer.
func (r *Repository) Begin(ctx context.Context) (*Writer, error) {
	base, err := r.Current(ctx)
	if err != nil {
		return nil, err
	}
	return &Writer{
		repo:     r,
		base:     base,
		expected: base.Commit,
		objects:  make(map[storage.Hash][]byte),
	}, nil
}

func (s *Snapshot) Store() storage.Store { return &snapshotStore{snapshot: s} }

func (w *Writer) BaseSnapshot() *Snapshot { return w.base }

func (w *Writer) Put(_ context.Context, data []byte) (storage.Hash, error) {
	hash := storage.Sum(data)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed {
		return "", errors.New("snapshot writer is already committed")
	}
	if _, exists := w.objects[hash]; !exists {
		w.objects[hash] = append([]byte(nil), data...)
	}
	return hash, nil
}

func (w *Writer) Get(ctx context.Context, hash storage.Hash) ([]byte, error) {
	if !hash.Valid() {
		return nil, fmt.Errorf("invalid object hash %q", hash)
	}
	w.mu.RLock()
	data, exists := w.objects[hash]
	w.mu.RUnlock()
	if exists {
		return append([]byte(nil), data...), nil
	}
	if w.base == nil {
		return nil, storage.ErrNotFound
	}
	return w.base.Store().Get(ctx, hash)
}

// RetainOnly bounds the next snapshot inventory to the supplied object graph.
// Callers must include schema roots, data roots, and every descendant.
func (w *Writer) RetainOnly(hashes []storage.Hash) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed {
		return errors.New("snapshot writer is already committed")
	}
	w.retained = make(map[storage.Hash]struct{}, len(hashes))
	for _, hash := range hashes {
		if !hash.Valid() {
			return fmt.Errorf("invalid retained object hash %q", hash)
		}
		w.retained[hash] = struct{}{}
	}
	return nil
}

// Commit writes a complete Git tree and commit before atomically advancing the
// live data ref from the writer's expected head.
func (w *Writer) Commit(ctx context.Context, manifest Manifest) (*Snapshot, error) {
	result, err := w.CommitWithOutcome(ctx, manifest)
	return result.Snapshot, err
}

func (w *Writer) CommitWithOutcome(ctx context.Context, manifest Manifest) (CommitResult, error) {
	result := CommitResult{Outcome: OutcomeRejected}
	w.mu.Lock()
	if w.committed {
		w.mu.Unlock()
		return result, errors.New("snapshot writer is already committed")
	}
	w.committed = true
	newObjects := cloneObjects(w.objects)
	retained := w.retained
	w.mu.Unlock()

	all := make(map[storage.Hash][]byte)
	if w.base != nil {
		for _, hash := range w.base.Manifest.Objects {
			if retained != nil {
				if _, ok := retained[hash]; !ok {
					continue
				}
			}
			data, err := w.base.Store().Get(ctx, hash)
			if err != nil {
				return result, err
			}
			all[hash] = data
		}
	}
	for hash, data := range newObjects {
		if retained != nil {
			if _, ok := retained[hash]; !ok {
				continue
			}
		}
		all[hash] = data
	}
	if retained != nil && len(all) != len(retained) {
		return result, fmt.Errorf("retained object graph contains %d unavailable objects", len(retained)-len(all))
	}
	manifest.FormatVersion = FormatVersion
	if manifest.DefaultDatabase == "" {
		manifest.DefaultDatabase = "repodb"
	}
	if manifest.Tables == nil {
		manifest.Tables = map[string]Table{}
	}
	manifest.Objects = sortedHashes(all)
	if err := validateManifest(manifest, all); err != nil {
		return result, err
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return result, err
	}
	manifestData = append(manifestData, '\n')

	release, err := w.repo.lock(ctx)
	if err != nil {
		return result, err
	}
	defer release()

	entries := make([]repodbgit.TreeEntry, 0, len(all)+1)
	manifestOID, err := w.repo.git.HashObject(ctx, w.repo.Root, manifestData)
	if err != nil {
		return result, err
	}
	entries = append(entries, repodbgit.TreeEntry{Path: "manifest.json", ObjectID: manifestOID})
	for _, hash := range manifest.Objects {
		oid := ""
		if w.base != nil {
			oid = w.base.objectOIDs[hash]
		}
		if oid == "" {
			oid, err = w.repo.git.HashObject(ctx, w.repo.Root, all[hash])
			if err != nil {
				return result, err
			}
		}
		entries = append(entries, repodbgit.TreeEntry{Path: objectPath(hash), ObjectID: oid})
	}
	tree, err := w.repo.git.WriteTree(ctx, w.repo.Root, w.repo.CommonDir, entries)
	if err != nil {
		return result, err
	}
	commit, err := w.repo.git.CommitTree(ctx, w.repo.Root, tree, w.expected, fmt.Sprintf("RepoDB snapshot v%d", FormatVersion))
	if err != nil {
		return result, err
	}
	result.Commit = commit
	if w.repo.fault != nil {
		if err := w.repo.fault(BeforeRefPublication); err != nil {
			return result, err
		}
		if err := w.repo.fault(DuringRefPublication); err != nil {
			result.Outcome = OutcomeUnknown
			return result, &CommitError{Outcome: OutcomeUnknown, Commit: commit, Err: fmt.Errorf("%w: candidate %s: %v", ErrCommitUnknown, commit, err)}
		}
	}
	if err := w.repo.git.UpdateRef(ctx, w.repo.Root, DataRef, commit, w.expected); err != nil {
		result = w.repo.resolvePublication(commit, w.expected)
		if result.Outcome == OutcomeRejected {
			actual, resolveErr := w.repo.git.ResolveRef(context.Background(), w.repo.Root, DataRef)
			if resolveErr == nil && actual != w.expected {
				return result, ErrConflict
			}
			return result, err
		}
		if result.Outcome == OutcomeUnknown {
			return result, &CommitError{Outcome: OutcomeUnknown, Commit: commit, Err: fmt.Errorf("%w: candidate %s: %v", ErrCommitUnknown, commit, err)}
		}
	}
	result = CommitResult{Outcome: OutcomeCommitted, Commit: commit}
	if w.repo.fault != nil {
		if err := w.repo.fault(AfterRefPublication); err != nil {
			return result, &CommitError{Outcome: OutcomeCommitted, Commit: commit, Err: err}
		}
	}
	snapshot, err := w.repo.loadSnapshot(context.WithoutCancel(ctx), commit)
	result.Snapshot = snapshot
	if err != nil {
		return result, &CommitError{Outcome: OutcomeCommitted, Commit: commit, Err: fmt.Errorf("published %s but verification failed: %w", commit, err)}
	}
	return result, nil
}

func (r *Repository) resolvePublication(candidate, expected string) CommitResult {
	actual, err := r.git.ResolveRef(context.Background(), r.Root, DataRef)
	if err != nil {
		return CommitResult{Outcome: OutcomeUnknown, Commit: candidate}
	}
	if actual == candidate {
		return CommitResult{Outcome: OutcomeCommitted, Commit: candidate}
	}
	if actual != expected {
		return CommitResult{Outcome: OutcomeRejected, Commit: candidate}
	}
	return CommitResult{Outcome: OutcomeRejected, Commit: candidate}
}

// RecoverCommit resolves a candidate returned with an uncertain outcome.
func (r *Repository) RecoverCommit(ctx context.Context, candidate string) (CommitResult, error) {
	actual, err := r.git.ResolveRef(ctx, r.Root, DataRef)
	if err != nil {
		return CommitResult{Outcome: OutcomeUnknown, Commit: candidate}, err
	}
	if actual != candidate {
		published, ancestorErr := r.git.IsAncestor(ctx, r.Root, candidate, actual)
		if ancestorErr != nil {
			return CommitResult{Outcome: OutcomeUnknown, Commit: candidate}, ancestorErr
		}
		if !published {
			return CommitResult{Outcome: OutcomeRejected, Commit: candidate}, nil
		}
	}
	snapshot, err := r.loadSnapshot(ctx, candidate)
	return CommitResult{Outcome: OutcomeCommitted, Commit: candidate, Snapshot: snapshot}, err
}

func (r *Repository) loadSnapshot(ctx context.Context, commit string) (*Snapshot, error) {
	data, err := r.git.ReadTreeFile(ctx, r.Root, commit, "manifest.json")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	var manifest Manifest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("%w: decode manifest: %v", ErrCorrupt, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing manifest content", ErrCorrupt)
	}
	if manifest.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("unsupported RepoDB format %d (supported: %d)", manifest.FormatVersion, FormatVersion)
	}
	objectSet := make(map[storage.Hash]struct{}, len(manifest.Objects))
	for i, hash := range manifest.Objects {
		if !hash.Valid() {
			return nil, fmt.Errorf("%w: invalid object hash %q", ErrCorrupt, hash)
		}
		if i > 0 && manifest.Objects[i-1] >= hash {
			return nil, fmt.Errorf("%w: object inventory is not sorted and unique", ErrCorrupt)
		}
		objectSet[hash] = struct{}{}
	}
	snapshot := &Snapshot{repo: r, Commit: commit, Manifest: manifest, objectSet: objectSet, objectOIDs: make(map[storage.Hash]string), objectData: make(map[storage.Hash][]byte)}
	if err := validateManifestInventory(manifest, objectSet); err != nil {
		return nil, err
	}
	objects, err := r.git.ListTreeObjects(ctx, r.Root, commit)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	expectedPaths := make(map[string]struct{}, len(objectSet)+1)
	expectedPaths["manifest.json"] = struct{}{}
	for hash := range objectSet {
		expectedPaths[objectPath(hash)] = struct{}{}
	}
	for path, oid := range objects {
		if _, ok := expectedPaths[path]; !ok {
			return nil, fmt.Errorf("%w: unlisted tree entry %s", ErrCorrupt, path)
		}
		if strings.HasPrefix(path, "objects/sha256/") {
			hash := storage.Hash(strings.ReplaceAll(strings.TrimPrefix(path, "objects/sha256/"), "/", ""))
			if hash.Valid() {
				snapshot.objectOIDs[hash] = oid
			}
		}
		delete(expectedPaths, path)
	}
	if len(expectedPaths) != 0 {
		return nil, fmt.Errorf("%w: snapshot is missing %d listed tree entries", ErrCorrupt, len(expectedPaths))
	}
	oids := make([]string, 0, len(snapshot.objectOIDs))
	for _, oid := range snapshot.objectOIDs {
		oids = append(oids, oid)
	}
	objectData, err := r.git.ReadObjects(ctx, r.Root, oids)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	for hash := range objectSet {
		data, ok := objectData[snapshot.objectOIDs[hash]]
		if !ok || storage.Sum(data) != hash {
			return nil, fmt.Errorf("%w: object %s failed integrity check", ErrCorrupt, hash)
		}
		snapshot.objectData[hash] = data
	}
	return snapshot, nil
}

type snapshotStore struct{ snapshot *Snapshot }

func (s *snapshotStore) Get(ctx context.Context, hash storage.Hash) ([]byte, error) {
	if !hash.Valid() {
		return nil, fmt.Errorf("invalid object hash %q", hash)
	}
	if _, ok := s.snapshot.objectSet[hash]; !ok {
		return nil, storage.ErrNotFound
	}
	s.snapshot.mu.RLock()
	cached, ok := s.snapshot.objectData[hash]
	s.snapshot.mu.RUnlock()
	if ok {
		return append([]byte(nil), cached...), nil
	}
	data, err := s.snapshot.repo.git.ReadTreeFile(ctx, s.snapshot.repo.Root, s.snapshot.Commit, objectPath(hash))
	if err != nil {
		return nil, err
	}
	if storage.Sum(data) != hash {
		return nil, fmt.Errorf("object %s failed integrity check", hash)
	}
	s.snapshot.mu.Lock()
	s.snapshot.objectData[hash] = append([]byte(nil), data...)
	s.snapshot.mu.Unlock()
	return data, nil
}

func (*snapshotStore) Put(context.Context, []byte) (storage.Hash, error) {
	return "", errors.New("snapshot store is read-only")
}

func validateManifest(manifest Manifest, objects map[storage.Hash][]byte) error {
	set := make(map[storage.Hash]struct{}, len(objects))
	for hash, data := range objects {
		if !hash.Valid() || storage.Sum(data) != hash {
			return fmt.Errorf("object %q failed integrity check", hash)
		}
		set[hash] = struct{}{}
	}
	return validateManifestInventory(manifest, set)
}

func validateManifestInventory(manifest Manifest, objects map[storage.Hash]struct{}) error {
	if manifest.DefaultDatabase == "" {
		return fmt.Errorf("%w: default database is empty", ErrCorrupt)
	}
	if manifest.Tables == nil {
		return fmt.Errorf("%w: tables map is absent", ErrCorrupt)
	}
	for name, table := range manifest.Tables {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("%w: table name is empty", ErrCorrupt)
		}
		for kind, hash := range map[string]storage.Hash{"schema": table.SchemaRoot, "data": table.DataRoot} {
			if hash == "" {
				continue
			}
			if _, ok := objects[hash]; !ok {
				return fmt.Errorf("%w: table %q %s root %s is not in the object inventory", ErrCorrupt, name, kind, hash)
			}
		}
	}
	return nil
}

func sortedHashes(objects map[storage.Hash][]byte) []storage.Hash {
	hashes := make([]storage.Hash, 0, len(objects))
	for hash := range objects {
		hashes = append(hashes, hash)
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i] < hashes[j] })
	return hashes
}

func cloneObjects(objects map[storage.Hash][]byte) map[storage.Hash][]byte {
	cloned := make(map[storage.Hash][]byte, len(objects))
	for hash, data := range objects {
		cloned[hash] = append([]byte(nil), data...)
	}
	return cloned
}

func objectPath(hash storage.Hash) string {
	return "objects/sha256/" + string(hash[:2]) + "/" + string(hash[2:])
}

func (r *Repository) lock(ctx context.Context) (func(), error) {
	lockDir := filepath.Join(r.CommonDir, "repodb", "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(lockDir, "publish.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return func() {
				_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
				_ = file.Close()
			}, nil
		} else if !errors.Is(err, unix.EWOULDBLOCK) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func legacyExists(path string) bool {
	_, configErr := os.Stat(filepath.Join(path, "config.json"))
	_, manifestErr := os.Stat(filepath.Join(path, "manifest.json"))
	return configErr == nil || manifestErr == nil
}
