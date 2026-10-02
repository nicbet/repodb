package repository

import (
	"bytes"
	"sort"
)

// PendingRows is an immutable, sorted set of typed row edits: one table's
// journal edits since the last checkpoint. It is a list of sorted runs, newest
// first, that are never modified once built, so snapshots and transactions
// share them without copying. With merges runs size-tiered (a run is merged
// into the older one while it is at least half its size), so each edit is
// rewritten O(log n) times and a set of n edits has O(log n) runs.
//
// The key and value bytes of the edits are shared and must not be modified.
type PendingRows struct {
	runs []pendingRun
}

// pendingRun holds edits sorted by key with unique keys.
type pendingRun []TypedRowEdit

// Empty reports whether the set holds no edits.
func (p PendingRows) Empty() bool { return len(p.runs) == 0 }

// Len returns an upper bound on the number of distinct keys: the total length
// of the runs, in which a key may appear more than once.
func (p PendingRows) Len() int {
	n := 0
	for _, run := range p.runs {
		n += len(run)
	}
	return n
}

// With returns a set holding p's edits overlaid with edits, which win over p's
// for equal keys; among edits, the last one for a key wins. p is unchanged.
// With takes ownership of the edits' key and value bytes.
func (p PendingRows) With(edits []TypedRowEdit) PendingRows {
	if len(edits) == 0 {
		return p
	}
	run := make(pendingRun, len(edits))
	copy(run, edits)
	sort.SliceStable(run, func(i, j int) bool { return bytes.Compare(run[i].Key, run[j].Key) < 0 })
	unique := run[:0]
	for i, edit := range run {
		if i+1 < len(run) && bytes.Equal(edit.Key, run[i+1].Key) {
			continue
		}
		unique = append(unique, edit)
	}
	runs := make([]pendingRun, 0, len(p.runs)+1)
	runs = append(runs, unique[:len(unique):len(unique)])
	runs = append(runs, p.runs...)
	for len(runs) > 1 && 2*len(runs[0]) >= len(runs[1]) {
		merged := mergeRuns(runs[0], runs[1])
		runs = append(runs[:1], runs[2:]...)
		runs[0] = merged
	}
	return PendingRows{runs: runs}
}

// mergeRuns merges two runs into a new one; newer wins for equal keys.
func mergeRuns(newer, older pendingRun) pendingRun {
	out := make(pendingRun, 0, len(newer)+len(older))
	i, j := 0, 0
	for i < len(newer) && j < len(older) {
		switch c := bytes.Compare(newer[i].Key, older[j].Key); {
		case c < 0:
			out = append(out, newer[i])
			i++
		case c > 0:
			out = append(out, older[j])
			j++
		default:
			out = append(out, newer[i])
			i++
			j++
		}
	}
	out = append(out, newer[i:]...)
	return append(out, older[j:]...)
}

// Get returns the newest edit for key.
func (p PendingRows) Get(key []byte) (TypedRowEdit, bool) {
	for _, run := range p.runs {
		i := sort.Search(len(run), func(i int) bool { return bytes.Compare(run[i].Key, key) >= 0 })
		if i < len(run) && bytes.Equal(run[i].Key, key) {
			return run[i], true
		}
	}
	return TypedRowEdit{}, false
}

// Iter returns an iterator over the newest edit for each key in [start, end),
// in key order. A nil end is unbounded. Deletes are returned as edits.
func (p PendingRows) Iter(start, end []byte) *PendingIter {
	it := &PendingIter{runs: p.runs, pos: make([]int, len(p.runs)), end: end}
	for r, run := range p.runs {
		it.pos[r] = sort.Search(len(run), func(i int) bool { return bytes.Compare(run[i].Key, start) >= 0 })
	}
	return it
}

// IterReverse returns an iterator over the newest edit for each key in
// [start, end), in descending key order. A nil end is unbounded.
func (p PendingRows) IterReverse(start, end []byte) *PendingIter {
	it := &PendingIter{runs: p.runs, pos: make([]int, len(p.runs)), start: start, reverse: true}
	for r, run := range p.runs {
		it.pos[r] = len(run) - 1
		if end != nil {
			it.pos[r] = sort.Search(len(run), func(i int) bool { return bytes.Compare(run[i].Key, end) >= 0 }) - 1
		}
	}
	return it
}

// PendingIter walks a PendingRows in key order, ascending or descending.
type PendingIter struct {
	runs    []pendingRun
	pos     []int // per run: the next index, counting up, or down when reverse
	end     []byte
	start   []byte
	reverse bool
}

// Next returns the next edit, or ok=false at the end.
func (it *PendingIter) Next() (edit TypedRowEdit, ok bool) {
	if it.reverse {
		return it.prev()
	}
	best := -1
	for r, run := range it.runs {
		if it.pos[r] >= len(run) {
			continue
		}
		// Runs are newest first, so on equal keys the earlier run wins.
		if best < 0 || bytes.Compare(run[it.pos[r]].Key, it.runs[best][it.pos[best]].Key) < 0 {
			best = r
		}
	}
	if best < 0 {
		return TypedRowEdit{}, false
	}
	edit = it.runs[best][it.pos[best]]
	if it.end != nil && bytes.Compare(edit.Key, it.end) >= 0 {
		for r := range it.pos {
			it.pos[r] = len(it.runs[r])
		}
		return TypedRowEdit{}, false
	}
	for r, run := range it.runs {
		if it.pos[r] < len(run) && bytes.Equal(run[it.pos[r]].Key, edit.Key) {
			it.pos[r]++
		}
	}
	return edit, true
}

func (it *PendingIter) prev() (edit TypedRowEdit, ok bool) {
	best := -1
	for r, run := range it.runs {
		if it.pos[r] < 0 {
			continue
		}
		// Runs are newest first, so on equal keys the earlier run wins.
		if best < 0 || bytes.Compare(run[it.pos[r]].Key, it.runs[best][it.pos[best]].Key) > 0 {
			best = r
		}
	}
	if best < 0 {
		return TypedRowEdit{}, false
	}
	edit = it.runs[best][it.pos[best]]
	if it.start != nil && bytes.Compare(edit.Key, it.start) < 0 {
		for r := range it.pos {
			it.pos[r] = -1
		}
		return TypedRowEdit{}, false
	}
	for r, run := range it.runs {
		if it.pos[r] >= 0 && bytes.Equal(run[it.pos[r]].Key, edit.Key) {
			it.pos[r]--
		}
	}
	return edit, true
}
