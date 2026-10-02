package repository

import "fmt"

// writeLog records what the journal transactions committed since a view's base
// commit wrote: the generations that changed each table and each unique-index
// claim. Row keys need no entries of their own, because the view's pending
// edits hold the newest edit per key together with the generation that wrote it.
//
// With it, a transaction whose base generation is stale still commits when its
// writes are disjoint from everything committed since its base
// (first-committer-wins snapshot isolation). Only holders of working.lock use
// it. Entries are only added, with increasing generations, so a view sharing a
// log that a later load extended can at worst report a spurious conflict, never
// miss one.
type writeLog struct {
	// from is the generation the log starts at: it records every write above
	// it and none at or below it.
	from   uint64
	tables map[string]tableWrites
	claims map[claimKey]uint64
}

type tableWrites struct {
	// schema is the last generation that created, altered or dropped the
	// table, touched the last that wrote to it at all.
	schema, touched uint64
}

type claimKey struct{ table, index, key string }

// recorded returns l with generation's writes added. A nil log starts at the
// generation before it.
func (l *writeLog) recorded(edits []TypedTableEdit, generation uint64) *writeLog {
	if l == nil {
		l = &writeLog{from: generation - 1, tables: make(map[string]tableWrites), claims: make(map[claimKey]uint64)}
	}
	for _, te := range edits {
		tw := l.tables[te.Table]
		tw.touched = generation
		if te.Drop || len(te.Schema) > 0 {
			tw.schema = generation
		}
		l.tables[te.Table] = tw
		for _, claim := range te.Claims {
			l.claims[claimKey{te.Table, claim.Index, string(claim.Key)}] = generation
		}
	}
	return l
}

// writeConflict reports whether edits, made by a transaction whose snapshot
// is at generation base, conflict with the transactions committed since. It
// returns nil when they are disjoint, or an error wrapping ErrConflict.
func (v workingView) writeConflict(base uint64, edits []TypedTableEdit) error {
	if base == v.generation {
		return nil
	}
	l := v.writes
	if base > v.generation || l == nil || base < l.from {
		// The log does not cover every commit since base: a checkpoint or
		// a whole-manifest commit came in between.
		return ErrConflict
	}
	for _, te := range edits {
		tw := l.tables[te.Table]
		if tw.touched <= base {
			continue
		}
		if tw.schema > base {
			return fmt.Errorf("%w: table %s was created, altered or dropped by a concurrent transaction", ErrConflict, te.Table)
		}
		if te.Drop || len(te.Schema) > 0 {
			return fmt.Errorf("%w: table %s was written by a concurrent transaction", ErrConflict, te.Table)
		}
		if pending := v.pendingEdits[te.Table]; pending != nil {
			for _, re := range te.Edits {
				if edit, ok := pending.rows.Get(re.Key); ok && edit.generation > base {
					return fmt.Errorf("%w: a row of table %s was written by a concurrent transaction", ErrConflict, te.Table)
				}
			}
		}
		for _, claim := range te.Claims {
			if l.claims[claimKey{te.Table, claim.Index, string(claim.Key)}] > base {
				return fmt.Errorf("%w: a concurrent transaction added the same key to unique index %s of table %s", ErrConflict, claim.Index, te.Table)
			}
		}
	}
	return nil
}
