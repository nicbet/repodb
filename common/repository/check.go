package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/nicbet/repodb/common/storage"
)

// verifyBatchSize bounds the objects VerifyObjects requests from Git at once.
const verifyBatchSize = 1024

// ObjectProblemKind classifies an object that failed VerifyObjects.
type ObjectProblemKind string

const (
	ObjectMismatch ObjectProblemKind = "mismatch"
	ObjectMissing  ObjectProblemKind = "missing"
	ObjectNotBlob  ObjectProblemKind = "not-blob"
)

// ObjectProblem is a listed object whose Git object is missing, is not a blob,
// or does not hash to the object's name.
type ObjectProblem struct {
	Hash storage.Hash
	OID  string
	Kind ObjectProblemKind
	// Type is the Git object type found, for ObjectNotBlob.
	Type string
}

func (p ObjectProblem) String() string {
	switch p.Kind {
	case ObjectMissing:
		return fmt.Sprintf("object %s: missing Git object %s", p.Hash, p.OID)
	case ObjectNotBlob:
		return fmt.Sprintf("object %s: Git object %s is a %s, not a blob", p.Hash, p.OID, p.Type)
	default:
		return fmt.Sprintf("object %s: content does not match its name", p.Hash)
	}
}

// LoadSnapshotUncached opens commit with every check of an ordinary open, but
// bypasses the snapshot memo: the snapshot shares no objects read earlier in
// the process, and later opens don't reuse it.
func (r *Repository) LoadSnapshotUncached(ctx context.Context, commit string) (*Snapshot, error) {
	if commit == "" {
		return nil, errors.New("snapshot commit is required")
	}
	return r.readSnapshot(ctx, commit)
}

// ResolveCommit resolves a Git revision to a commit ID.
func (r *Repository) ResolveCommit(ctx context.Context, revision string) (string, error) {
	return r.git.ResolveRef(ctx, r.Root, revision)
}

// DataHistory lists every commit reachable from the data head, newest first,
// following every parent of merges.
func (r *Repository) DataHistory(ctx context.Context) ([]string, error) {
	head, err := r.Head(ctx)
	if err != nil {
		return nil, err
	}
	if head == "" {
		return nil, ErrNotInitialized
	}
	return r.git.RevList(ctx, r.Root, head)
}

// VerifyObjects reads every listed object by Git object ID, in batches, and
// checks that it hashes to its name. Objects that pass are cached in the
// snapshot, so later reads cost nothing; every failure is returned, not just
// the first. Objects whose Git object ID skip accepts are neither read nor
// cached. The error reports operational failures only.
func (s *Snapshot) VerifyObjects(ctx context.Context, skip func(oid string) bool) ([]ObjectProblem, error) {
	var problems []ObjectProblem
	hashes := make([]storage.Hash, 0, verifyBatchSize)
	oids := make([]string, 0, verifyBatchSize)
	flush := func() error {
		if len(oids) == 0 {
			return nil
		}
		results, err := s.repo.git.ReadObjectResults(ctx, s.repo.Root, oids)
		if err != nil {
			return err
		}
		verified := make(map[storage.Hash][]byte, len(results))
		for i, result := range results {
			problem := ObjectProblem{Hash: hashes[i], OID: oids[i]}
			switch {
			case result.Missing:
				problem.Kind = ObjectMissing
			case result.Type != "blob":
				problem.Kind, problem.Type = ObjectNotBlob, result.Type
			case storage.Sum(result.Data) != hashes[i]:
				problem.Kind = ObjectMismatch
			default:
				verified[hashes[i]] = result.Data
				continue
			}
			problems = append(problems, problem)
		}
		s.cache.mu.Lock()
		for hash, data := range verified {
			s.cache.data[hash] = data
		}
		s.cache.mu.Unlock()
		hashes, oids = hashes[:0], oids[:0]
		return nil
	}
	for _, hash := range s.Manifest.Objects {
		oid := s.objectOIDs[hash]
		if skip != nil && skip(oid) {
			continue
		}
		hashes = append(hashes, hash)
		oids = append(oids, oid)
		if len(oids) == verifyBatchSize {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return problems, nil
}
