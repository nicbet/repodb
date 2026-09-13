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

func (o CommitOutcome) String() string {
	switch o {
	case OutcomeRejected:
		return "rejected"
	case OutcomeCommitted:
		return "committed"
	case OutcomeUnknown:
		return "unknown"
	default:
		return "invalid"
	}
}

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

func (e *CommitError) Error() string {
	return fmt.Sprintf("[repodb-commit outcome=%s candidate=%s] %v", e.Outcome, e.Commit, e.Err)
}
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
	generation uint64
	Manifest   Manifest
	objectSet  map[storage.Hash]struct{}
	objectOIDs map[storage.Hash]string
	cache      *snapshotObjectCache
}

// Generation identifies the durable working-state version layered over Commit.
// Native-Git snapshots use generation zero.
func (s *Snapshot) Generation() uint64 { return s.generation }

type snapshotObjectCache struct {
	mu   sync.RWMutex
	data map[storage.Hash][]byte
}

type Writer struct {
	repo       *Repository
	base       *Snapshot
	expected   string
	objects    map[storage.Hash][]byte
	objectOIDs map[storage.Hash]string
	retained   map[storage.Hash]struct{}
	parents    []string
	committed  bool
	mu         sync.RWMutex
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
	w := &Writer{repo: repo, objects: make(map[storage.Hash][]byte), objectOIDs: make(map[storage.Hash]string)}
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
	repo, _, err := OpenWithSnapshot(ctx, start)
	return repo, err
}

// OpenWithSnapshot opens RepoDB and returns the validated current repository
// snapshot so callers can reuse the load instead of immediately reading it
// again.
func OpenWithSnapshot(ctx context.Context, start string) (*Repository, *Snapshot, error) {
	repo, err := discover(ctx, start)
	if err != nil {
		return nil, nil, err
	}
	commit, err := repo.git.ResolveRef(ctx, repo.Root, DataRef)
	if err != nil {
		if errors.Is(err, repodbgit.ErrRefNotFound) {
			if legacyExists(repo.Dir) {
				return nil, nil, fmt.Errorf("%w; run repodb import-legacy", ErrLegacyLayout)
			}
			return nil, nil, ErrNotInitialized
		}
		return nil, nil, err
	}
	snapshot, err := repo.loadSnapshot(ctx, commit)
	if err != nil {
		return nil, nil, err
	}
	return repo, snapshot, nil
}

// Discover locates repository-wide RepoDB state without requiring an initialized
// data ref. Integration setup uses it before deciding whether to adopt remote
// data or initialize an empty catalog.
func Discover(ctx context.Context, start string) (*Repository, error) { return discover(ctx, start) }

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

// Identity is stable for repository-scoped caches and includes the object
// format so cache entries cannot cross incompatible Git repositories.
func (r *Repository) Identity() string { return r.CommonDir + "\x00" + r.ObjectFormat }

// RepositoryIdentity identifies the repository that owns this snapshot.
func (s *Snapshot) RepositoryIdentity() string { return s.repo.Identity() }

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

// Head returns the live data commit, or an empty string when RepoDB has not yet
// initialized a local data history.
func (r *Repository) Head(ctx context.Context) (string, error) {
	head, err := r.git.ResolveRef(ctx, r.Root, DataRef)
	if errors.Is(err, repodbgit.ErrRefNotFound) {
		return "", nil
	}
	return head, err
}

func (r *Repository) SnapshotAt(ctx context.Context, revision string) (*Snapshot, error) {
	commit, err := r.git.ResolveRef(ctx, r.Root, revision)
	if err != nil {
		return nil, err
	}
	return r.loadSnapshot(ctx, commit)
}

// SnapshotCommit loads an already resolved immutable commit without an
// additional ref-resolution process.
func (r *Repository) SnapshotCommit(ctx context.Context, commit string) (*Snapshot, error) {
	if strings.TrimSpace(commit) == "" {
		return nil, errors.New("snapshot commit is required")
	}
	return r.loadSnapshot(ctx, commit)
}

func (r *Repository) MergeBase(ctx context.Context, left, right string) (string, error) {
	return r.git.MergeBase(ctx, r.Root, left, right)
}

type LockedPublication struct{ repo *Repository }

// WithPublicationLock serializes integration reconciliation with SQL commits.
func (r *Repository) WithPublicationLock(ctx context.Context, fn func(*LockedPublication) error) error {
	release, err := r.lock(ctx)
	if err != nil {
		return err
	}
	defer release()
	return fn(&LockedPublication{repo: r})
}

func (p *LockedPublication) Head(ctx context.Context) (string, error) {
	head, err := p.repo.git.ResolveRef(ctx, p.repo.Root, DataRef)
	if errors.Is(err, repodbgit.ErrRefNotFound) {
		return "", nil
	}
	return head, err
}

// FastForward validates target and advances the live head from expected. The
// LockedPublication value ensures callers already hold the shared writer lock.
func (p *LockedPublication) FastForward(ctx context.Context, expected, target string) (*Snapshot, error) {
	snapshot, err := p.repo.loadSnapshot(ctx, target)
	if err != nil {
		return nil, err
	}
	return p.FastForwardSnapshot(ctx, expected, snapshot)
}

// FastForwardSnapshot advances to a snapshot already loaded and validated by
// the caller, while retaining the publication-time expected-head check.
func (p *LockedPublication) FastForwardSnapshot(ctx context.Context, expected string, snapshot *Snapshot) (*Snapshot, error) {
	if snapshot == nil || snapshot.repo != p.repo {
		return nil, errors.New("fast-forward snapshot belongs to a different repository")
	}
	target := snapshot.Commit
	actual, err := p.Head(ctx)
	if err != nil {
		return nil, err
	}
	if actual != expected {
		return nil, ErrConflict
	}
	if actual == target {
		return snapshot, nil
	}
	if actual != "" {
		forward, err := p.repo.git.IsAncestor(ctx, p.repo.Root, actual, target)
		if err != nil {
			return nil, err
		}
		if !forward {
			return nil, fmt.Errorf("target %s is not a fast-forward of local data head %s", target, actual)
		}
	}
	if err := p.repo.git.UpdateRef(ctx, p.repo.Root, DataRef, target, actual); err != nil {
		now, resolveErr := p.Head(context.Background())
		if resolveErr == nil && now != actual {
			return nil, ErrConflict
		}
		return nil, err
	}
	return snapshot, nil
}

// Begin pins the current immutable snapshot. Reads through the writer see that
// snapshot plus blobs added to the writer.
func (r *Repository) Begin(ctx context.Context) (*Writer, error) {
	base, err := r.Current(ctx)
	if err != nil {
		return nil, err
	}
	return r.BeginSnapshot(base)
}

// BeginSnapshot creates a writer from an already loaded immutable base.
func (r *Repository) BeginSnapshot(base *Snapshot) (*Writer, error) {
	if base == nil || base.repo != r {
		return nil, errors.New("base snapshot belongs to a different repository")
	}
	return &Writer{
		repo:       r,
		base:       base,
		expected:   base.Commit,
		objects:    make(map[storage.Hash][]byte),
		objectOIDs: make(map[storage.Hash]string),
	}, nil
}

// BeginMerge creates a writer whose publication expects local and whose commit
// records both reconciled histories as parents.
func (r *Repository) BeginMerge(ctx context.Context, local, remote string) (*Writer, error) {
	base, err := r.loadSnapshot(ctx, local)
	if err != nil {
		return nil, err
	}
	other, err := r.loadSnapshot(ctx, remote)
	if err != nil {
		return nil, err
	}
	return r.BeginMergeSnapshots(base, other)
}

// BeginMergeSnapshots creates a merge writer from snapshots already loaded and
// validated by the caller. Both snapshots must belong to this repository and
// their immutable commit IDs become the expected head and merge parents.
func (r *Repository) BeginMergeSnapshots(local, remote *Snapshot) (*Writer, error) {
	if local == nil || remote == nil {
		return nil, errors.New("local and remote merge snapshots are required")
	}
	if local.repo != r || remote.repo != r {
		return nil, errors.New("merge snapshots belong to a different repository")
	}
	return &Writer{
		repo: r, base: local, expected: local.Commit,
		objects: make(map[storage.Hash][]byte), objectOIDs: make(map[storage.Hash]string), parents: []string{local.Commit, remote.Commit},
	}, nil
}

func (s *Snapshot) Store() storage.Store { return &snapshotStore{snapshot: s} }

func (w *Writer) BaseSnapshot() *Snapshot { return w.base }

// PendingHashes returns the immutable objects added by this writer. It is used
// by the journal prototype to retain a conservative object superset without an
// exact reachability walk on every SQL save.
func (w *Writer) PendingHashes() []storage.Hash {
	w.mu.RLock()
	defer w.mu.RUnlock()
	hashes := make([]storage.Hash, 0, len(w.objects))
	for hash := range w.objects {
		hashes = append(hashes, hash)
	}
	return hashes
}

func (w *Writer) Put(_ context.Context, data []byte) (storage.Hash, error) {
	hash := storage.Sum(data)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed {
		return "", errors.New("snapshot writer is already committed")
	}
	if w.base != nil {
		if _, exists := w.base.objectSet[hash]; exists {
			return hash, nil
		}
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

// ImportObjects makes selected immutable objects from another validated
// snapshot available to this writer. Objects already present in the local base
// are retained by identity; only missing bytes are copied, and their existing
// Git object IDs are reused during publication.
func (w *Writer) ImportObjects(ctx context.Context, source *Snapshot, hashes []storage.Hash) error {
	if source == nil || source.repo != w.repo {
		return errors.New("import snapshot belongs to a different repository")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed {
		return errors.New("snapshot writer is already committed")
	}
	for _, hash := range hashes {
		if !hash.Valid() {
			return fmt.Errorf("invalid imported object hash %q", hash)
		}
		if w.base != nil {
			if _, ok := w.base.objectSet[hash]; ok {
				continue
			}
		}
		if _, ok := w.objects[hash]; ok {
			continue
		}
		data, err := source.Store().Get(ctx, hash)
		if err != nil {
			return err
		}
		w.objects[hash] = data
		w.objectOIDs[hash] = source.objectOIDs[hash]
	}
	return nil
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
	return w.CommitWithOutcomeMessage(ctx, manifest, fmt.Sprintf("RepoDB snapshot v%d", FormatVersion))
}

// CommitWithOutcomeMessage publishes a snapshot with an explicit data-history
// message. SQL's native-Git path continues to use CommitWithOutcome.
func (w *Writer) CommitWithOutcomeMessage(ctx context.Context, manifest Manifest, message string) (CommitResult, error) {
	result := CommitResult{Outcome: OutcomeRejected}
	inventoryStarted := time.Now()
	w.mu.Lock()
	if w.committed {
		w.mu.Unlock()
		return result, errors.New("snapshot writer is already committed")
	}
	w.committed = true
	newObjects := w.objects
	w.objects = nil
	newObjectOIDs := make(map[storage.Hash]string, len(w.objectOIDs))
	for hash, oid := range w.objectOIDs {
		newObjectOIDs[hash] = oid
	}
	retained := w.retained
	w.mu.Unlock()

	available := make(map[storage.Hash]struct{})
	if retained != nil {
		for hash := range retained {
			available[hash] = struct{}{}
		}
	} else {
		if w.base != nil {
			for _, hash := range w.base.Manifest.Objects {
				available[hash] = struct{}{}
			}
		}
		for hash := range newObjects {
			available[hash] = struct{}{}
		}
	}
	for hash := range available {
		_, inBase := w.baseObjectOID(hash)
		data, isNew := newObjects[hash]
		if !inBase && !isNew {
			return result, fmt.Errorf("retained object %s is unavailable", hash)
		}
		if isNew && storage.Sum(data) != hash {
			return result, fmt.Errorf("object %q failed integrity check", hash)
		}
	}
	manifest.FormatVersion = FormatVersion
	if manifest.DefaultDatabase == "" {
		manifest.DefaultDatabase = "repodb"
	}
	if manifest.Tables == nil {
		manifest.Tables = map[string]Table{}
	}
	manifest.Objects = sortedHashSet(available)
	if err := validateManifestInventory(manifest, available); err != nil {
		return result, err
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return result, err
	}
	manifestData = append(manifestData, '\n')
	publicationMetrics.inventory.Add(uint64(time.Since(inventoryStarted)))

	lockStarted := time.Now()
	release, err := w.repo.lock(ctx)
	if err != nil {
		return result, err
	}
	publicationMetrics.lockWait.Add(uint64(time.Since(lockStarted)))
	lockAcquired := time.Now()
	defer func() {
		release()
		publicationMetrics.lockHold.Add(uint64(time.Since(lockAcquired)))
	}()

	entries := make([]repodbgit.TreeEntry, 0, len(available)+1)
	missing := make([]storage.Hash, 0, len(manifest.Objects))
	writeData := make([][]byte, 0, len(manifest.Objects)+1)
	writeData = append(writeData, manifestData)
	for _, hash := range manifest.Objects {
		oid := ""
		if w.base != nil {
			oid = w.base.objectOIDs[hash]
		}
		if oid == "" {
			oid = newObjectOIDs[hash]
		}
		if oid == "" {
			missing = append(missing, hash)
			writeData = append(writeData, newObjects[hash])
		}
	}
	phaseStarted := time.Now()
	oids, err := w.repo.git.HashObjects(ctx, w.repo.Root, filepath.Join(w.repo.CommonDir, "repodb", "tmp"), writeData)
	publicationMetrics.objectWrite.Add(uint64(time.Since(phaseStarted)))
	if err != nil {
		return result, err
	}
	manifestOID := oids[0]
	writtenOIDs := make(map[storage.Hash]string, len(missing))
	for i, hash := range missing {
		writtenOIDs[hash] = oids[i+1]
	}
	entries = append(entries, repodbgit.TreeEntry{Path: "manifest.json", ObjectID: manifestOID})
	for _, hash := range manifest.Objects {
		oid := ""
		if w.base != nil {
			oid = w.base.objectOIDs[hash]
		}
		if oid == "" {
			oid = newObjectOIDs[hash]
		}
		if oid == "" {
			oid = writtenOIDs[hash]
		}
		entries = append(entries, repodbgit.TreeEntry{Path: objectPath(hash), ObjectID: oid})
	}
	phaseStarted = time.Now()
	tree := ""
	if w.base != nil {
		updates := []repodbgit.TreeEntry{entries[0]}
		for hash := range newObjects {
			if _, kept := available[hash]; kept {
				updates = append(updates, repodbgit.TreeEntry{Path: objectPath(hash), ObjectID: snapshotOID(entries, hash)})
			}
		}
		var deleted []string
		for _, hash := range w.base.Manifest.Objects {
			if _, kept := available[hash]; !kept {
				deleted = append(deleted, objectPath(hash))
			}
		}
		tree, err = w.repo.git.UpdateTree(ctx, w.repo.Root, w.repo.CommonDir, w.base.Commit, updates, deleted)
	} else {
		tree, err = w.repo.git.WriteTree(ctx, w.repo.Root, w.repo.CommonDir, entries)
	}
	if err != nil {
		return result, err
	}
	parents := w.parents
	if len(parents) == 0 && w.expected != "" {
		parents = []string{w.expected}
	}
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf("RepoDB snapshot v%d", FormatVersion)
	}
	commit, err := w.repo.git.CommitTreeParents(ctx, w.repo.Root, tree, parents, message)
	publicationMetrics.treeCommit.Add(uint64(time.Since(phaseStarted)))
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
	phaseStarted = time.Now()
	if err := w.repo.git.UpdateRef(ctx, w.repo.Root, DataRef, commit, w.expected); err != nil {
		publicationMetrics.refUpdate.Add(uint64(time.Since(phaseStarted)))
		actual, resolveErr := w.repo.git.ResolveRef(context.Background(), w.repo.Root, DataRef)
		if resolveErr == nil && actual != w.expected {
			// Another writer advanced the ref past our expected head. Even if
			// actual == commit (identical tree/parents/timestamp produced the
			// same hash), our CAS did not succeed.
			return CommitResult{Outcome: OutcomeRejected, Commit: commit}, ErrConflict
		}
		result = w.repo.resolvePublication(commit, w.expected)
		if result.Outcome == OutcomeRejected {
			return result, err
		}
		if result.Outcome == OutcomeUnknown {
			return result, &CommitError{Outcome: OutcomeUnknown, Commit: commit, Err: fmt.Errorf("%w: candidate %s: %v", ErrCommitUnknown, commit, err)}
		}
	}
	publicationMetrics.refUpdate.Add(uint64(time.Since(phaseStarted)))
	result = CommitResult{Outcome: OutcomeCommitted, Commit: commit}
	if w.repo.fault != nil {
		if err := w.repo.fault(AfterRefPublication); err != nil {
			return result, &CommitError{Outcome: OutcomeCommitted, Commit: commit, Err: err}
		}
	}
	cache := &snapshotObjectCache{data: make(map[storage.Hash][]byte)}
	if w.base != nil && w.base.cache != nil {
		cache = w.base.cache
	}
	snapshot := &Snapshot{
		repo: w.repo, Commit: commit, Manifest: manifest,
		objectSet: available, objectOIDs: make(map[storage.Hash]string, len(available)),
		cache: cache,
	}
	for _, entry := range entries[1:] {
		hash := storage.Hash(strings.ReplaceAll(strings.TrimPrefix(entry.Path, "objects/sha256/"), "/", ""))
		snapshot.objectOIDs[hash] = entry.ObjectID
	}
	cache.mu.Lock()
	for hash, data := range newObjects {
		if _, kept := available[hash]; kept {
			cache.data[hash] = data
		}
	}
	cache.mu.Unlock()
	phaseStarted = time.Now()
	err = w.repo.verifySnapshotTree(context.WithoutCancel(ctx), snapshot, manifestOID)
	publicationMetrics.verification.Add(uint64(time.Since(phaseStarted)))
	result.Snapshot = snapshot
	if err != nil {
		return result, &CommitError{Outcome: OutcomeCommitted, Commit: commit, Err: fmt.Errorf("published %s but verification failed: %w", commit, err)}
	}
	return result, nil
}

func (w *Writer) baseObjectOID(hash storage.Hash) (string, bool) {
	if w.base == nil {
		return "", false
	}
	oid, ok := w.base.objectOIDs[hash]
	return oid, ok
}

func snapshotOID(entries []repodbgit.TreeEntry, hash storage.Hash) string {
	path := objectPath(hash)
	for _, entry := range entries {
		if entry.Path == path {
			return entry.ObjectID
		}
	}
	return ""
}

func (r *Repository) verifySnapshotTree(ctx context.Context, snapshot *Snapshot, manifestOID string) error {
	objects, err := r.git.ListTreeObjects(ctx, r.Root, snapshot.Commit)
	if err != nil {
		return err
	}
	if objects["manifest.json"] != manifestOID || len(objects) != len(snapshot.Manifest.Objects)+1 {
		return fmt.Errorf("%w: published snapshot tree inventory differs", ErrCorrupt)
	}
	for hash, oid := range snapshot.objectOIDs {
		if objects[objectPath(hash)] != oid {
			return fmt.Errorf("%w: published object %s differs", ErrCorrupt, hash)
		}
	}
	return nil
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
	snapshot := &Snapshot{repo: r, Commit: commit, Manifest: manifest, objectSet: objectSet, objectOIDs: make(map[storage.Hash]string), cache: &snapshotObjectCache{data: make(map[storage.Hash][]byte)}}
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
		snapshot.cache.data[hash] = data
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
	s.snapshot.cache.mu.RLock()
	cached, ok := s.snapshot.cache.data[hash]
	s.snapshot.cache.mu.RUnlock()
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
	s.snapshot.cache.mu.Lock()
	s.snapshot.cache.data[hash] = append([]byte(nil), data...)
	s.snapshot.cache.mu.Unlock()
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

func sortedHashSet(objects map[storage.Hash]struct{}) []storage.Hash {
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
