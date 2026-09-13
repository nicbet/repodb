package repository

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/nicbet/repodb/common/storage"
	"golang.org/x/sys/unix"
)

const workingFormatVersion = 1

var (
	ErrWorkingCorrupt     = errors.New("corrupt RepoDB working journal")
	ErrWorkingBaseChanged = errors.New("RepoDB committed head changed while working state is dirty")
)

type WorkingFaultPoint string

const (
	BeforeJournalAppend WorkingFaultPoint = "before-journal-append"
	BeforeJournalFlush  WorkingFaultPoint = "before-journal-flush"
	AfterJournalFlush   WorkingFaultPoint = "after-journal-flush"
	AfterCheckpointRef  WorkingFaultPoint = "after-checkpoint-ref"
)

// WorkingState is the opt-in M4.3 durable journal prototype. Its files are
// authoritative and live beneath the repository's common Git directory.
type WorkingState struct {
	repo  *Repository
	fault func(WorkingFaultPoint) error
}

type WorkingStatus struct {
	BaseCommit string
	HeadCommit string
	Generation uint64
	Dirty      bool
}

type WorkingCommitResult struct {
	Outcome       CommitOutcome
	TransactionID string
	Generation    uint64
}

type WorkingCommitError struct {
	Outcome       CommitOutcome
	TransactionID string
	Err           error
}

func (e *WorkingCommitError) Error() string {
	return fmt.Sprintf("[repodb-working-commit outcome=%s transaction=%s] %v", e.Outcome, e.TransactionID, e.Err)
}
func (e *WorkingCommitError) Unwrap() error { return e.Err }

type TableChange struct {
	Table  string
	Change string
}

type journalRecord struct {
	Version    int                     `json:"version"`
	Kind       string                  `json:"kind"`
	TxID       string                  `json:"tx_id,omitempty"`
	Generation uint64                  `json:"generation"`
	BaseCommit string                  `json:"base_commit"`
	GitCommit  string                  `json:"git_commit,omitempty"`
	Manifest   *Manifest               `json:"manifest,omitempty"`
	Objects    map[storage.Hash][]byte `json:"objects,omitempty"`
}

type workingView struct {
	snapshot   *Snapshot
	generation uint64
	dirty      bool
	baseCommit string
}

func OpenWorkingState(repo *Repository) (*WorkingState, error) {
	if repo == nil {
		return nil, errors.New("repository is required")
	}
	return &WorkingState{repo: repo}, nil
}

func (w *WorkingState) SetFaultInjector(inject func(WorkingFaultPoint) error) { w.fault = inject }

func (w *WorkingState) Dir() string {
	return filepath.Join(w.repo.CommonDir, "repodb", "working", "v1")
}
func (w *WorkingState) JournalPath() string { return filepath.Join(w.Dir(), "journal") }

func (w *WorkingState) Exists() bool {
	_, err := os.Stat(w.JournalPath())
	return err == nil
}

func (w *WorkingState) Current(ctx context.Context) (*Snapshot, error) {
	release, err := w.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	view, err := w.load(ctx)
	if err != nil {
		return nil, err
	}
	return view.snapshot, nil
}

func (w *WorkingState) Status(ctx context.Context) (WorkingStatus, error) {
	release, err := w.lock(ctx)
	if err != nil {
		return WorkingStatus{}, err
	}
	defer release()
	view, err := w.load(ctx)
	if err != nil {
		return WorkingStatus{}, err
	}
	head, err := w.repo.Head(ctx)
	if err != nil {
		return WorkingStatus{}, err
	}
	return WorkingStatus{BaseCommit: view.baseCommit, HeadCommit: head, Generation: view.generation, Dirty: view.dirty}, nil
}

// Diff reports table-root changes since the last intentional data commit.
func (w *WorkingState) Diff(ctx context.Context) ([]TableChange, error) {
	release, err := w.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	view, err := w.load(ctx)
	if err != nil {
		return nil, err
	}
	base, err := w.repo.SnapshotCommit(ctx, view.baseCommit)
	if err != nil {
		return nil, err
	}
	names := make(map[string]struct{}, len(base.Manifest.Tables)+len(view.snapshot.Manifest.Tables))
	for name := range base.Manifest.Tables {
		names[name] = struct{}{}
	}
	for name := range view.snapshot.Manifest.Tables {
		names[name] = struct{}{}
	}
	changes := make([]TableChange, 0)
	for name := range names {
		before, beforeOK := base.Manifest.Tables[name]
		after, afterOK := view.snapshot.Manifest.Tables[name]
		change := "modified"
		switch {
		case !beforeOK:
			change = "added"
		case !afterOK:
			change = "deleted"
		case before == after:
			continue
		}
		changes = append(changes, TableChange{Table: name, Change: change})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Table < changes[j].Table })
	return changes, nil
}

// Commit durably appends a prepared catalog and its commit marker. The marker
// and all object bytes are flushed before success is returned.
func (w *WorkingState) Commit(ctx context.Context, writer *Writer, manifest Manifest) (*Snapshot, string, error) {
	if writer == nil || writer.repo != w.repo {
		return nil, "", errors.New("working writer belongs to a different repository")
	}
	release, err := w.lock(ctx)
	if err != nil {
		return nil, "", err
	}
	defer release()
	view, err := w.load(ctx)
	if err != nil {
		return nil, "", err
	}
	if writer.base.Generation() != view.generation || writer.base.Commit != view.snapshot.Commit {
		return nil, "", ErrConflict
	}
	manifest, available, objects, err := takeWorkingWriter(writer, manifest)
	if err != nil {
		return nil, "", err
	}
	txid, err := randomID()
	if err != nil {
		return nil, "", err
	}
	generation := view.generation + 1
	prepare := journalRecord{Version: workingFormatVersion, Kind: "prepare", TxID: txid, Generation: generation, BaseCommit: view.baseCommit, Manifest: &manifest, Objects: objects}
	marker := journalRecord{Version: workingFormatVersion, Kind: "commit", TxID: txid, Generation: generation, BaseCommit: view.baseCommit}
	if w.fault != nil {
		if err := w.fault(BeforeJournalAppend); err != nil {
			return nil, txid, &WorkingCommitError{Outcome: OutcomeRejected, TransactionID: txid, Err: err}
		}
	}
	if err := w.appendRecords(prepare, marker); err != nil {
		return nil, txid, &WorkingCommitError{Outcome: OutcomeUnknown, TransactionID: txid, Err: err}
	}
	snapshot := workingSnapshot(writer.base, manifest, available, objects, generation)
	if w.fault != nil {
		if err := w.fault(AfterJournalFlush); err != nil {
			return snapshot, txid, &WorkingCommitError{Outcome: OutcomeCommitted, TransactionID: txid, Err: err}
		}
	}
	return snapshot, txid, nil
}

// RecoverTransaction resolves the durable outcome of a journal transaction ID.
func (w *WorkingState) RecoverTransaction(ctx context.Context, transactionID string) (WorkingCommitResult, error) {
	if strings.TrimSpace(transactionID) == "" {
		return WorkingCommitResult{Outcome: OutcomeUnknown}, errors.New("working transaction ID is required")
	}
	release, err := w.lock(ctx)
	if err != nil {
		return WorkingCommitResult{Outcome: OutcomeUnknown, TransactionID: transactionID}, err
	}
	defer release()
	records, err := readJournal(w.JournalPath())
	if err != nil {
		return WorkingCommitResult{Outcome: OutcomeUnknown, TransactionID: transactionID}, err
	}
	result := WorkingCommitResult{Outcome: OutcomeRejected, TransactionID: transactionID}
	prepared := false
	for _, record := range records {
		if record.TxID != transactionID {
			continue
		}
		switch record.Kind {
		case "prepare":
			prepared, result.Generation = true, record.Generation
		case "commit":
			if prepared && record.Generation == result.Generation {
				result.Outcome = OutcomeCommitted
				return result, nil
			}
		}
	}
	return result, nil
}

// Checkpoint captures the current durable generation as one Git data commit.
// Writers are serialized for the prototype so later generations cannot be
// accidentally marked clean.
func (w *WorkingState) Checkpoint(ctx context.Context, message string) (CommitResult, error) {
	release, err := w.lock(ctx)
	if err != nil {
		return CommitResult{Outcome: OutcomeRejected}, err
	}
	defer release()
	view, err := w.load(ctx)
	if err != nil {
		return CommitResult{Outcome: OutcomeRejected}, err
	}
	if !view.dirty {
		return CommitResult{Outcome: OutcomeCommitted, Commit: view.snapshot.Commit, Snapshot: view.snapshot}, nil
	}
	base, err := w.repo.SnapshotCommit(ctx, view.baseCommit)
	if err != nil {
		return CommitResult{Outcome: OutcomeRejected}, err
	}
	writer, err := w.repo.BeginSnapshot(base)
	if err != nil {
		return CommitResult{Outcome: OutcomeRejected}, err
	}
	for _, hash := range view.snapshot.Manifest.Objects {
		if _, present := base.objectSet[hash]; present {
			continue
		}
		data, err := view.snapshot.Store().Get(ctx, hash)
		if err != nil {
			return CommitResult{Outcome: OutcomeRejected}, err
		}
		if _, err := writer.Put(ctx, data); err != nil {
			return CommitResult{Outcome: OutcomeRejected}, err
		}
	}
	if err := writer.RetainOnly(view.snapshot.Manifest.Objects); err != nil {
		return CommitResult{Outcome: OutcomeRejected}, err
	}
	result, err := writer.CommitWithOutcomeMessage(ctx, view.snapshot.Manifest, message)
	if err != nil && result.Outcome != OutcomeCommitted {
		return result, err
	}
	if w.fault != nil {
		if faultErr := w.fault(AfterCheckpointRef); faultErr != nil {
			return result, errors.Join(err, faultErr)
		}
	}
	marker := journalRecord{Version: workingFormatVersion, Kind: "checkpoint", Generation: view.generation, BaseCommit: view.baseCommit, GitCommit: result.Commit}
	if appendErr := w.appendRecords(marker); appendErr != nil {
		return result, errors.Join(err, fmt.Errorf("checkpoint %s published but journal bookkeeping failed: %w", result.Commit, appendErr))
	}
	return result, err
}

func takeWorkingWriter(w *Writer, manifest Manifest) (Manifest, map[storage.Hash]struct{}, map[storage.Hash][]byte, error) {
	w.mu.Lock()
	if w.committed {
		w.mu.Unlock()
		return Manifest{}, nil, nil, errors.New("snapshot writer is already committed")
	}
	w.committed = true
	newObjects := w.objects
	w.objects = nil
	retained := w.retained
	w.mu.Unlock()
	available := make(map[storage.Hash]struct{})
	if retained != nil {
		for hash := range retained {
			available[hash] = struct{}{}
		}
	} else {
		for _, hash := range w.base.Manifest.Objects {
			available[hash] = struct{}{}
		}
		for hash := range newObjects {
			available[hash] = struct{}{}
		}
	}
	for hash := range available {
		_, inBase := w.base.objectSet[hash]
		data, isNew := newObjects[hash]
		if !inBase && !isNew {
			return Manifest{}, nil, nil, fmt.Errorf("retained object %s is unavailable", hash)
		}
		if isNew && storage.Sum(data) != hash {
			return Manifest{}, nil, nil, fmt.Errorf("object %q failed integrity check", hash)
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
		return Manifest{}, nil, nil, err
	}
	kept := make(map[storage.Hash][]byte)
	for hash, data := range newObjects {
		if _, ok := available[hash]; ok {
			kept[hash] = append([]byte(nil), data...)
		}
	}
	return manifest, available, kept, nil
}

func workingSnapshot(base *Snapshot, manifest Manifest, available map[storage.Hash]struct{}, objects map[storage.Hash][]byte, generation uint64) *Snapshot {
	cache := base.cache
	if cache == nil {
		cache = &snapshotObjectCache{data: make(map[storage.Hash][]byte)}
	}
	cache.mu.Lock()
	for hash, data := range objects {
		cache.data[hash] = append([]byte(nil), data...)
	}
	cache.mu.Unlock()
	oids := make(map[storage.Hash]string)
	for hash := range available {
		if oid := base.objectOIDs[hash]; oid != "" {
			oids[hash] = oid
		}
	}
	return &Snapshot{repo: base.repo, Commit: base.Commit, generation: generation, Manifest: manifest, objectSet: available, objectOIDs: oids, cache: cache}
}

func (w *WorkingState) load(ctx context.Context) (workingView, error) {
	head, err := w.repo.Head(ctx)
	if err != nil {
		return workingView{}, err
	}
	if head == "" {
		return workingView{}, ErrNotInitialized
	}
	records, err := readJournal(w.JournalPath())
	if err != nil {
		return workingView{}, err
	}
	initial := head
	for _, record := range records {
		if record.Kind == "prepare" && record.BaseCommit != "" {
			initial = record.BaseCommit
			break
		}
	}
	base, err := w.repo.SnapshotCommit(ctx, initial)
	if err != nil {
		return workingView{}, err
	}
	view := workingView{snapshot: base, baseCommit: initial}
	pending := make(map[string]journalRecord)
	for _, record := range records {
		switch record.Kind {
		case "prepare":
			if record.Manifest == nil || record.TxID == "" {
				return workingView{}, fmt.Errorf("%w: incomplete prepare record", ErrWorkingCorrupt)
			}
			for hash, data := range record.Objects {
				if !hash.Valid() || storage.Sum(data) != hash {
					return workingView{}, fmt.Errorf("%w: journal object %q failed integrity check", ErrWorkingCorrupt, hash)
				}
			}
			pending[record.TxID] = record
		case "commit":
			prepare, ok := pending[record.TxID]
			if !ok || prepare.Generation != record.Generation {
				return workingView{}, fmt.Errorf("%w: unmatched commit marker", ErrWorkingCorrupt)
			}
			if prepare.Generation != view.generation+1 {
				return workingView{}, fmt.Errorf("%w: non-monotonic generation", ErrWorkingCorrupt)
			}
			if prepare.BaseCommit != view.baseCommit {
				return workingView{}, fmt.Errorf("%w: transaction base changed", ErrWorkingCorrupt)
			}
			available := make(map[storage.Hash]struct{}, len(prepare.Manifest.Objects))
			for _, hash := range prepare.Manifest.Objects {
				available[hash] = struct{}{}
			}
			if err := validateManifestInventory(*prepare.Manifest, available); err != nil {
				return workingView{}, err
			}
			view.snapshot = workingSnapshot(view.snapshot, *prepare.Manifest, available, prepare.Objects, prepare.Generation)
			view.generation, view.dirty = prepare.Generation, true
			delete(pending, record.TxID)
		case "checkpoint":
			if record.Generation != view.generation {
				return workingView{}, fmt.Errorf("%w: checkpoint generation differs", ErrWorkingCorrupt)
			}
			base, err = w.repo.SnapshotCommit(ctx, record.GitCommit)
			if err != nil {
				return workingView{}, err
			}
			base.generation = view.generation
			view.snapshot, view.baseCommit, view.dirty = base, record.GitCommit, false
		default:
			return workingView{}, fmt.Errorf("%w: unknown record kind %q", ErrWorkingCorrupt, record.Kind)
		}
	}
	if view.dirty && view.baseCommit != head {
		current, err := w.repo.SnapshotCommit(ctx, head)
		if err != nil {
			return workingView{}, err
		}
		if !reflect.DeepEqual(current.Manifest, view.snapshot.Manifest) {
			return workingView{}, ErrWorkingBaseChanged
		}
		current.generation = view.generation
		view.snapshot, view.baseCommit, view.dirty = current, head, false
	} else if !view.dirty && view.baseCommit != head {
		current, err := w.repo.SnapshotCommit(ctx, head)
		if err != nil {
			return workingView{}, err
		}
		current.generation = view.generation
		view.snapshot, view.baseCommit = current, head
	}
	return view, nil
}

var journalCRC = crc32.MakeTable(crc32.Castagnoli)

func (w *WorkingState) appendRecords(records ...journalRecord) error {
	if err := os.MkdirAll(w.Dir(), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(w.JournalPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	for _, record := range records {
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		header := make([]byte, 12)
		copy(header[:4], "RDBJ")
		binary.BigEndian.PutUint32(header[4:8], uint32(len(data)))
		binary.BigEndian.PutUint32(header[8:12], crc32.Checksum(data, journalCRC))
		if _, err := file.Write(append(header, data...)); err != nil {
			return err
		}
	}
	if w.fault != nil {
		if err := w.fault(BeforeJournalFlush); err != nil {
			return err
		}
	}
	return file.Sync()
}

func readJournal(path string) ([]journalRecord, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var records []journalRecord
	for {
		header := make([]byte, 12)
		_, err := io.ReadFull(file, header)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return records, nil
		}
		if err != nil {
			return nil, err
		}
		if string(header[:4]) != "RDBJ" {
			return nil, fmt.Errorf("%w: invalid frame magic", ErrWorkingCorrupt)
		}
		length := binary.BigEndian.Uint32(header[4:8])
		if length == 0 || length > 1<<30 {
			return nil, fmt.Errorf("%w: invalid frame length", ErrWorkingCorrupt)
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(file, data); errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return records, nil
		} else if err != nil {
			return nil, err
		}
		if crc32.Checksum(data, journalCRC) != binary.BigEndian.Uint32(header[8:12]) {
			return nil, fmt.Errorf("%w: checksum mismatch", ErrWorkingCorrupt)
		}
		var record journalRecord
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&record); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrWorkingCorrupt, err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: trailing frame content", ErrWorkingCorrupt)
		}
		if record.Version != workingFormatVersion {
			return nil, fmt.Errorf("unsupported working journal format %d", record.Version)
		}
		records = append(records, record)
	}
}

func randomID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}

func (w *WorkingState) lock(ctx context.Context) (func(), error) {
	lockDir := filepath.Join(w.repo.CommonDir, "repodb", "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(lockDir, "working.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }, nil
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
