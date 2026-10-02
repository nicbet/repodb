package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"sort"

	"github.com/dolthub/go-mysql-server/sql"

	"github.com/nicbet/repodb/common/prolly"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/common/storage"
)

// Range scans. Keys are order-preserving (keycodec.go), so a SQL range over
// index columns maps to one half-open interval of encoded keys. Scans stream
// rows in key order, merging the base Prolly tree with the pending overlay,
// and decode only the rows they return, so LIMIT stops early.

// keyInterval is [start, end) in encoded key space. A nil end is unbounded.
type keyInterval struct{ start, end []byte }

func (iv keyInterval) empty() bool { return iv.end != nil && bytes.Compare(iv.start, iv.end) >= 0 }

func (iv keyInterval) contains(key []byte) bool {
	return bytes.Compare(key, iv.start) >= 0 && (iv.end == nil || bytes.Compare(key, iv.end) < 0)
}

// rangePartition scans intervals of the primary key (index == nil) or of a
// secondary index, in ascending key order, or descending when reverse.
type rangePartition struct {
	intervals []keyInterval
	index     *indexDisk
	reverse   bool
}

// scanOrder returns intervals in the order a scan visits them: as given
// (ascending, non-overlapping) or last to first.
func scanOrder(intervals []keyInterval, reverse bool) []keyInterval {
	if !reverse {
		return intervals
	}
	out := slices.Clone(intervals)
	slices.Reverse(out)
	return out
}

// before reports whether key a comes before key b in a scan's direction.
func before(a, b string, reverse bool) bool {
	if reverse {
		return a > b
	}
	return a < b
}

func (rangePartition) Key() []byte { return []byte("range") }

func isUnboundedLower(cut sql.MySQLRangeCut) bool {
	_, ok := cut.(sql.BelowNull)
	return ok
}

// rangeShapeSupported reports whether a range has the shape an index scan
// answers exactly: point columns, then at most one ranged column, then only
// unconstrained columns.
func rangeShapeSupported(r sql.MySQLRange, nullable bool) bool {
	ranged := false
	for _, col := range r {
		switch {
		case isAllColumn(col, nullable):
			ranged = true // every later column must be unconstrained too
		case ranged:
			return false
		case isPointColumn(col):
		default:
			ranged = true
		}
	}
	return true
}

func isAllColumn(col sql.MySQLRangeColumnExpr, nullable bool) bool {
	if _, ok := col.UpperBound.(sql.AboveAll); !ok {
		return false
	}
	if isUnboundedLower(col.LowerBound) {
		return true
	}
	_, aboveNull := col.LowerBound.(sql.AboveNull)
	return aboveNull && !nullable
}

func isPointColumn(col sql.MySQLRangeColumnExpr) bool {
	if _, ok := col.LowerBound.(sql.BelowNull); ok {
		_, upperNull := col.UpperBound.(sql.AboveNull)
		return upperNull
	}
	lower, lok := col.LowerBound.(sql.Below)
	upper, uok := col.UpperBound.(sql.Above)
	return lok && uok && reflect.DeepEqual(lower.Key, upper.Key)
}

// rangeIntervals converts a range collection over the given column types to
// sorted, merged key intervals. nullable selects secondary-index encoding
// (0x00 NULL / 0x01 value markers per column).
func rangeIntervals(ctx context.Context, columnTypes []sql.Type, nullable bool, ranges sql.MySQLRangeCollection) ([]keyInterval, error) {
	var out []keyInterval
	for _, r := range ranges {
		if len(r) > len(columnTypes) {
			return nil, errors.New("range has more columns than the index")
		}
		iv, none, err := rangeInterval(ctx, columnTypes, nullable, r)
		if err != nil {
			return nil, err
		}
		if !none && !iv.empty() {
			out = append(out, iv)
		}
	}
	return mergeIntervals(out), nil
}

// rangeInterval converts one range to a key interval. none reports a range
// that selects nothing, which an interval cannot always express: its nil start
// means "from the first key", and there is no key after an all-0xFF key.
func rangeInterval(ctx context.Context, columnTypes []sql.Type, nullable bool, r sql.MySQLRange) (iv keyInterval, none bool, err error) {
	var prefix []byte
	for i, col := range r {
		if isAllColumn(col, nullable) {
			return keyInterval{start: prefix, end: keyPrefixEnd(prefix)}, false, nil
		}
		if !isPointColumn(col) {
			start, none, err := lowerCutKey(ctx, prefix, columnTypes[i], nullable, col.LowerBound)
			if err != nil || none {
				return keyInterval{}, none, err
			}
			end, err := upperCutKey(ctx, prefix, columnTypes[i], nullable, col.UpperBound)
			if err != nil {
				return keyInterval{}, false, err
			}
			return keyInterval{start: start, end: end}, false, nil
		}
		if _, isNull := col.LowerBound.(sql.BelowNull); isNull {
			if !nullable {
				return keyInterval{}, true, nil // primary keys are never NULL
			}
			prefix = append(prefix, 0x00)
			continue
		}
		if prefix, err = appendBoundValue(ctx, prefix, columnTypes[i], nullable, col.LowerBound.(sql.Below).Key); err != nil {
			return keyInterval{}, false, err
		}
	}
	return keyInterval{start: prefix, end: keyPrefixEnd(prefix)}, false, nil
}

func appendBoundValue(ctx context.Context, prefix []byte, typ sql.Type, nullable bool, key any) ([]byte, error) {
	value, _, err := typ.Convert(ctx, key)
	if err != nil {
		return nil, err
	}
	out := append([]byte(nil), prefix...)
	if nullable {
		out = append(out, 0x01)
	}
	return appendKeyColumn(out, typ, value)
}

// lowerCutKey returns the inclusive start key for a lower bound. none reports
// an exclusive bound past the largest possible key (for example k > MAX of an
// integer key, whose encoding is all 0xFF): no key can follow it, so the range
// is empty. A nil start would instead mean "from the first key".
func lowerCutKey(ctx context.Context, prefix []byte, typ sql.Type, nullable bool, cut sql.MySQLRangeCut) (start []byte, none bool, err error) {
	switch c := cut.(type) {
	case sql.BelowNull:
		return nullMarker(prefix, nullable, 0x00), false, nil
	case sql.AboveNull:
		return nullMarker(prefix, nullable, 0x01), false, nil
	case sql.Below:
		start, err := appendBoundValue(ctx, prefix, typ, nullable, c.Key)
		return start, false, err
	case sql.Above:
		key, err := appendBoundValue(ctx, prefix, typ, nullable, c.Key)
		if err != nil {
			return nil, false, err
		}
		return exclusiveStart(key)
	case sql.AboveAll:
		return exclusiveStart(prefix)
	}
	return nil, false, errors.New("unsupported lower range bound")
}

// exclusiveStart returns the first key after every key starting with prefix,
// or none when no such key exists.
func exclusiveStart(prefix []byte) ([]byte, bool, error) {
	start := keyPrefixEnd(prefix)
	return start, start == nil, nil
}

// upperCutKey returns the exclusive end key for an upper bound (nil: none).
func upperCutKey(ctx context.Context, prefix []byte, typ sql.Type, nullable bool, cut sql.MySQLRangeCut) ([]byte, error) {
	switch c := cut.(type) {
	case sql.AboveAll:
		return keyPrefixEnd(prefix), nil
	case sql.Below:
		return appendBoundValue(ctx, prefix, typ, nullable, c.Key)
	case sql.Above:
		key, err := appendBoundValue(ctx, prefix, typ, nullable, c.Key)
		if err != nil {
			return nil, err
		}
		return keyPrefixEnd(key), nil
	case sql.AboveNull:
		return nullMarker(prefix, nullable, 0x01), nil
	case sql.BelowNull:
		return nullMarker(prefix, nullable, 0x00), nil
	}
	return nil, errors.New("unsupported upper range bound")
}

// nullMarker positions a bound around NULLs. Primary keys have no NULLs or
// markers, so both positions collapse to the prefix itself.
func nullMarker(prefix []byte, nullable bool, marker byte) []byte {
	out := append([]byte(nil), prefix...)
	if nullable {
		out = append(out, marker)
	}
	return out
}

func mergeIntervals(in []keyInterval) []keyInterval {
	if len(in) < 2 {
		return in
	}
	sort.Slice(in, func(i, j int) bool { return bytes.Compare(in[i].start, in[j].start) < 0 })
	out := []keyInterval{in[0]}
	for _, iv := range in[1:] {
		last := &out[len(out)-1]
		if last.end != nil && bytes.Compare(iv.start, last.end) > 0 {
			out = append(out, iv)
			continue
		}
		if last.end != nil && (iv.end == nil || bytes.Compare(iv.end, last.end) > 0) {
			last.end = iv.end
		}
	}
	return out
}

// sortedOverlayKeys returns the keys of an overlay map inside iv, sorted in
// ascending order, or descending when reverse.
func sortedOverlayKeys[V any](overlay map[string]V, iv keyInterval, reverse bool) []string {
	keys := make([]string, 0)
	for key := range overlay {
		if iv.contains([]byte(key)) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if reverse {
		slices.Reverse(keys)
	}
	return keys
}

// overlayCursor yields the keys of an overlay of pending changes in scan
// order.
type overlayCursor interface {
	peek() (key string, ok bool)
	advance() error
}

// keyCursor walks a sorted key slice.
type keyCursor struct {
	keys []string
	pos  int
}

func (c *keyCursor) peek() (string, bool) {
	if c.pos >= len(c.keys) {
		return "", false
	}
	return c.keys[c.pos], true
}

func (c *keyCursor) advance() error {
	c.pos++
	return nil
}

// pendingCursor walks a PendingRows interval. After advance, edit holds the
// edit for the key just passed.
type pendingCursor struct {
	it   *repository.PendingIter
	next repository.TypedRowEdit
	key  string
	ok   bool
	edit repository.TypedRowEdit
}

func newPendingCursor(edits repository.PendingRows, iv keyInterval, reverse bool) *pendingCursor {
	c := &pendingCursor{}
	if !edits.Empty() {
		c.it = pendingIter(edits, iv, reverse)
		c.load()
	}
	return c
}

func pendingIter(edits repository.PendingRows, iv keyInterval, reverse bool) *repository.PendingIter {
	if reverse {
		return edits.IterReverse(iv.start, iv.end)
	}
	return edits.Iter(iv.start, iv.end)
}

func (c *pendingCursor) load() {
	c.next, c.ok = c.it.Next()
	if c.ok {
		c.key = string(c.next.Key)
	}
}

func (c *pendingCursor) peek() (string, bool) { return c.key, c.ok }

func (c *pendingCursor) advance() error {
	c.edit = c.next
	c.load()
	return nil
}

// rowOverlayCursor merges a transaction's own row edits with the journal's
// pending edits in scan order; the transaction's edit wins for equal keys.
// After advance, edit holds the edit for the key just passed.
type rowOverlayCursor struct {
	state      *tableState
	reverse    bool
	local      keyCursor
	pending    *repository.PendingIter
	pendingKey string
	pendingRow repository.TypedRowEdit
	pendingOK  bool
	edit       rowEdit
}

func newRowOverlayCursor(state *tableState, iv keyInterval, reverse bool) *rowOverlayCursor {
	c := &rowOverlayCursor{state: state, reverse: reverse, local: keyCursor{keys: sortedOverlayKeys(state.edits, iv, reverse)}}
	if !state.pending.Empty() {
		c.pending = pendingIter(state.pending, iv, reverse)
		c.nextPending()
	}
	return c
}

func (c *rowOverlayCursor) nextPending() {
	c.pendingRow, c.pendingOK = c.pending.Next()
	if c.pendingOK {
		c.pendingKey = string(c.pendingRow.Key)
	}
}

func (c *rowOverlayCursor) peek() (string, bool) {
	local, localOK := c.local.peek()
	switch {
	case !c.pendingOK:
		return local, localOK
	case !localOK || before(c.pendingKey, local, c.reverse):
		return c.pendingKey, true
	default:
		return local, true
	}
}

func (c *rowOverlayCursor) advance() error {
	local, localOK := c.local.peek()
	if localOK && (!c.pendingOK || !before(c.pendingKey, local, c.reverse)) {
		if c.pendingOK && local == c.pendingKey {
			c.nextPending()
		}
		c.edit = c.state.edits[local]
		return c.local.advance()
	}
	edit, err := c.state.decodePending(c.pendingRow)
	if err != nil {
		return err
	}
	c.edit = edit
	c.nextPending()
	return nil
}

// mergedEntries walks one interval of a Prolly tree merged with an overlay of
// pending changes, ascending or, when reverse, descending. Overlay entries win
// over tree entries with the same key; the overlay must yield keys in the same
// direction.
type mergedEntries struct {
	ctx      context.Context
	tree     prolly.EntryIterator
	treeNext prolly.Entry
	treeOK   bool
	overlay  overlayCursor
	iv       keyInterval
	reverse  bool
}

func newMergedEntries(ctx context.Context, store storage.Store, root storage.Hash, iv keyInterval, overlay overlayCursor, reverse bool) (*mergedEntries, error) {
	m := &mergedEntries{ctx: ctx, overlay: overlay, iv: iv, reverse: reverse}
	if root.Valid() {
		tree, err := prolly.Open(store, root)
		if err != nil {
			return nil, err
		}
		if reverse {
			m.tree, err = tree.ReverseIterator(ctx, iv.end)
		} else {
			m.tree, err = tree.IteratorFrom(ctx, iv.start)
		}
		if err != nil {
			return nil, err
		}
		if err := m.advanceTree(); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *mergedEntries) advanceTree() error {
	entry, ok, err := m.tree.Next()
	if err != nil {
		return err
	}
	if ok && m.reverse && bytes.Compare(entry.Key, m.iv.start) < 0 {
		ok = false
	} else if ok && !m.reverse && m.iv.end != nil && bytes.Compare(entry.Key, m.iv.end) >= 0 {
		ok = false
	}
	m.treeNext, m.treeOK = entry, ok
	return nil
}

// next advances to the next key. fromOverlay reports whether it is an overlay
// key, returned in key, for the caller to look up; otherwise value is the tree
// entry's value and key is empty.
func (m *mergedEntries) next() (key string, value []byte, fromOverlay, ok bool, err error) {
	overlayKey, hasOverlay := m.overlay.peek()
	switch {
	case !hasOverlay && !m.treeOK:
		return "", nil, false, false, nil
	case hasOverlay && (!m.treeOK || !before(string(m.treeNext.Key), overlayKey, m.reverse)):
		key = overlayKey
		if err := m.overlay.advance(); err != nil {
			return "", nil, false, false, err
		}
		if m.treeOK && key == string(m.treeNext.Key) {
			if err := m.advanceTree(); err != nil {
				return "", nil, false, false, err
			}
		}
		return key, nil, true, true, nil
	default:
		// Callers use the key only for overlay entries, so a tree entry's key
		// is not converted.
		value = m.treeNext.Value
		if err := m.advanceTree(); err != nil {
			return "", nil, false, false, err
		}
		return key, value, false, true, nil
	}
}

func (m *mergedEntries) close() error {
	if m.tree != nil {
		return m.tree.Close()
	}
	return nil
}

// primaryRowIter streams table rows in primary-key order over intervals.
type primaryRowIter struct {
	state      *tableState
	reverse    bool
	projection *rowProjection
	intervals  []keyInterval
	next       int
	cur        *mergedEntries
	overlay    *rowOverlayCursor
	// When the transaction has materialized every row (state.rows), rows are
	// served from it; keys holds the matching keys in order.
	keys []string
}

func newPrimaryRowIter(state *tableState, intervals []keyInterval, projection *rowProjection, reverse bool) *primaryRowIter {
	intervals = scanOrder(intervals, reverse)
	it := &primaryRowIter{state: state, intervals: intervals, projection: projection, reverse: reverse}
	if state.rows != nil {
		for _, iv := range intervals {
			it.keys = append(it.keys, sortedOverlayKeys(state.rows, iv, reverse)...)
		}
	}
	return it
}

func (it *primaryRowIter) Next(ctx *sql.Context) (sql.Row, error) {
	if it.state.rows != nil {
		if it.next >= len(it.keys) {
			return nil, io.EOF
		}
		key := it.keys[it.next]
		it.next++
		performanceCounters.rowsScanned.Add(1)
		return it.row(it.state.rows[key]), nil
	}
	for {
		if it.cur == nil {
			if it.next >= len(it.intervals) {
				return nil, io.EOF
			}
			iv := it.intervals[it.next]
			it.next++
			it.overlay = newRowOverlayCursor(it.state, iv, it.reverse)
			cur, err := newMergedEntries(ctx, it.state.store, it.state.manifest.DataRoot, iv, it.overlay, it.reverse)
			if err != nil {
				return nil, err
			}
			it.cur = cur
		}
		_, value, fromOverlay, ok, err := it.cur.next()
		if err != nil {
			return nil, err
		}
		if !ok {
			if err := it.cur.close(); err != nil {
				return nil, err
			}
			it.cur = nil
			continue
		}
		if fromOverlay {
			edit := it.overlay.edit
			if edit.delete {
				continue
			}
			performanceCounters.rowsScanned.Add(1)
			return it.row(edit.row), nil
		}
		// Stored keys are not re-derived from rows here: ValidateSnapshot
		// checks every stored row's key once per snapshot.
		row, err := decodeRowProjected(it.state.schema.Schema, value, it.projection)
		if err != nil {
			return nil, err
		}
		performanceCounters.rowsDecoded.Add(1)
		performanceCounters.rowsScanned.Add(1)
		return row, nil
	}
}

// row returns a caller-owned copy of an in-memory row, projected.
func (it *primaryRowIter) row(row sql.Row) sql.Row {
	if it.projection != nil {
		return it.projection.project(row)
	}
	return cloneRow(row)
}

func (it *primaryRowIter) Close(*sql.Context) error {
	if it.cur != nil {
		return it.cur.close()
	}
	return nil
}

// indexRowIter streams rows in secondary-index order over intervals of the
// index's key space, merging the index tree with its pending edits.
type indexRowIter struct {
	state      *tableState
	reverse    bool
	projection *rowProjection
	def        *indexDisk
	intervals  []keyInterval
	next       int
	cur        *mergedEntries
	overlay    repository.PendingRows
	cursor     *pendingCursor
}

func newIndexRowIter(ctx context.Context, state *tableState, def *indexDisk, intervals []keyInterval, projection *rowProjection, reverse bool) (*indexRowIter, error) {
	if err := state.ensureIndexEdits(ctx); err != nil {
		return nil, err
	}
	var overlay repository.PendingRows
	if edits := state.idxEdits[def.Name]; edits != nil {
		overlay = edits.view()
	}
	return &indexRowIter{state: state, reverse: reverse, projection: projection, def: def, intervals: scanOrder(intervals, reverse), overlay: overlay}, nil
}

func (it *indexRowIter) Next(ctx *sql.Context) (sql.Row, error) {
	for {
		if it.cur == nil {
			if it.next >= len(it.intervals) {
				return nil, io.EOF
			}
			iv := it.intervals[it.next]
			it.next++
			it.cursor = newPendingCursor(it.overlay, iv, it.reverse)
			cur, err := newMergedEntries(ctx, it.state.store, it.state.manifest.Indexes[it.def.Name], iv, it.cursor, it.reverse)
			if err != nil {
				return nil, err
			}
			it.cur = cur
		}
		_, pk, fromOverlay, ok, err := it.cur.next()
		if err != nil {
			return nil, err
		}
		if !ok {
			if err := it.cur.close(); err != nil {
				return nil, err
			}
			it.cur = nil
			continue
		}
		if fromOverlay {
			if it.cursor.edit.Delete {
				continue
			}
			pk = it.cursor.edit.Value
		}
		performanceCounters.pointKeysVisited.Add(1)
		row, exists, err := it.state.lookupRow(ctx, string(pk))
		if err != nil {
			return nil, err
		}
		if exists {
			return it.projection.project(row), nil
		}
	}
}

func (it *indexRowIter) Close(*sql.Context) error {
	if it.cur != nil {
		return it.cur.close()
	}
	return nil
}
