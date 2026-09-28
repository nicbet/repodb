package repository

import (
	"errors"
	"os"
	"path/filepath"
)

// refStamp records the on-disk state of every file Git may use to store the
// data ref: the loose ref, packed-refs, and the reftable table list. Every ref
// update by any process (RepoDB, plain git update-ref, pack-refs) rewrites or
// replaces at least one of them through a lockfile rename, which changes its
// identity, size, or modification time. An unchanged stamp therefore means the
// ref still has the value last resolved.
//
// A false match would need an inode reused within a single mtime tick with an
// identical size. Filesystems with nanosecond timestamps make that impractical;
// on coarse-timestamp filesystems it would delay visibility until the next
// ref change.
type refStamp [3]os.FileInfo

func (r *Repository) refStorageStamp() (refStamp, error) {
	paths := [3]string{
		filepath.Join(r.CommonDir, filepath.FromSlash(DataRef)),
		filepath.Join(r.CommonDir, "packed-refs"),
		filepath.Join(r.CommonDir, "reftable", "tables.list"),
	}
	var stamp refStamp
	for i, path := range paths {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return refStamp{}, err
		}
		stamp[i] = info
	}
	return stamp, nil
}

func (s refStamp) equal(other refStamp) bool {
	for i := range s {
		a, b := s[i], other[i]
		if a == nil || b == nil {
			if a != b {
				return false
			}
			continue
		}
		if !os.SameFile(a, b) || a.Size() != b.Size() || !a.ModTime().Equal(b.ModTime()) {
			return false
		}
	}
	return true
}
