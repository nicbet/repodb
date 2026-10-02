package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
	querypb "github.com/dolthub/vitess/go/vt/proto/query"
	"github.com/nicbet/repodb/common/prolly"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/common/storage"
)

type provider struct{ db *database }

func (p *provider) Database(_ *sql.Context, name string) (sql.Database, error) {
	if !strings.EqualFold(name, p.db.name) {
		return nil, sql.ErrDatabaseNotFound.New(name)
	}
	return p.db, nil
}
func (p *provider) HasDatabase(_ *sql.Context, name string) bool {
	return strings.EqualFold(name, p.db.name)
}
func (p *provider) AllDatabases(*sql.Context) []sql.Database { return []sql.Database{p.db} }

type database struct {
	name     string
	repo     *repository.Repository
	working  *repository.WorkingState
	mu       sync.RWMutex
	snapshot *repository.Snapshot

	metadataCache       map[string]cachedTableMeta
	metadataCacheCommit string
	metadataCacheGen    uint64

	// indexEdits caches each table's secondary-index edits for the current
	// snapshot, keyed by table then index. A committing journal transaction
	// stores its own edits here, so the next transaction need not re-derive
	// them from the pending-row overlay.
	indexEditsGen    uint64
	indexEditsCommit string
	indexEdits       map[string]map[string]repository.PendingRows
}

type cachedTableMeta struct {
	schema   sql.PrimaryKeySchema
	checks   []sql.CheckDefinition
	indexes  []indexDisk
	manifest repository.Table
}

func (d *database) Name() string { return d.name }

func (d *database) GetTableInsensitive(ctx *sql.Context, name string) (sql.Table, bool, error) {
	tx, err := transactionFrom(ctx)
	if err != nil {
		return nil, false, err
	}
	for tableName, state := range tx.tables {
		if strings.EqualFold(tableName, name) {
			return &table{db: d, name: tableName, state: state}, true, nil
		}
	}
	return nil, false, nil
}

func (d *database) GetTableNames(ctx *sql.Context) ([]string, error) {
	tx, err := transactionFrom(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(tx.tables))
	for name := range tx.tables {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (d *database) CreateTable(ctx *sql.Context, name string, schema sql.PrimaryKeySchema, _ sql.CollationID, _ string) error {
	tx, err := transactionFrom(ctx)
	if err != nil {
		return err
	}
	if len(schema.PkOrdinals) == 0 {
		return errors.New("RepoDB requires an explicit PRIMARY KEY")
	}
	for existing := range tx.tables {
		if strings.EqualFold(existing, name) {
			return sql.ErrTableAlreadyExists.New(name)
		}
	}
	if err := validateSchema(schema); err != nil {
		return err
	}
	if err := validateKeyColumns(schema.Schema, schema.PkOrdinals); err != nil {
		return err
	}
	tx.tables[name] = &tableState{schema: copySchema(schema), rows: make(map[string]sql.Row), dirty: true}
	tx.dirty = true
	return nil
}

func (d *database) DropTable(ctx *sql.Context, name string) error {
	tx, err := transactionFrom(ctx)
	if err != nil {
		return err
	}
	for existing := range tx.tables {
		if strings.EqualFold(existing, name) {
			delete(tx.tables, existing)
			if tx.dropped == nil {
				tx.dropped = make(map[string]bool)
			}
			tx.dropped[existing] = true
			tx.dirty = true
			return nil
		}
	}
	return sql.ErrTableNotFound.New(name)
}

type transaction struct {
	writer   *repository.Writer
	tables   map[string]*tableState
	dropped  map[string]bool
	dirty    bool
	readOnly bool
}

func (t *transaction) String() string   { return "RepoDB snapshot transaction" }
func (t *transaction) IsReadOnly() bool { return t.readOnly }

type session struct {
	*sql.BaseSession
	db *database
}

func newSession(base *sql.BaseSession, db *database) *session {
	base.SetCurrentDatabase(db.name)
	return &session{BaseSession: base, db: db}
}

func (s *session) StartTransaction(ctx *sql.Context, characteristic sql.TransactionCharacteristic) (sql.Transaction, error) {
	snapshot, err := s.resolveSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	writer, err := s.db.repo.BeginSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	tx := &transaction{writer: writer, tables: make(map[string]*tableState), readOnly: characteristic == sql.ReadOnly}
	s.db.mu.RLock()
	cached := s.db.metadataCache
	cacheHit := cached != nil && s.db.metadataCacheCommit == snapshot.Commit && s.db.metadataCacheGen == snapshot.Generation()
	s.db.mu.RUnlock()
	if cacheHit {
		performanceCounters.metadataCacheHits.Add(1)
		for name, meta := range cached {
			m := meta.manifest
			m.Indexes = copyManifestIndexes(meta.manifest.Indexes)
			tx.tables[name] = &tableState{schema: copySchema(meta.schema), checks: copyChecks(meta.checks), indexes: copyIndexes(meta.indexes), manifest: m, store: snapshot.Store()}
		}
	} else {
		performanceCounters.metadataCacheMisses.Add(1)
		newCache := make(map[string]cachedTableMeta, len(snapshot.Manifest.Tables))
		for name, manifestTable := range snapshot.Manifest.Tables {
			state, err := loadTableMetadata(ctx, snapshot.Store(), manifestTable)
			if err != nil {
				return nil, fmt.Errorf("load table %s: %w", name, err)
			}
			tx.tables[name] = state
			cm := state.manifest
			cm.Indexes = copyManifestIndexes(state.manifest.Indexes)
			newCache[name] = cachedTableMeta{schema: copySchema(state.schema), checks: copyChecks(state.checks), indexes: copyIndexes(state.indexes), manifest: cm}
		}
		s.db.mu.Lock()
		s.db.metadataCache = newCache
		s.db.metadataCacheCommit = snapshot.Commit
		s.db.metadataCacheGen = snapshot.Generation()
		s.db.mu.Unlock()
	}
	// Pending journal edits are shared, not copied: a transaction's own edits
	// go to state.edits, layered over state.pending.
	for name, rows := range snapshot.PendingEdits() {
		if state := tx.tables[name]; state != nil {
			state.pending = rows
		}
	}
	s.db.mu.RLock()
	if s.db.indexEdits != nil && s.db.indexEditsCommit == snapshot.Commit && s.db.indexEditsGen == snapshot.Generation() {
		for name, byIndex := range s.db.indexEdits {
			if state := tx.tables[name]; state != nil {
				state.idxEdits = make(map[string]*indexOverlay, len(byIndex))
				for index, edits := range byIndex {
					state.idxEdits[index] = &indexOverlay{base: edits}
				}
			}
		}
	}
	s.db.mu.RUnlock()
	return tx, nil
}

// indexOverlay is one secondary index's edits since the last checkpoint:
// base is shared between transactions and never modified, and local holds this
// transaction's own edits in order. For each index key the last edit wins.
type indexOverlay struct {
	base  repository.PendingRows
	local []repository.TypedRowEdit
}

func (o *indexOverlay) add(edit prolly.Edit) {
	o.local = append(o.local, repository.TypedRowEdit{Key: edit.Key, Value: edit.Value, Delete: edit.Delete})
}

// get returns the newest edit for key.
func (o *indexOverlay) get(key []byte) (repository.TypedRowEdit, bool) {
	for i := len(o.local) - 1; i >= 0; i-- {
		if bytes.Equal(o.local[i].Key, key) {
			return o.local[i], true
		}
	}
	return o.base.Get(key)
}

// view returns every edit as one set. Without local edits it is base itself.
func (o *indexOverlay) view() repository.PendingRows {
	return o.base.With(o.local)
}

// sorted returns the newest edit per key in key order.
func (o *indexOverlay) sorted() []prolly.Edit {
	view := o.view()
	edits := make([]prolly.Edit, 0, view.Len())
	for it := view.Iter(nil, nil); ; {
		edit, ok := it.Next()
		if !ok {
			return edits
		}
		edits = append(edits, prolly.Edit{Key: edit.Key, Value: edit.Value, Delete: edit.Delete})
	}
}

// uniqueClaims returns the keys this transaction added to unique indexes, so
// that the journal rejects a concurrent transaction that gave another row the
// same value. A key with a NULL contains the primary key and never collides.
func (s *tableState) uniqueClaims() []repository.IndexClaim {
	var claims []repository.IndexClaim
	for _, idx := range s.indexes {
		overlay := s.idxEdits[idx.Name]
		if !idx.Unique || overlay == nil {
			continue
		}
		last := make(map[string]bool, len(overlay.local))
		for _, edit := range overlay.local {
			last[string(edit.Key)] = !edit.Delete
		}
		for key, put := range last {
			if put {
				claims = append(claims, repository.IndexClaim{Index: idx.Name, Key: []byte(key)})
			}
		}
	}
	return claims
}

// indexViews returns each index's edits as one set, for the cache.
func indexViews(overlays map[string]*indexOverlay) map[string]repository.PendingRows {
	views := make(map[string]repository.PendingRows, len(overlays))
	for name, overlay := range overlays {
		views[name] = overlay.view()
	}
	return views
}

// storeIndexEdits caches a committed journal transaction's index edits for the
// generation it produced. They stay valid because a journal commit keeps the
// base Git commit, and with it the persisted data and index trees.
//
// A rebased commit (one that landed on later generations than its base) has
// index edits that miss the commits in between. Its own edits are still right
// on top of them, because it wrote no row they wrote: they are added to the
// cache of the generation it landed on, if that is cached.
func (d *database) storeIndexEdits(tx *transaction, next *repository.Snapshot) {
	base := tx.writer.BaseSnapshot()
	d.mu.Lock()
	defer d.mu.Unlock()
	if next.Commit != base.Commit {
		d.indexEdits = nil
		return
	}
	rebased := next.Generation() != base.Generation()+1
	var carried map[string]map[string]repository.PendingRows
	if d.indexEditsCommit == base.Commit && d.indexEditsGen == next.Generation()-1 {
		carried = d.indexEdits
	}
	if rebased && carried == nil {
		return
	}
	cache := make(map[string]map[string]repository.PendingRows, len(tx.tables))
	for name, state := range tx.tables {
		switch {
		case tx.dropped[name] || state.schemaDirty:
		case rebased:
			// Views this transaction derived for a table it only read
			// are of its base generation: only carried views are current.
			if !state.dirty {
				if carried[name] != nil {
					cache[name] = carried[name]
				}
			} else if views := rebaseIndexViews(carried[name], state.idxEdits); views != nil {
				cache[name] = views
			}
		case state.idxEdits != nil:
			cache[name] = indexViews(state.idxEdits)
		case !state.dirty && carried[name] != nil:
			cache[name] = carried[name]
		}
	}
	d.indexEdits = cache
	d.indexEditsCommit = next.Commit
	d.indexEditsGen = next.Generation()
}

// rebaseIndexViews returns current's index edits with a rebased
// transaction's own edits added, or nil if either lacks an index.
func rebaseIndexViews(current map[string]repository.PendingRows, overlays map[string]*indexOverlay) map[string]repository.PendingRows {
	if current == nil || overlays == nil || len(current) != len(overlays) {
		return nil
	}
	views := make(map[string]repository.PendingRows, len(overlays))
	for name, overlay := range overlays {
		view, ok := current[name]
		if !ok {
			return nil
		}
		views[name] = view.With(overlay.local)
	}
	return views
}

// rememberIndexEdits caches index edits that a clean transaction derived, so
// later readers of the same generation reuse them. Entries already cached for
// that generation are kept.
func (d *database) rememberIndexEdits(tx *transaction) {
	base := tx.writer.BaseSnapshot()
	var derived map[string]*tableState
	for name, state := range tx.tables {
		if state.idxEdits != nil {
			if derived == nil {
				derived = make(map[string]*tableState)
			}
			derived[name] = state
		}
	}
	if derived == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.indexEdits == nil || d.indexEditsCommit != base.Commit || d.indexEditsGen != base.Generation() {
		d.indexEdits = make(map[string]map[string]repository.PendingRows, len(derived))
		d.indexEditsCommit = base.Commit
		d.indexEditsGen = base.Generation()
	}
	for name, state := range derived {
		if _, cached := d.indexEdits[name]; !cached {
			d.indexEdits[name] = indexViews(state.idxEdits)
		}
	}
}

// resolveSnapshot returns the current valid snapshot. It checks on-disk state
// every time, so writes by other engines and processes are visible; the checks
// are cheap while nothing has changed (a journal stat, and a ref storage stamp
// in place of a Git process).
func (s *session) resolveSnapshot(ctx *sql.Context) (*repository.Snapshot, error) {
	s.db.mu.RLock()
	snapshot := s.db.snapshot
	s.db.mu.RUnlock()
	var current *repository.Snapshot
	if s.db.working != nil {
		var err error
		if current, err = s.db.working.Current(ctx); err != nil {
			return nil, err
		}
	} else {
		head, err := s.db.repo.Head(ctx)
		if err != nil {
			return nil, err
		}
		if head == "" {
			return nil, repository.ErrNotInitialized
		}
		if snapshot != nil && snapshot.Commit == head && snapshot.Generation() == 0 {
			current = snapshot
		} else if current, err = s.db.repo.SnapshotCommit(ctx, head); err != nil {
			return nil, err
		}
	}
	if snapshot == nil || snapshot.Commit != current.Commit || snapshot.Generation() != current.Generation() {
		snapshot = current
		if err := ValidateSnapshot(ctx, snapshot); err != nil {
			return nil, err
		}
	}
	s.db.mu.Lock()
	s.db.snapshot = snapshot
	s.db.mu.Unlock()
	return snapshot, nil
}

func (s *session) CommitTransaction(ctx *sql.Context, opaque sql.Transaction) error {
	tx, ok := opaque.(*transaction)
	if !ok {
		return fmt.Errorf("invalid RepoDB transaction %T", opaque)
	}
	if !tx.dirty {
		if s.db.working != nil {
			s.db.rememberIndexEdits(tx)
		}
		return nil
	}
	var err error
	if s.db.working != nil {
		err = s.commitTypedEdits(ctx, tx)
	} else {
		err = s.commitNativeGit(ctx, tx)
	}
	if err != nil {
		// go-mysql-server clears the transaction only after a successful
		// commit, but a RepoDB transaction cannot commit twice. Drop it, as
		// MySQL rolls back a failed commit, so the next statement (or the
		// application's retry) starts fresh on the current snapshot.
		ctx.SetTransaction(nil)
		ctx.SetIgnoreAutoCommit(false)
	}
	return err
}

func (s *session) commitTypedEdits(ctx *sql.Context, tx *transaction) error {
	buildStarted := time.Now()
	var edits []repository.TypedTableEdit
	for name, state := range tx.tables {
		if !state.dirty {
			continue
		}
		te := repository.TypedTableEdit{Table: name}
		if !state.manifest.SchemaRoot.Valid() || state.schemaDirty {
			schemaData, err := encodeSchema(state.schema, state.checks, state.indexes)
			if err != nil {
				return err
			}
			te.Schema = schemaData
		}
		// Only this transaction's own edits: replay merges records key by key,
		// so the pending edits it read are already in the journal.
		for key, edit := range state.edits {
			re := repository.TypedRowEdit{Key: []byte(key), Delete: edit.delete}
			if !edit.delete {
				value, err := encodeRow(state.schema.Schema, edit.row)
				if err != nil {
					return fmt.Errorf("encode table %s row: %w", name, err)
				}
				re.Value = value
			}
			te.Edits = append(te.Edits, re)
		}
		te.Claims = state.uniqueClaims()
		edits = append(edits, te)
	}
	for name := range tx.dropped {
		edits = append(edits, repository.TypedTableEdit{Table: name, Drop: true})
	}
	performanceCounters.snapshotBuildNanos.Add(uint64(time.Since(buildStarted)))
	snapshot, _, err := s.db.working.CommitTypedEdits(ctx, tx.writer.BaseSnapshot(), edits)
	if snapshot != nil {
		s.db.mu.Lock()
		s.db.snapshot = snapshot
		s.db.mu.Unlock()
		if err == nil {
			s.db.storeIndexEdits(tx, snapshot)
		}
	}
	return err
}

func (s *session) commitNativeGit(ctx *sql.Context, tx *transaction) error {
	buildStarted := time.Now()
	manifest := repository.Manifest{DefaultDatabase: s.db.name, Tables: make(map[string]repository.Table, len(tx.tables))}
	reachable := make(map[storage.Hash]struct{})
	for name, state := range tx.tables {
		if !state.dirty {
			performanceCounters.tablesReused.Add(1)
			manifest.Tables[name] = state.manifest
			objects, cached := validatedTableObjects(tx.writer.BaseSnapshot(), name)
			if !cached {
				hashes, err := prolly.Reachable(ctx, tx.writer.BaseSnapshot().Store(), state.manifest.DataRoot)
				if err != nil {
					return err
				}
				objects = append([]storage.Hash{state.manifest.SchemaRoot}, hashes...)
				for _, idxRoot := range state.manifest.Indexes {
					idxHashes, err := prolly.Reachable(ctx, tx.writer.BaseSnapshot().Store(), idxRoot)
					if err != nil {
						return err
					}
					objects = append(objects, idxHashes...)
				}
			}
			for _, hash := range objects {
				reachable[hash] = struct{}{}
			}
			continue
		}
		performanceCounters.tablesRebuilt.Add(1)
		if state.manifest.DataRoot.Valid() {
			edits := make([]prolly.Edit, 0, len(state.edits))
			if err := state.forEachEdit(func(key string, edit rowEdit) error {
				item := prolly.Edit{Key: []byte(key), Delete: edit.delete}
				if !edit.delete {
					value, err := encodeRow(state.schema.Schema, edit.row)
					if err != nil {
						return fmt.Errorf("encode table %s row: %w", name, err)
					}
					item.Value = value
				}
				edits = append(edits, item)
				return nil
			}); err != nil {
				return err
			}
			sort.Slice(edits, func(i, j int) bool { return bytes.Compare(edits[i].Key, edits[j].Key) < 0 })
			baseTree, err := prolly.Open(tx.writer.BaseSnapshot().Store(), state.manifest.DataRoot)
			if err != nil {
				return err
			}
			phaseStarted := time.Now()
			tree, err := prolly.Apply(ctx, tx.writer, baseTree, edits)
			performanceCounters.treeMutationNanos.Add(uint64(time.Since(phaseStarted)))
			if err != nil {
				return err
			}
			phaseStarted = time.Now()
			hashes, err := prolly.Reachable(ctx, tx.writer, tree.Root())
			performanceCounters.reachabilityNanos.Add(uint64(time.Since(phaseStarted)))
			if err != nil {
				return err
			}
			for _, hash := range hashes {
				reachable[hash] = struct{}{}
			}
			schemaRoot := state.manifest.SchemaRoot
			if state.schemaDirty {
				sd, err := encodeSchema(state.schema, state.checks, state.indexes)
				if err != nil {
					return err
				}
				schemaRoot, err = tx.writer.Put(ctx, sd)
				if err != nil {
					return err
				}
			}
			reachable[schemaRoot] = struct{}{}
			tbl := repository.Table{SchemaRoot: schemaRoot, DataRoot: tree.Root()}
			idxRoots, idxHashes, err := commitIndexTrees(ctx, tx.writer, state, tx.writer.BaseSnapshot().Store())
			if err != nil {
				return err
			}
			if idxRoots != nil {
				tbl.Indexes = idxRoots
				for _, h := range idxHashes {
					reachable[h] = struct{}{}
				}
			}
			manifest.Tables[name] = tbl
			continue
		}
		schemaData, err := encodeSchema(state.schema, state.checks, state.indexes)
		if err != nil {
			return err
		}
		schemaRoot, err := tx.writer.Put(ctx, schemaData)
		if err != nil {
			return err
		}
		reachable[schemaRoot] = struct{}{}
		entries := make([]prolly.Entry, 0, len(state.rows))
		for key, row := range state.rows {
			value, err := encodeRow(state.schema.Schema, row)
			if err != nil {
				return fmt.Errorf("encode table %s row: %w", name, err)
			}
			entries = append(entries, prolly.Entry{Key: []byte(key), Value: value})
		}
		tree, err := prolly.Build(ctx, tx.writer, entries, prolly.DefaultOptions)
		if err != nil {
			return err
		}
		hashes, err := prolly.Reachable(ctx, tx.writer, tree.Root())
		if err != nil {
			return err
		}
		for _, hash := range hashes {
			reachable[hash] = struct{}{}
		}
		tbl := repository.Table{SchemaRoot: schemaRoot, DataRoot: tree.Root()}
		idxRoots, idxHashes, err := commitIndexTrees(ctx, tx.writer, state, tx.writer.BaseSnapshot().Store())
		if err != nil {
			return err
		}
		if idxRoots != nil {
			tbl.Indexes = idxRoots
			for _, h := range idxHashes {
				reachable[h] = struct{}{}
			}
		}
		manifest.Tables[name] = tbl
	}
	hashes := make([]storage.Hash, 0, len(reachable))
	for hash := range reachable {
		hashes = append(hashes, hash)
	}
	if err := tx.writer.RetainOnly(hashes); err != nil {
		return err
	}
	performanceCounters.snapshotBuildNanos.Add(uint64(time.Since(buildStarted)))
	result, err := tx.writer.CommitWithOutcome(ctx, manifest)
	if result.Snapshot != nil {
		s.db.mu.Lock()
		s.db.snapshot = result.Snapshot
		s.db.mu.Unlock()
	}
	return err
}

// commitIndexTrees builds or applies pending index edits for a dirty table,
// returning the index root hashes and all reachable objects.
func commitIndexTrees(ctx context.Context, writer storage.Store, state *tableState, baseStore storage.Store) (map[string]storage.Hash, []storage.Hash, error) {
	if len(state.indexes) == 0 {
		return nil, nil, nil
	}
	roots := make(map[string]storage.Hash, len(state.indexes))
	var allHashes []storage.Hash
	for _, idx := range state.indexes {
		var edits []prolly.Edit
		if overlay := state.idxEdits[idx.Name]; overlay != nil {
			edits = overlay.sorted()
		}
		existingRoot, hasExisting := state.manifest.Indexes[idx.Name]
		if hasExisting && existingRoot.Valid() && len(edits) > 0 {
			baseTree, err := prolly.Open(baseStore, existingRoot)
			if err != nil {
				return nil, nil, err
			}
			tree, err := prolly.Apply(ctx, writer, baseTree, edits)
			if err != nil {
				return nil, nil, err
			}
			roots[idx.Name] = tree.Root()
		} else if hasExisting && existingRoot.Valid() && len(edits) == 0 {
			roots[idx.Name] = existingRoot
		} else {
			root, err := buildIndexTreeForDef(ctx, writer, state, idx)
			if err != nil {
				return nil, nil, err
			}
			if !root.Valid() {
				continue
			}
			roots[idx.Name] = root
		}
		if roots[idx.Name].Valid() {
			hashes, err := prolly.Reachable(ctx, writer, roots[idx.Name])
			if err != nil {
				return nil, nil, err
			}
			allHashes = append(allHashes, hashes...)
		}
	}
	return roots, allHashes, nil
}

// buildIndexTreeForDef builds a single index tree from scratch by scanning all rows.
func buildIndexTreeForDef(ctx context.Context, writer storage.Store, state *tableState, idx indexDisk) (storage.Hash, error) {
	if err := state.ensureRows(ctx); err != nil {
		return "", err
	}
	if len(state.rows) == 0 {
		return "", nil
	}
	entries := make([]prolly.Entry, 0, len(state.rows))
	for pkStr, row := range state.rows {
		pk := []byte(pkStr)
		idxKey, _, err := encodeIndexKey(state.schema, row, idx.Columns, pk, idx.Unique)
		if err != nil {
			return "", err
		}
		entries = append(entries, prolly.Entry{Key: idxKey, Value: pk})
	}
	sort.Slice(entries, func(i, j int) bool { return bytes.Compare(entries[i].Key, entries[j].Key) < 0 })
	tree, err := prolly.Build(ctx, writer, entries, prolly.DefaultOptions)
	if err != nil {
		return "", err
	}
	return tree.Root(), nil
}

func (s *session) Rollback(*sql.Context, sql.Transaction) error { return nil }
func (s *session) CreateSavepoint(*sql.Context, sql.Transaction, string) error {
	return errors.New("RepoDB does not support savepoints")
}
func (s *session) RollbackToSavepoint(*sql.Context, sql.Transaction, string) error {
	return errors.New("RepoDB does not support savepoints")
}
func (s *session) ReleaseSavepoint(*sql.Context, sql.Transaction, string) error {
	return errors.New("RepoDB does not support savepoints")
}

func transactionFrom(ctx *sql.Context) (*transaction, error) {
	tx, ok := ctx.GetTransaction().(*transaction)
	if ok && tx != nil {
		return tx, nil
	}
	sess, ok := ctx.Session.(*session)
	if !ok {
		return nil, errors.New("RepoDB SQL operation has no active transaction")
	}
	opaque, err := sess.StartTransaction(ctx, sql.ReadWrite)
	if err != nil {
		return nil, err
	}
	ctx.SetTransaction(opaque)
	return opaque.(*transaction), nil
}

type tableState struct {
	schema   sql.PrimaryKeySchema
	checks   []sql.CheckDefinition
	indexes  []indexDisk
	idxEdits map[string]*indexOverlay
	rows     map[string]sql.Row
	manifest repository.Table
	store    storage.Store
	// pending holds the journal's row edits since the last checkpoint. It is
	// shared between transactions and never modified; values are encoded rows.
	pending repository.PendingRows
	// edits holds this transaction's own row edits, which win over pending.
	edits       map[string]rowEdit
	dirty       bool
	schemaDirty bool
}

type rowEdit struct {
	row    sql.Row
	delete bool
}

func (s *tableState) ensureRows(ctx context.Context) error {
	if s.rows != nil {
		return nil
	}
	if !s.manifest.DataRoot.Valid() {
		s.rows = make(map[string]sql.Row, len(s.edits))
		return s.forEachEdit(func(key string, edit rowEdit) error {
			if !edit.delete {
				s.rows[key] = cloneRow(edit.row)
			}
			return nil
		})
	}
	tree, err := prolly.Open(s.store, s.manifest.DataRoot)
	if err != nil {
		return err
	}
	entries, err := tree.Entries(ctx)
	if err != nil {
		return err
	}
	performanceCounters.rowsDecoded.Add(uint64(len(entries)))
	s.rows = make(map[string]sql.Row, len(entries))
	for _, entry := range entries {
		row, err := decodeRow(s.schema.Schema, entry.Value)
		if err != nil {
			return err
		}
		key, err := encodeKey(s.schema, row)
		if err != nil || !bytes.Equal(key, entry.Key) {
			return errors.New("stored row key does not match row primary key")
		}
		s.rows[string(entry.Key)] = row
	}
	return s.forEachEdit(func(key string, edit rowEdit) error {
		if edit.delete {
			delete(s.rows, key)
		} else {
			s.rows[key] = cloneRow(edit.row)
		}
		return nil
	})
}

// edit returns the overlay edit for key: the transaction's own, else the
// pending journal edit.
func (s *tableState) edit(key string) (rowEdit, bool, error) {
	if edit, ok := s.edits[key]; ok {
		return edit, true, nil
	}
	if s.pending.Empty() {
		return rowEdit{}, false, nil
	}
	pending, ok := s.pending.Get([]byte(key))
	if !ok {
		return rowEdit{}, false, nil
	}
	edit, err := s.decodePending(pending)
	return edit, true, err
}

func (s *tableState) decodePending(pending repository.TypedRowEdit) (rowEdit, error) {
	if pending.Delete {
		return rowEdit{delete: true}, nil
	}
	row, err := decodeRow(s.schema.Schema, pending.Value)
	if err != nil {
		return rowEdit{}, fmt.Errorf("decode pending edit: %w", err)
	}
	return rowEdit{row: row}, nil
}

// hasEdits reports whether the overlay holds any edit.
func (s *tableState) hasEdits() bool { return len(s.edits) != 0 || !s.pending.Empty() }

// forEachEdit calls fn once per overlay key with the winning edit, in no
// particular order.
func (s *tableState) forEachEdit(fn func(key string, edit rowEdit) error) error {
	if !s.pending.Empty() {
		it := s.pending.Iter(nil, nil)
		for {
			pending, ok := it.Next()
			if !ok {
				break
			}
			key := string(pending.Key)
			if _, local := s.edits[key]; local {
				continue
			}
			edit, err := s.decodePending(pending)
			if err != nil {
				return err
			}
			if err := fn(key, edit); err != nil {
				return err
			}
		}
	}
	for key, edit := range s.edits {
		if err := fn(key, edit); err != nil {
			return err
		}
	}
	return nil
}

// overlayDeletes returns the overlay's deletes, for DDL that re-keys a table.
// Rows are not decoded, so it is safe while the schema is being changed.
func (s *tableState) overlayDeletes() map[string]rowEdit {
	deletes := make(map[string]rowEdit)
	if !s.pending.Empty() {
		for it := s.pending.Iter(nil, nil); ; {
			pending, ok := it.Next()
			if !ok {
				break
			}
			if pending.Delete {
				deletes[string(pending.Key)] = rowEdit{delete: true}
			}
		}
	}
	for key, edit := range s.edits {
		if edit.delete {
			deletes[key] = edit
		} else {
			delete(deletes, key)
		}
	}
	return deletes
}

func (s *tableState) lookupRow(ctx context.Context, key string) (sql.Row, bool, error) {
	if s.rows != nil {
		row, ok := s.rows[key]
		return cloneRow(row), ok, nil
	}
	if edit, ok, err := s.edit(key); err != nil || ok {
		return cloneRow(edit.row), ok && !edit.delete, err
	}
	if !s.manifest.DataRoot.Valid() {
		return nil, false, nil
	}
	tree, err := prolly.Open(s.store, s.manifest.DataRoot)
	if err != nil {
		return nil, false, err
	}
	value, err := tree.Get(ctx, []byte(key))
	if errors.Is(err, prolly.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	row, err := decodeRow(s.schema.Schema, value)
	return row, err == nil, err
}

func (s *tableState) setEdit(key string, edit rowEdit) {
	if s.edits == nil {
		s.edits = make(map[string]rowEdit)
	}
	s.edits[key] = rowEdit{row: cloneRow(edit.row), delete: edit.delete}
	if s.rows != nil {
		if edit.delete {
			delete(s.rows, key)
		} else {
			s.rows[key] = cloneRow(edit.row)
		}
	}
}

type table struct {
	db    *database
	name  string
	state *tableState
	// projectedNames is the column projection go-mysql-server pushed down, or
	// nil; projection maps it onto the schema, nil when it is every column.
	projectedNames []string
	projection     *rowProjection
}

func (t *table) Name() string   { return t.name }
func (t *table) String() string { return t.name }
func (t *table) Schema(*sql.Context) sql.Schema {
	schema := t.state.schema.Schema
	if t.projection != nil {
		projected := make(sql.Schema, len(t.projection.ordinals))
		for i, ordinal := range t.projection.ordinals {
			projected[i] = schema[ordinal]
		}
		schema = projected
	}
	return sourcedSchema(schema, t.name)
}

// WithProjections returns the table reading only the named columns, so scans
// decode no other cells. go-mysql-server applies it only below read-only
// nodes; writes always see full rows.
func (t *table) WithProjections(_ *sql.Context, colNames []string) (sql.Table, error) {
	ordinals := make([]int, len(colNames))
	for i, name := range colNames {
		ordinals[i] = t.state.schema.Schema.IndexOfColName(name)
		if ordinals[i] < 0 {
			return nil, fmt.Errorf("column %s not found", name)
		}
	}
	projected := *t
	projected.projectedNames = append([]string{}, colNames...)
	projected.projection = newRowProjection(len(t.state.schema.Schema), ordinals)
	return &projected, nil
}

func (t *table) Projections() []string { return t.projectedNames }

// RowCount reports the table's row count without scanning. It is exact when
// every row is materialized, or when the transaction sees no overlay edits:
// the data tree's root records the number of entries below it. With overlay
// edits it returns an estimate, so go-mysql-server does not answer COUNT(*)
// from it. go-mysql-server calls it while planning every query.
func (t *table) RowCount(ctx *sql.Context) (uint64, bool, error) {
	s := t.state
	if s.rows != nil {
		return uint64(len(s.rows)), true, nil
	}
	var count uint64
	if s.manifest.DataRoot.Valid() {
		tree, err := prolly.Open(s.store, s.manifest.DataRoot)
		if err != nil {
			return 0, false, err
		}
		if count, err = tree.Count(ctx); err != nil {
			return 0, false, err
		}
	}
	if s.hasEdits() {
		return count + uint64(len(s.edits)+s.pending.Len()), false, nil
	}
	return count, true, nil
}

// DataLength is not tracked.
func (t *table) DataLength(*sql.Context) (uint64, error) { return 0, nil }
func (t *table) PrimaryKeySchema(*sql.Context) sql.PrimaryKeySchema {
	return sql.PrimaryKeySchema{Schema: sourcedSchema(t.state.schema.Schema, t.name), PkOrdinals: append([]int(nil), t.state.schema.PkOrdinals...)}
}
func (t *table) Collation() sql.CollationID { return sql.Collation_Default }
func (t *table) Partitions(*sql.Context) (sql.PartitionIter, error) {
	return sql.PartitionsToPartitionIter(singlePartition{}), nil
}

func (t *table) GetIndexes(*sql.Context) ([]sql.Index, error) {
	indexes := []sql.Index{&primaryIndex{table: t}}
	for i := range t.state.indexes {
		indexes = append(indexes, &secondaryIndex{table: t, def: &t.state.indexes[i]})
	}
	return indexes, nil
}

func (t *table) CreateIndex(ctx *sql.Context, def sql.IndexDef) error {
	for _, existing := range t.state.indexes {
		if existing.Name == def.Name {
			return fmt.Errorf("index %s already exists", def.Name)
		}
	}
	ordinals := make([]int, len(def.Columns))
	for i, col := range def.Columns {
		found := false
		for j, sc := range t.state.schema.Schema {
			if strings.EqualFold(sc.Name, col.Name) {
				ordinals[i] = j
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("column %s not found", col.Name)
		}
	}
	if err := rejectIndexPrefixes(def.Columns); err != nil {
		return err
	}
	idx := indexDisk{Name: def.Name, Columns: ordinals, Unique: def.IsUnique()}
	if def.IsUnique() {
		if err := t.state.ensureRows(ctx); err != nil {
			return err
		}
		seen := make(map[string]struct{})
		for pkStr, row := range t.state.rows {
			pk := []byte(pkStr)
			idxKey, hasNull, err := encodeIndexKey(t.state.schema, row, ordinals, pk, true)
			if err != nil {
				return err
			}
			if !hasNull {
				keyStr := string(idxKey)
				if _, dup := seen[keyStr]; dup {
					return sql.NewUniqueKeyErr(def.Name, false, row)
				}
				seen[keyStr] = struct{}{}
			}
		}
	}
	t.state.indexes = append(t.state.indexes, idx)
	t.state.schemaDirty = true
	t.state.dirty = true
	tx, _ := transactionFrom(ctx)
	tx.dirty = true
	return nil
}

func (t *table) DropIndex(ctx *sql.Context, indexName string) error {
	found := false
	for i, idx := range t.state.indexes {
		if idx.Name == indexName {
			t.state.indexes = append(t.state.indexes[:i], t.state.indexes[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("index %s not found", indexName)
	}
	delete(t.state.idxEdits, indexName)
	t.state.schemaDirty = true
	t.state.dirty = true
	tx, _ := transactionFrom(ctx)
	tx.dirty = true
	return nil
}

func (t *table) RenameIndex(ctx *sql.Context, fromName string, toName string) error {
	for i, idx := range t.state.indexes {
		if idx.Name == fromName {
			t.state.indexes[i].Name = toName
			if edits, ok := t.state.idxEdits[fromName]; ok {
				t.state.idxEdits[toName] = edits
				delete(t.state.idxEdits, fromName)
			}
			t.state.schemaDirty = true
			t.state.dirty = true
			tx, _ := transactionFrom(ctx)
			tx.dirty = true
			return nil
		}
	}
	return fmt.Errorf("index %s not found", fromName)
}

func (t *table) IndexedAccess(_ *sql.Context, lookup sql.IndexLookup) sql.IndexedTable {
	return t
}

func (*table) PreciseMatch() bool { return true }

// IsTemporary reports that RepoDB tables are never temporary. go-mysql-server's
// read-only transaction check calls it on every table a write touches and
// dereferences a nil interface for tables that do not implement
// sql.TemporaryTable.
func (*table) IsTemporary() bool { return false }

func (t *table) LookupPartitions(ctx *sql.Context, lookup sql.IndexLookup) (sql.PartitionIter, error) {
	if lookup.IsEmptyRange {
		return sql.PartitionsToPartitionIter(pointPartition{}), nil
	}
	ranges, ok := lookup.Ranges.(sql.MySQLRangeCollection)
	if !ok {
		return nil, errors.New("RepoDB index supports point lookups only")
	}
	if _, isPrimary := lookup.Index.(*primaryIndex); isPrimary {
		if allFullPointRanges(ranges, len(t.state.schema.PkOrdinals)) {
			return t.lookupPrimaryPartitions(ctx, ranges, lookup.IsReverse)
		}
		intervals, err := rangeIntervals(ctx, t.columnTypes(t.state.schema.PkOrdinals), false, ranges)
		if err != nil {
			return nil, err
		}
		return sql.PartitionsToPartitionIter(rangePartition{intervals: intervals, reverse: lookup.IsReverse}), nil
	}
	if si, isSecondary := lookup.Index.(*secondaryIndex); isSecondary {
		intervals, err := rangeIntervals(ctx, t.columnTypes(si.def.Columns), true, ranges)
		if err != nil {
			return nil, err
		}
		return sql.PartitionsToPartitionIter(rangePartition{intervals: intervals, index: si.def, reverse: lookup.IsReverse}), nil
	}
	return nil, fmt.Errorf("unsupported index type %T", lookup.Index)
}

func (t *table) columnTypes(ordinals []int) []sql.Type {
	out := make([]sql.Type, len(ordinals))
	for i, ordinal := range ordinals {
		out[i] = t.state.schema.Schema[ordinal].Type
	}
	return out
}

// allFullPointRanges reports whether every range fixes every one of n columns
// to a non-NULL value, which the primary-key point path answers directly.
func allFullPointRanges(ranges sql.MySQLRangeCollection, n int) bool {
	for _, r := range ranges {
		if len(r) != n {
			return false
		}
		for _, col := range r {
			if _, isNull := col.LowerBound.(sql.BelowNull); isNull || !isPointColumn(col) {
				return false
			}
		}
	}
	return true
}

func (t *table) lookupPrimaryPartitions(ctx *sql.Context, ranges sql.MySQLRangeCollection, reverse bool) (sql.PartitionIter, error) {
	keys := make([]string, 0, len(ranges))
	for _, indexRange := range ranges {
		row := make(sql.Row, len(t.state.schema.Schema))
		if len(indexRange) != len(t.state.schema.PkOrdinals) {
			return nil, errors.New("incomplete RepoDB primary-key lookup")
		}
		for i, ordinal := range t.state.schema.PkOrdinals {
			lower, lok := indexRange[i].LowerBound.(sql.Below)
			upper, uok := indexRange[i].UpperBound.(sql.Above)
			if !lok || !uok || !reflect.DeepEqual(lower.Key, upper.Key) {
				return nil, errors.New("RepoDB primary index received a non-point range")
			}
			value, _, err := t.state.schema.Schema[ordinal].Type.Convert(ctx, lower.Key)
			if err != nil {
				return nil, err
			}
			row[ordinal] = value
		}
		key, err := encodeKey(t.state.schema, row)
		if err != nil {
			return nil, err
		}
		keys = append(keys, string(key))
	}
	// Keys are order-preserving: sorted keys return rows in index order.
	sort.Strings(keys)
	keys = slices.Compact(keys)
	if reverse {
		slices.Reverse(keys)
	}
	return sql.PartitionsToPartitionIter(pointPartition{keys: keys}), nil
}

func (t *table) PartitionRows(ctx *sql.Context, partition sql.Partition) (sql.RowIter, error) {
	return t.partitionRows(ctx, partition)
}

func (t *table) partitionRows(ctx context.Context, partition sql.Partition) (sql.RowIter, error) {
	if point, ok := partition.(pointPartition); ok {
		performanceCounters.pointKeysVisited.Add(uint64(len(point.keys)))
		rows := make([]sql.Row, 0, len(point.keys))
		for _, key := range point.keys {
			row, exists, err := t.state.lookupRow(ctx, key)
			if err != nil {
				return nil, err
			}
			if exists {
				rows = append(rows, t.projection.project(row))
			}
		}
		return sql.RowsToRowIter(rows...), nil
	}
	if rp, ok := partition.(rangePartition); ok {
		if rp.index != nil {
			return newIndexRowIter(ctx, t.state, rp.index, rp.intervals, t.projection, rp.reverse)
		}
		return newPrimaryRowIter(t.state, rp.intervals, t.projection, rp.reverse), nil
	}
	// A full scan streams every row in primary-key order.
	return newPrimaryRowIter(t.state, []keyInterval{{}}, t.projection, false), nil
}
func (t *table) Inserter(*sql.Context) sql.RowInserter { return &editor{table: t} }
func (t *table) Updater(*sql.Context) sql.RowUpdater   { return &editor{table: t} }
func (t *table) Deleter(*sql.Context) sql.RowDeleter   { return &editor{table: t} }

func (t *table) AddColumn(ctx *sql.Context, column *sql.Column, order *sql.ColumnOrder) error {
	tx, err := transactionFrom(ctx)
	if err != nil {
		return err
	}
	if err := validateSchema(sql.PrimaryKeySchema{Schema: sql.Schema{column}}); err != nil {
		return err
	}
	if column.PrimaryKey {
		return fmt.Errorf("cannot add primary key column %s via ALTER TABLE", column.Name)
	}
	if err := t.state.ensureRows(ctx); err != nil {
		return err
	}
	if !column.Nullable && column.Default == nil && len(t.state.rows) > 0 {
		return fmt.Errorf("cannot add NOT NULL column %s to table with existing rows without a DEFAULT", column.Name)
	}
	newSchema := make(sql.Schema, len(t.state.schema.Schema)+1)
	insertAt := len(t.state.schema.Schema)
	if order != nil {
		if order.First {
			insertAt = 0
		} else if order.AfterColumn != "" {
			found := false
			for i, col := range t.state.schema.Schema {
				if strings.EqualFold(col.Name, order.AfterColumn) {
					insertAt = i + 1
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("column %s not found", order.AfterColumn)
			}
		}
	}
	copy(newSchema, t.state.schema.Schema[:insertAt])
	newSchema[insertAt] = column
	copy(newSchema[insertAt+1:], t.state.schema.Schema[insertAt:])
	newPK := make([]int, len(t.state.schema.PkOrdinals))
	for i, ord := range t.state.schema.PkOrdinals {
		if ord >= insertAt {
			newPK[i] = ord + 1
		} else {
			newPK[i] = ord
		}
	}
	newRows := make(map[string]sql.Row, len(t.state.rows))
	for key, row := range t.state.rows {
		newRow := make(sql.Row, len(row)+1)
		copy(newRow, row[:insertAt])
		newRow[insertAt] = nil
		copy(newRow[insertAt+1:], row[insertAt:])
		newRows[key] = newRow
	}
	t.state.schema = sql.PrimaryKeySchema{Schema: newSchema, PkOrdinals: newPK}
	for i := range t.state.indexes {
		for j, ord := range t.state.indexes[i].Columns {
			if ord >= insertAt {
				t.state.indexes[i].Columns[j] = ord + 1
			}
		}
	}
	t.state.idxEdits = nil
	t.state.rows = newRows
	t.state.pending = repository.PendingRows{}
	t.state.edits = make(map[string]rowEdit)
	for key, row := range newRows {
		t.state.edits[key] = rowEdit{row: cloneRow(row)}
	}
	t.state.dirty = true
	t.state.schemaDirty = true
	tx.dirty = true
	return nil
}

// rekeyRows re-keys rows under schema. Only the delete edits of oldEdits are
// used: they are carried over.
func rekeyRows(schema sql.PrimaryKeySchema, oldRows map[string]sql.Row, oldEdits map[string]rowEdit) (map[string]sql.Row, map[string]rowEdit, error) {
	newRows := make(map[string]sql.Row, len(oldRows))
	for _, row := range oldRows {
		key, err := encodeKey(schema, row)
		if err != nil {
			return nil, nil, fmt.Errorf("re-key failed: %w", err)
		}
		keyStr := string(key)
		if _, dup := newRows[keyStr]; dup {
			return nil, nil, fmt.Errorf("primary key column change produces duplicate key")
		}
		newRows[keyStr] = row
	}
	newEdits := make(map[string]rowEdit, len(newRows)+len(oldRows))
	for key, edit := range oldEdits {
		if edit.delete {
			newEdits[key] = edit
		}
	}
	for oldKey := range oldRows {
		if _, exists := newRows[oldKey]; !exists {
			newEdits[oldKey] = rowEdit{delete: true}
		}
	}
	for newKey, row := range newRows {
		newEdits[newKey] = rowEdit{row: cloneRow(row)}
	}
	return newRows, newEdits, nil
}

func (t *table) DropColumn(ctx *sql.Context, columnName string) error {
	tx, err := transactionFrom(ctx)
	if err != nil {
		return err
	}
	dropIdx := -1
	for i, col := range t.state.schema.Schema {
		if strings.EqualFold(col.Name, columnName) {
			dropIdx = i
			break
		}
	}
	if dropIdx < 0 {
		return fmt.Errorf("column %s not found", columnName)
	}
	isPKColumn := false
	for _, ord := range t.state.schema.PkOrdinals {
		if ord == dropIdx {
			isPKColumn = true
			break
		}
	}
	if isPKColumn && len(t.state.schema.PkOrdinals) == 1 {
		return fmt.Errorf("cannot drop the only primary key column %s", columnName)
	}
	for _, idx := range t.state.indexes {
		for _, ord := range idx.Columns {
			if ord == dropIdx {
				return fmt.Errorf("cannot drop column %s: used by index %s", columnName, idx.Name)
			}
		}
	}
	if err := t.state.ensureRows(ctx); err != nil {
		return err
	}
	newSchema := make(sql.Schema, 0, len(t.state.schema.Schema)-1)
	newSchema = append(newSchema, t.state.schema.Schema[:dropIdx]...)
	newSchema = append(newSchema, t.state.schema.Schema[dropIdx+1:]...)
	var newPK []int
	if isPKColumn {
		newPK = make([]int, 0, len(t.state.schema.PkOrdinals)-1)
		for _, ord := range t.state.schema.PkOrdinals {
			if ord == dropIdx {
				continue
			}
			if ord > dropIdx {
				newPK = append(newPK, ord-1)
			} else {
				newPK = append(newPK, ord)
			}
		}
	} else {
		newPK = make([]int, len(t.state.schema.PkOrdinals))
		for i, ord := range t.state.schema.PkOrdinals {
			if ord > dropIdx {
				newPK[i] = ord - 1
			} else {
				newPK[i] = ord
			}
		}
	}
	candidateRows := make(map[string]sql.Row, len(t.state.rows))
	for key, row := range t.state.rows {
		newRow := make(sql.Row, len(row)-1)
		copy(newRow, row[:dropIdx])
		copy(newRow[dropIdx:], row[dropIdx+1:])
		candidateRows[key] = newRow
	}
	candidateSchema := sql.PrimaryKeySchema{Schema: newSchema, PkOrdinals: newPK}
	if isPKColumn {
		overlay := t.state.overlayDeletes()
		reKeyedRows, reKeyedEdits, err := rekeyRows(candidateSchema, candidateRows, overlay)
		if err != nil {
			return err
		}
		t.state.schema = candidateSchema
		for i := range t.state.indexes {
			for j, ord := range t.state.indexes[i].Columns {
				if ord > dropIdx {
					t.state.indexes[i].Columns[j] = ord - 1
				}
			}
		}
		t.state.rows = reKeyedRows
		t.state.pending = repository.PendingRows{}
		t.state.edits = reKeyedEdits
		t.state.idxEdits = nil
		t.state.manifest.Indexes = nil
	} else {
		t.state.schema = candidateSchema
		for i := range t.state.indexes {
			for j, ord := range t.state.indexes[i].Columns {
				if ord > dropIdx {
					t.state.indexes[i].Columns[j] = ord - 1
				}
			}
		}
		t.state.idxEdits = nil
		t.state.rows = candidateRows
		t.state.pending = repository.PendingRows{}
		t.state.edits = make(map[string]rowEdit)
		for key, row := range candidateRows {
			t.state.edits[key] = rowEdit{row: cloneRow(row)}
		}
	}
	t.state.dirty = true
	t.state.schemaDirty = true
	tx.dirty = true
	return nil
}

func (t *table) ModifyColumn(ctx *sql.Context, columnName string, column *sql.Column, order *sql.ColumnOrder) error {
	tx, err := transactionFrom(ctx)
	if err != nil {
		return err
	}
	if err := validateSchema(sql.PrimaryKeySchema{Schema: sql.Schema{column}}); err != nil {
		return err
	}
	colIdx := -1
	for i, col := range t.state.schema.Schema {
		if strings.EqualFold(col.Name, columnName) {
			colIdx = i
			break
		}
	}
	if colIdx < 0 {
		return fmt.Errorf("column %s not found", columnName)
	}
	isPKColumn := false
	for _, ord := range t.state.schema.PkOrdinals {
		if ord == colIdx {
			isPKColumn = true
			break
		}
	}
	if err := t.state.ensureRows(ctx); err != nil {
		return err
	}
	if !column.Nullable && t.state.schema.Schema[colIdx].Nullable {
		for _, row := range t.state.rows {
			if row[colIdx] == nil {
				return fmt.Errorf("cannot change column %s to NOT NULL: existing rows contain NULL values", columnName)
			}
		}
	}
	typeChanged := !t.state.schema.Schema[colIdx].Type.Equals(column.Type)
	if typeChanged {
		oldEnum, oldIsEnum := t.state.schema.Schema[colIdx].Type.(sql.EnumType)
		convertedRows := make(map[string]any, len(t.state.rows))
		for key, row := range t.state.rows {
			if row[colIdx] != nil {
				val := row[colIdx]
				if oldIsEnum {
					if idx, ok := val.(uint16); ok {
						str, _ := oldEnum.At(int(idx))
						val = str
					}
				}
				converted, inRange, err := column.Type.Convert(ctx, val)
				if err != nil {
					return fmt.Errorf("cannot convert column %s value: %w", columnName, err)
				}
				if inRange != sql.InRange {
					return fmt.Errorf("value out of range for column %s new type", columnName)
				}
				convertedRows[key] = converted
			}
		}
		candidateSchema := copySchema(t.state.schema)
		candidateSchema.Schema[colIdx] = column
		for _, idx := range t.state.indexes {
			affectsIdx := false
			for _, ord := range idx.Columns {
				if ord == colIdx {
					affectsIdx = true
					break
				}
			}
			if !affectsIdx {
				continue
			}
			if idx.Unique {
				seen := make(map[string]struct{}, len(t.state.rows))
				for pkStr, row := range t.state.rows {
					pk := []byte(pkStr)
					candidateRow := cloneRow(row)
					if v, ok := convertedRows[pkStr]; ok {
						candidateRow[colIdx] = v
					}
					idxKey, hasNull, err := encodeIndexKey(candidateSchema, candidateRow, idx.Columns, pk, idx.Unique)
					if err != nil {
						return err
					}
					if !hasNull {
						keyStr := string(idxKey)
						if _, dup := seen[keyStr]; dup {
							return fmt.Errorf("column type change on %s creates duplicate in unique index %s", columnName, idx.Name)
						}
						seen[keyStr] = struct{}{}
					}
				}
			}
		}
		if isPKColumn {
			candidateSchema := copySchema(t.state.schema)
			candidateSchema.Schema[colIdx] = column
			candidateRows := make(map[string]sql.Row, len(t.state.rows))
			for key, row := range t.state.rows {
				r := cloneRow(row)
				if v, ok := convertedRows[key]; ok {
					r[colIdx] = v
				}
				candidateRows[key] = r
			}
			overlay := t.state.overlayDeletes()
			if _, _, err := rekeyRows(candidateSchema, candidateRows, overlay); err != nil {
				return err
			}
		}
		for key, converted := range convertedRows {
			row := t.state.rows[key]
			row[colIdx] = converted
			t.state.rows[key] = row
		}
		t.state.idxEdits = nil
		for _, idx := range t.state.indexes {
			for _, ord := range idx.Columns {
				if ord == colIdx {
					delete(t.state.manifest.Indexes, idx.Name)
					break
				}
			}
		}
	}
	t.state.schema.Schema[colIdx] = column
	if order != nil {
		col := t.state.schema.Schema[colIdx]
		reduced := make(sql.Schema, 0, len(t.state.schema.Schema)-1)
		reduced = append(reduced, t.state.schema.Schema[:colIdx]...)
		reduced = append(reduced, t.state.schema.Schema[colIdx+1:]...)
		insertAt := len(reduced)
		if order.First {
			insertAt = 0
		} else if order.AfterColumn != "" {
			for i, c := range reduced {
				if strings.EqualFold(c.Name, order.AfterColumn) {
					insertAt = i + 1
					break
				}
			}
		}
		final := make(sql.Schema, len(reduced)+1)
		copy(final, reduced[:insertAt])
		final[insertAt] = col
		copy(final[insertAt+1:], reduced[insertAt:])
		t.state.schema.Schema = final
		oldPK := t.state.schema.PkOrdinals
		newPK := make([]int, len(oldPK))
		for i, ord := range oldPK {
			r := ord
			if ord > colIdx {
				r = ord - 1
			} else if ord == colIdx {
				newPK[i] = insertAt
				continue
			}
			if r >= insertAt {
				r++
			}
			newPK[i] = r
		}
		t.state.schema.PkOrdinals = newPK
		for ii := range t.state.indexes {
			for jj, ord := range t.state.indexes[ii].Columns {
				r := ord
				if ord > colIdx {
					r = ord - 1
				} else if ord == colIdx {
					t.state.indexes[ii].Columns[jj] = insertAt
					continue
				}
				if r >= insertAt {
					r++
				}
				t.state.indexes[ii].Columns[jj] = r
			}
		}
		t.state.idxEdits = nil
		for key, row := range t.state.rows {
			cell := row[colIdx]
			reducedRow := make(sql.Row, len(row)-1)
			copy(reducedRow, row[:colIdx])
			copy(reducedRow[colIdx:], row[colIdx+1:])
			newRow := make(sql.Row, len(reducedRow)+1)
			copy(newRow, reducedRow[:insertAt])
			newRow[insertAt] = cell
			copy(newRow[insertAt+1:], reducedRow[insertAt:])
			t.state.rows[key] = newRow
		}
	}
	if isPKColumn && typeChanged {
		overlay := t.state.overlayDeletes()
		reKeyedRows, reKeyedEdits, err := rekeyRows(t.state.schema, t.state.rows, overlay)
		if err != nil {
			return err
		}
		t.state.rows = reKeyedRows
		t.state.pending = repository.PendingRows{}
		t.state.edits = reKeyedEdits
		t.state.idxEdits = nil
		t.state.manifest.Indexes = nil
	} else {
		t.state.pending = repository.PendingRows{}
		t.state.edits = make(map[string]rowEdit)
		for key, row := range t.state.rows {
			t.state.edits[key] = rowEdit{row: cloneRow(row)}
		}
	}
	t.state.dirty = true
	t.state.schemaDirty = true
	tx.dirty = true
	return nil
}

func (t *table) GetChecks(_ *sql.Context) ([]sql.CheckDefinition, error) {
	return t.state.checks, nil
}

func (t *table) CreateCheck(ctx *sql.Context, check *sql.CheckDefinition) error {
	tx, err := transactionFrom(ctx)
	if err != nil {
		return err
	}
	chk := *check
	if chk.Name == "" {
		chk.Name = t.generateCheckName()
	} else {
		for _, existing := range t.state.checks {
			if strings.EqualFold(existing.Name, chk.Name) {
				return fmt.Errorf("duplicate check constraint name '%s'", chk.Name)
			}
		}
	}
	t.state.checks = append(copyChecks(t.state.checks), chk)
	t.state.dirty = true
	t.state.schemaDirty = true
	tx.dirty = true
	return nil
}

func (t *table) generateCheckName() string {
	max := 0
	prefix := strings.ToLower(t.name) + "_chk_"
	for _, chk := range t.state.checks {
		lower := strings.ToLower(chk.Name)
		if strings.HasPrefix(lower, prefix) {
			var v int
			if n, _ := fmt.Sscanf(lower[len(prefix):], "%d", &v); n == 1 && v > max {
				max = v
			}
		}
	}
	return fmt.Sprintf("%s%d", prefix, max+1)
}

func (t *table) DropCheck(ctx *sql.Context, chName string) error {
	tx, err := transactionFrom(ctx)
	if err != nil {
		return err
	}
	for i, chk := range t.state.checks {
		if strings.EqualFold(chk.Name, chName) {
			newChecks := make([]sql.CheckDefinition, 0, len(t.state.checks)-1)
			newChecks = append(newChecks, t.state.checks[:i]...)
			newChecks = append(newChecks, t.state.checks[i+1:]...)
			t.state.checks = newChecks
			t.state.dirty = true
			t.state.schemaDirty = true
			tx.dirty = true
			return nil
		}
	}
	return fmt.Errorf("check constraint %s not found", chName)
}

type singlePartition struct{}
type pointPartition struct{ keys []string }

func sourcedSchema(schema sql.Schema, source string) sql.Schema {
	result := make(sql.Schema, len(schema))
	for i, column := range schema {
		copy := *column
		copy.Source = source
		result[i] = &copy
	}
	return result
}

func (singlePartition) Key() []byte  { return []byte("all") }
func (p pointPartition) Key() []byte { return []byte(strings.Join(p.keys, "\x00")) }

type primaryIndex struct{ table *table }

func (*primaryIndex) ID() string              { return "PRIMARY" }
func (i *primaryIndex) Database() string      { return i.table.db.name }
func (i *primaryIndex) Table() string         { return i.table.name }
func (*primaryIndex) IsUnique() bool          { return true }
func (*primaryIndex) IsSpatial() bool         { return false }
func (*primaryIndex) IsFullText() bool        { return false }
func (*primaryIndex) IsVector() bool          { return false }
func (*primaryIndex) Comment() string         { return "" }
func (*primaryIndex) IndexType() string       { return "BTREE" }
func (*primaryIndex) IsGenerated() bool       { return false }
func (*primaryIndex) PrefixLengths() []uint16 { return nil }

// CoversColumns reports false: index lookups always fetch full rows, so no
// index is covering.
func (*primaryIndex) CoversColumns([]string) bool           { return false }
func (*primaryIndex) CanSupportOrderBy(sql.Expression) bool { return false }
func (i *primaryIndex) Expressions() []string {
	expressions := make([]string, len(i.table.state.schema.PkOrdinals))
	for n, ordinal := range i.table.state.schema.PkOrdinals {
		expressions[n] = i.table.name + "." + i.table.state.schema.Schema[ordinal].Name
	}
	return expressions
}
func (i *primaryIndex) ColumnExpressionTypes(*sql.Context) []sql.ColumnExpressionType {
	types := make([]sql.ColumnExpressionType, len(i.table.state.schema.PkOrdinals))
	for n, ordinal := range i.table.state.schema.PkOrdinals {
		types[n] = sql.ColumnExpressionType{Expression: i.Expressions()[n], Type: i.table.state.schema.Schema[ordinal].Type}
	}
	return types
}
func (*primaryIndex) CanSupport(_ *sql.Context, ranges ...sql.Range) bool {
	return canSupportRanges(ranges, false)
}

// Order and Reversible make the primary index an sql.OrderedIndex: keys are
// order-preserving, so scans return rows in ascending primary-key order, or
// descending for a reverse lookup, and the planner can drop matching sorts.
func (*primaryIndex) Order(*sql.Context) sql.IndexOrder { return sql.IndexOrderAsc }
func (*primaryIndex) Reversible(*sql.Context) bool      { return true }

func canSupportRanges(ranges []sql.Range, nullable bool) bool {
	for _, candidate := range ranges {
		r, ok := candidate.(sql.MySQLRange)
		if !ok || len(r) == 0 || !rangeShapeSupported(r, nullable) {
			return false
		}
	}
	return true
}

type secondaryIndex struct {
	table *table
	def   *indexDisk
}

func (s *secondaryIndex) ID() string                          { return s.def.Name }
func (s *secondaryIndex) Database() string                    { return s.table.db.name }
func (s *secondaryIndex) Table() string                       { return s.table.name }
func (s *secondaryIndex) IsUnique() bool                      { return s.def.Unique }
func (*secondaryIndex) IsSpatial() bool                       { return false }
func (*secondaryIndex) IsFullText() bool                      { return false }
func (*secondaryIndex) IsVector() bool                        { return false }
func (*secondaryIndex) Comment() string                       { return "" }
func (*secondaryIndex) IndexType() string                     { return "BTREE" }
func (*secondaryIndex) IsGenerated() bool                     { return false }
func (*secondaryIndex) PrefixLengths() []uint16               { return nil }
func (*secondaryIndex) CoversColumns([]string) bool           { return false }
func (*secondaryIndex) CanSupportOrderBy(sql.Expression) bool { return false }
func (s *secondaryIndex) Expressions() []string {
	exprs := make([]string, len(s.def.Columns))
	for i, ordinal := range s.def.Columns {
		exprs[i] = s.table.name + "." + s.table.state.schema.Schema[ordinal].Name
	}
	return exprs
}
func (s *secondaryIndex) ColumnExpressionTypes(*sql.Context) []sql.ColumnExpressionType {
	cets := make([]sql.ColumnExpressionType, len(s.def.Columns))
	for i, ordinal := range s.def.Columns {
		cets[i] = sql.ColumnExpressionType{Expression: s.Expressions()[i], Type: s.table.state.schema.Schema[ordinal].Type}
	}
	return cets
}
func (*secondaryIndex) CanSupport(_ *sql.Context, ranges ...sql.Range) bool {
	return canSupportRanges(ranges, true)
}

func (*secondaryIndex) Order(*sql.Context) sql.IndexOrder { return sql.IndexOrderAsc }
func (*secondaryIndex) Reversible(*sql.Context) bool      { return true }

type editor struct {
	table  *table
	before map[string]beforeRow
	// idxSnapshot records each index's local edit count at StatementBegin;
	// local edits are append-only, so undo truncates back to it.
	idxSnapshot map[string]int
	idxInitErr  error
}

type beforeRow struct {
	row         sql.Row
	present     bool
	edit        rowEdit
	editPresent bool
}

func (e *editor) StatementBegin(ctx *sql.Context) {
	e.before = make(map[string]beforeRow)
	e.idxInitErr = nil
	e.idxSnapshot = nil
	if len(e.table.state.indexes) > 0 {
		if err := e.table.state.ensureIndexEdits(ctx); err != nil {
			e.idxInitErr = err
			return
		}
	}
	e.idxSnapshot = make(map[string]int, len(e.table.state.idxEdits))
	for name, overlay := range e.table.state.idxEdits {
		e.idxSnapshot[name] = len(overlay.local)
	}
}
func (e *editor) StatementComplete(*sql.Context) error {
	e.before = nil
	e.idxSnapshot = nil
	return nil
}
func (e *editor) DiscardChanges(*sql.Context, error) error {
	for key, before := range e.before {
		if before.editPresent {
			e.table.state.edits[key] = before.edit
		} else {
			delete(e.table.state.edits, key)
		}
		if e.table.state.rows == nil {
			continue
		}
		if before.present {
			e.table.state.rows[key] = before.row
		} else {
			delete(e.table.state.rows, key)
		}
	}
	if e.idxSnapshot != nil {
		for name, overlay := range e.table.state.idxEdits {
			n, ok := e.idxSnapshot[name]
			if !ok {
				// Created by this statement's first edit of the index.
				delete(e.table.state.idxEdits, name)
				continue
			}
			overlay.local = overlay.local[:n]
		}
	}
	e.before = nil
	e.idxSnapshot = nil
	return nil
}
func (e *editor) Close(*sql.Context) error { return nil }
func (e *editor) Insert(ctx *sql.Context, row sql.Row) error {
	if e.idxInitErr != nil {
		return e.idxInitErr
	}
	key, err := encodeKey(e.table.state.schema, row)
	if err != nil {
		return err
	}
	existing, ok, err := e.table.state.lookupRow(ctx, string(key))
	if err != nil {
		return err
	}
	if ok {
		return sql.NewUniqueKeyErr(base64.RawStdEncoding.EncodeToString(key), true, existing)
	}
	if err := e.table.state.ensureIndexEdits(ctx); err != nil {
		return err
	}
	for _, idx := range e.table.state.indexes {
		idxKey, hasNull, err := encodeIndexKey(e.table.state.schema, row, idx.Columns, key, idx.Unique)
		if err != nil {
			return err
		}
		if idx.Unique && !hasNull {
			found, err := e.table.state.lookupIndexKey(ctx, idx.Name, idxKey)
			if err != nil {
				return err
			}
			if found {
				return sql.NewUniqueKeyErr(idx.Name, false, row)
			}
		}
		e.table.state.addIndexEdit(idx.Name, prolly.Edit{Key: idxKey, Value: key})
	}
	e.remember(string(key), nil, false)
	e.table.state.setEdit(string(key), rowEdit{row: row})
	tx, _ := transactionFrom(ctx)
	tx.dirty = true
	e.table.state.dirty = true
	return nil
}
func (e *editor) Delete(ctx *sql.Context, row sql.Row) error {
	if e.idxInitErr != nil {
		return e.idxInitErr
	}
	key, err := encodeKey(e.table.state.schema, row)
	if err != nil {
		return err
	}
	existing, ok, err := e.table.state.lookupRow(ctx, string(key))
	if err != nil {
		return err
	}
	if !ok {
		return sql.ErrDeleteRowNotFound.New()
	}
	for _, idx := range e.table.state.indexes {
		idxKey, _, err := encodeIndexKey(e.table.state.schema, existing, idx.Columns, key, idx.Unique)
		if err != nil {
			return err
		}
		e.table.state.addIndexEdit(idx.Name, prolly.Edit{Key: idxKey, Delete: true})
	}
	e.remember(string(key), existing, true)
	e.table.state.setEdit(string(key), rowEdit{delete: true})
	tx, _ := transactionFrom(ctx)
	tx.dirty = true
	e.table.state.dirty = true
	return nil
}
func (e *editor) Update(ctx *sql.Context, oldRow, newRow sql.Row) error {
	if e.idxInitErr != nil {
		return e.idxInitErr
	}
	if err := e.table.state.ensureIndexEdits(ctx); err != nil {
		return err
	}
	oldKey, err := encodeKey(e.table.state.schema, oldRow)
	if err != nil {
		return err
	}
	newKey, err := encodeKey(e.table.state.schema, newRow)
	if err != nil {
		return err
	}
	if bytes.Equal(oldKey, newKey) && reflect.DeepEqual(oldRow, newRow) {
		return nil
	}
	existing, ok, err := e.table.state.lookupRow(ctx, string(oldKey))
	if err != nil {
		return err
	}
	if !ok {
		return sql.ErrDeleteRowNotFound.New()
	}
	e.remember(string(oldKey), existing, true)
	if !bytes.Equal(oldKey, newKey) {
		newExisting, ok, err := e.table.state.lookupRow(ctx, string(newKey))
		if err != nil {
			return err
		}
		if ok {
			return sql.NewUniqueKeyErr(base64.RawStdEncoding.EncodeToString(newKey), true, newExisting)
		}
		e.remember(string(newKey), nil, false)
		e.table.state.setEdit(string(oldKey), rowEdit{delete: true})
	}
	for _, idx := range e.table.state.indexes {
		oldIdxKey, _, err := encodeIndexKey(e.table.state.schema, existing, idx.Columns, oldKey, idx.Unique)
		if err != nil {
			return err
		}
		newIdxKey, hasNull, err := encodeIndexKey(e.table.state.schema, newRow, idx.Columns, newKey, idx.Unique)
		if err != nil {
			return err
		}
		if bytes.Equal(oldIdxKey, newIdxKey) {
			if !bytes.Equal(oldKey, newKey) {
				e.table.state.addIndexEdit(idx.Name, prolly.Edit{Key: newIdxKey, Value: newKey})
			}
			continue
		}
		if idx.Unique && !hasNull {
			found, err := e.table.state.lookupIndexKey(ctx, idx.Name, newIdxKey)
			if err != nil {
				return err
			}
			if found {
				return sql.NewUniqueKeyErr(idx.Name, false, newRow)
			}
		}
		e.table.state.addIndexEdit(idx.Name, prolly.Edit{Key: oldIdxKey, Delete: true})
		e.table.state.addIndexEdit(idx.Name, prolly.Edit{Key: newIdxKey, Value: newKey})
	}
	e.table.state.setEdit(string(newKey), rowEdit{row: newRow})
	tx, _ := transactionFrom(ctx)
	tx.dirty = true
	e.table.state.dirty = true
	return nil
}

func (e *editor) remember(key string, row sql.Row, present bool) {
	if e.before == nil {
		return
	}
	if _, ok := e.before[key]; ok {
		return
	}
	edit, editPresent := e.table.state.edits[key]
	e.before[key] = beforeRow{row: cloneRow(row), present: present, edit: edit, editPresent: editPresent}
	performanceCounters.undoRowsCaptured.Add(1)
}

type checkDisk struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
	Enforced   bool   `json:"enforced"`
}
type indexDisk struct {
	Name    string `json:"name"`
	Columns []int  `json:"columns"`
	Unique  bool   `json:"unique"`
}
type schemaDisk struct {
	Columns []columnDisk `json:"columns"`
	PK      []int        `json:"primary_key"`
	Checks  []checkDisk  `json:"checks,omitempty"`
	Indexes []indexDisk  `json:"indexes,omitempty"`
}
type columnDisk struct {
	Name         string   `json:"name"`
	Type         int32    `json:"type"`
	Length       int64    `json:"length,omitempty"`
	Precision    int      `json:"precision,omitempty"`
	Scale        int      `json:"scale,omitempty"`
	Collation    string   `json:"collation,omitempty"`
	EnumValues   []string `json:"enum_values,omitempty"`
	Nullable     bool     `json:"nullable"`
	Default      string   `json:"default,omitempty"`
	DefaultLit   bool     `json:"default_literal,omitempty"`
	DefaultParen bool     `json:"default_paren,omitempty"`
}

func encodeSchema(schema sql.PrimaryKeySchema, checks []sql.CheckDefinition, indexes []indexDisk) ([]byte, error) {
	d := schemaDisk{PK: append([]int(nil), schema.PkOrdinals...)}
	for _, col := range schema.Schema {
		cd := columnDisk{Name: col.Name, Type: int32(col.Type.Type()), Nullable: col.Nullable}
		if st, ok := col.Type.(sql.StringType); ok {
			cd.Length = st.Length()
			if c := st.Collation(); c != sql.Collation_Default {
				cd.Collation = c.Name()
			}
		}
		if dt, ok := col.Type.(sql.DatetimeType); ok {
			cd.Precision = dt.Precision()
		}
		if tt, ok := col.Type.(types.TimeType); ok {
			cd.Precision = tt.Precision()
		}
		if dec, ok := col.Type.(sql.DecimalType); ok {
			cd.Precision = int(dec.Precision())
			cd.Scale = int(dec.Scale())
		}
		if et, ok := col.Type.(sql.EnumType); ok {
			cd.EnumValues = et.Values()
			if c := et.Collation(); c != sql.Collation_Default {
				cd.Collation = c.Name()
			}
		}
		if col.Default != nil {
			cd.Default = col.Default.String()
			cd.DefaultLit = col.Default.IsLiteral()
			cd.DefaultParen = col.Default.IsParenthesized()
		}
		d.Columns = append(d.Columns, cd)
	}
	for _, chk := range checks {
		d.Checks = append(d.Checks, checkDisk{Name: chk.Name, Expression: chk.CheckExpression, Enforced: chk.Enforced})
	}
	d.Indexes = indexes
	return json.Marshal(d)
}

func decodeSchema(data []byte) (sql.PrimaryKeySchema, []sql.CheckDefinition, []indexDisk, error) {
	var d schemaDisk
	if err := json.Unmarshal(data, &d); err != nil {
		return sql.PrimaryKeySchema{}, nil, nil, err
	}
	cols := make(sql.Schema, len(d.Columns))
	for i, cd := range d.Columns {
		coll := sql.Collation_Default
		if cd.Collation != "" {
			var err error
			coll, err = sql.ParseCollation("", cd.Collation, false)
			if err != nil {
				return sql.PrimaryKeySchema{}, nil, nil, fmt.Errorf("column %s: %w", cd.Name, err)
			}
		}
		typ, err := decodeType(querypb.Type(cd.Type), cd.Length, cd.Precision, cd.Scale, cd.EnumValues, coll)
		if err != nil {
			return sql.PrimaryKeySchema{}, nil, nil, fmt.Errorf("column %s: %w", cd.Name, err)
		}
		col := &sql.Column{Name: cd.Name, Type: typ, Nullable: cd.Nullable, PrimaryKey: contains(d.PK, i)}
		if cd.Default != "" {
			defVal := sql.NewUnresolvedColumnDefaultValue(cd.Default)
			defVal.Literal = cd.DefaultLit
			defVal.Parenthesized = cd.DefaultParen
			col.Default = defVal
		}
		cols[i] = col
	}
	var checks []sql.CheckDefinition
	for _, cd := range d.Checks {
		checks = append(checks, sql.CheckDefinition{Name: cd.Name, CheckExpression: cd.Expression, Enforced: cd.Enforced})
	}
	return sql.PrimaryKeySchema{Schema: cols, PkOrdinals: d.PK}, checks, d.Indexes, nil
}

// decodeType maps a querypb.Type to a go-mysql-server sql.Type.
// Future cleanup: these per-type switches (decodeType, appendCell, rowDecoder.cell)
// could be collapsed into a type registry keyed by querypb.Type.
func decodeType(t querypb.Type, length int64, precision int, scale int, enumValues []string, collation sql.CollationID) (sql.Type, error) {
	switch t {
	case querypb.Type_INT8:
		return types.Int8, nil
	case querypb.Type_INT16:
		return types.Int16, nil
	case querypb.Type_INT24:
		return types.Int24, nil
	case querypb.Type_INT32:
		return types.Int32, nil
	case querypb.Type_INT64:
		return types.Int64, nil
	case querypb.Type_UINT8:
		return types.Uint8, nil
	case querypb.Type_UINT16:
		return types.Uint16, nil
	case querypb.Type_UINT24:
		return types.Uint24, nil
	case querypb.Type_UINT32:
		return types.Uint32, nil
	case querypb.Type_UINT64:
		return types.Uint64, nil
	case querypb.Type_FLOAT32:
		return types.Float32, nil
	case querypb.Type_FLOAT64:
		return types.Float64, nil
	case querypb.Type_VARCHAR, querypb.Type_CHAR, querypb.Type_TEXT:
		if length <= 0 {
			length = 65535
		}
		return types.CreateString(t, length, collation)
	case querypb.Type_BLOB, querypb.Type_VARBINARY, querypb.Type_BINARY:
		if length <= 0 {
			length = 65535
		}
		return types.CreateBinary(t, length)
	case querypb.Type_DATE, querypb.Type_DATETIME, querypb.Type_TIMESTAMP:
		return types.CreateDatetimeType(t, precision)
	case querypb.Type_TIME:
		return types.CreateTimespanType(precision)
	case querypb.Type_JSON:
		return types.JSON, nil
	case querypb.Type_DECIMAL:
		p, s := uint8(precision), uint8(scale)
		if p == 0 {
			p = 10
		}
		return types.CreateColumnDecimalType(p, s)
	case querypb.Type_ENUM:
		return types.CreateEnumType(enumValues, collation)
	default:
		return nil, fmt.Errorf("unsupported SQL type %s", t.String())
	}
}

func validateSchema(schema sql.PrimaryKeySchema) error {
	for _, col := range schema.Schema {
		if col.AutoIncrement || col.Generated != nil {
			return fmt.Errorf("column %s uses AUTO_INCREMENT or a generated column, which RepoDB does not support", col.Name)
		}
		var length int64
		collation := sql.Collation_Default
		if st, ok := col.Type.(sql.StringType); ok {
			length = st.Length()
			collation = st.Collation()
		}
		var precision int
		if dt, ok := col.Type.(sql.DatetimeType); ok {
			precision = dt.Precision()
		}
		if tt, ok := col.Type.(types.TimeType); ok {
			precision = tt.Precision()
		}
		var scale int
		if dec, ok := col.Type.(sql.DecimalType); ok {
			precision = int(dec.Precision())
			scale = int(dec.Scale())
		}
		var enumValues []string
		if et, ok := col.Type.(sql.EnumType); ok {
			enumValues = et.Values()
			collation = et.Collation()
		}
		if _, err := decodeType(col.Type.Type(), length, precision, scale, enumValues, collation); err != nil {
			return err
		}
	}
	return nil
}

func encodeKey(schema sql.PrimaryKeySchema, row sql.Row) ([]byte, error) {
	var out []byte
	for _, ordinal := range schema.PkOrdinals {
		if ordinal < 0 || ordinal >= len(row) || row[ordinal] == nil {
			return nil, errors.New("primary key columns must be non-NULL")
		}
		var err error
		if out, err = appendKeyColumn(out, schema.Schema[ordinal].Type, row[ordinal]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// encodeIndexKey builds an order-preserving key from the given column ordinals
// of a row (see keycodec.go). Each column is 0x00 if NULL, else 0x01 followed
// by its encoding, so NULL sorts first. When any column is NULL, or the index
// is not unique, pkKey is appended so that entries stay distinct (MySQL
// semantics: NULL != NULL).
func encodeIndexKey(schema sql.PrimaryKeySchema, row sql.Row, ordinals []int, pkKey []byte, unique bool) ([]byte, bool, error) {
	var out []byte
	hasNull := false
	for _, ordinal := range ordinals {
		if ordinal < 0 || ordinal >= len(row) || row[ordinal] == nil {
			out = append(out, 0x00)
			hasNull = true
			continue
		}
		out = append(out, 0x01)
		var err error
		if out, err = appendKeyColumn(out, schema.Schema[ordinal].Type, row[ordinal]); err != nil {
			return nil, false, err
		}
	}
	if hasNull || !unique {
		out = append(out, pkKey...)
	}
	return out, hasNull, nil
}

// ensureIndexEdits rebuilds in-memory index edits from existing rows when
// the index tree has not been persisted yet (e.g., journal path before checkpoint).
func (s *tableState) ensureIndexEdits(ctx context.Context) error {
	if s.idxEdits != nil || len(s.indexes) == 0 {
		return nil
	}
	var persisted, unpersisted []indexDisk
	for _, idx := range s.indexes {
		if root, ok := s.manifest.Indexes[idx.Name]; ok && root.Valid() {
			persisted = append(persisted, idx)
		} else {
			unpersisted = append(unpersisted, idx)
		}
	}
	built := make(map[string][]prolly.Edit, len(s.indexes))
	if len(persisted) > 0 && s.hasEdits() {
		if err := s.overlayIndexEdits(ctx, persisted, built); err != nil {
			return err
		}
	}
	if len(unpersisted) > 0 {
		if err := s.ensureRows(ctx); err != nil {
			return err
		}
		for pkStr, row := range s.rows {
			pk := []byte(pkStr)
			for _, idx := range unpersisted {
				idxKey, _, err := encodeIndexKey(s.schema, row, idx.Columns, pk, idx.Unique)
				if err != nil {
					return err
				}
				built[idx.Name] = append(built[idx.Name], prolly.Edit{Key: idxKey, Value: pk})
			}
		}
	}
	s.idxEdits = make(map[string]*indexOverlay, len(s.indexes))
	for _, idx := range s.indexes {
		overlay := &indexOverlay{}
		for _, edit := range built[idx.Name] {
			overlay.add(edit)
		}
		// Derived edits become the shared base; the cache keeps it for later
		// transactions of this generation.
		overlay.base, overlay.local = overlay.view(), nil
		s.idxEdits[idx.Name] = overlay
	}
	return nil
}

// overlayIndexEdits adds, for each persisted index tree, the edits implied by
// the pending row overlay: the base row's entry is removed and the overlay
// row's entry added. Without them, rows from earlier journal transactions are
// invisible to index lookups and unique checks.
//
// Each index's removals come before its additions: a unique value can move
// from one row to another, and its new row's entry must win whatever the rows'
// key order (rdb-9afb3c).
func (s *tableState) overlayIndexEdits(ctx context.Context, indexes []indexDisk, built map[string][]prolly.Edit) error {
	var base *prolly.Tree
	if s.manifest.DataRoot.Valid() {
		tree, err := prolly.Open(s.store, s.manifest.DataRoot)
		if err != nil {
			return err
		}
		base = tree
	}
	type keyedEdit struct {
		key  string
		edit rowEdit
	}
	var overlay []keyedEdit
	if err := s.forEachEdit(func(key string, edit rowEdit) error {
		overlay = append(overlay, keyedEdit{key, edit})
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(overlay, func(i, j int) bool { return overlay[i].key < overlay[j].key })
	added := make(map[string][]prolly.Edit, len(indexes))
	for _, item := range overlay {
		edit := item.edit
		pk := []byte(item.key)
		var baseRow sql.Row
		if base != nil {
			value, err := base.Get(ctx, pk)
			switch {
			case errors.Is(err, prolly.ErrNotFound):
			case err != nil:
				return err
			default:
				if baseRow, err = decodeRow(s.schema.Schema, value); err != nil {
					return err
				}
			}
		}
		for _, idx := range indexes {
			var oldKey, newKey []byte
			var err error
			if baseRow != nil {
				if oldKey, _, err = encodeIndexKey(s.schema, baseRow, idx.Columns, pk, idx.Unique); err != nil {
					return err
				}
			}
			if !edit.delete {
				if newKey, _, err = encodeIndexKey(s.schema, edit.row, idx.Columns, pk, idx.Unique); err != nil {
					return err
				}
			}
			if oldKey != nil && newKey != nil && bytes.Equal(oldKey, newKey) {
				continue
			}
			if oldKey != nil {
				built[idx.Name] = append(built[idx.Name], prolly.Edit{Key: oldKey, Delete: true})
			}
			if newKey != nil {
				added[idx.Name] = append(added[idx.Name], prolly.Edit{Key: newKey, Value: pk})
			}
		}
	}
	for name, edits := range added {
		built[name] = append(built[name], edits...)
	}
	return nil
}

// lookupIndexKey checks if an index key exists in the index's pending edits
// or in its persisted prolly tree. Returns true if a non-deleted entry exists.
func (s *tableState) lookupIndexKey(ctx context.Context, idxName string, key []byte) (bool, error) {
	if overlay := s.idxEdits[idxName]; overlay != nil {
		if edit, ok := overlay.get(key); ok {
			return !edit.Delete, nil
		}
	}
	root, ok := s.manifest.Indexes[idxName]
	if !ok || !root.Valid() {
		return false, nil
	}
	tree, err := prolly.Open(s.store, root)
	if err != nil {
		return false, err
	}
	_, err = tree.Get(ctx, key)
	if errors.Is(err, prolly.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *tableState) addIndexEdit(idxName string, edit prolly.Edit) {
	if s.idxEdits == nil {
		panic("addIndexEdit called before ensureIndexEdits")
	}
	overlay := s.idxEdits[idxName]
	if overlay == nil {
		overlay = &indexOverlay{}
		s.idxEdits[idxName] = overlay
	}
	overlay.add(edit)
}

func loadTableMetadata(ctx context.Context, store storage.Store, manifest repository.Table) (*tableState, error) {
	schemaData, err := store.Get(ctx, manifest.SchemaRoot)
	if err != nil {
		return nil, err
	}
	schema, checks, indexes, err := decodeSchema(schemaData)
	if err != nil {
		return nil, err
	}
	manifest.Indexes = copyManifestIndexes(manifest.Indexes)
	return &tableState{schema: schema, checks: checks, indexes: indexes, manifest: manifest, store: store}, nil
}

func contains(values []int, value int) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func copySchema(s sql.PrimaryKeySchema) sql.PrimaryKeySchema {
	cols := make(sql.Schema, len(s.Schema))
	for i, col := range s.Schema {
		cols[i] = col.Copy()
	}
	return sql.PrimaryKeySchema{Schema: cols, PkOrdinals: append([]int(nil), s.PkOrdinals...)}
}
func copyChecks(checks []sql.CheckDefinition) []sql.CheckDefinition {
	if checks == nil {
		return nil
	}
	out := make([]sql.CheckDefinition, len(checks))
	copy(out, checks)
	return out
}
func copyIndexes(indexes []indexDisk) []indexDisk {
	if indexes == nil {
		return nil
	}
	out := make([]indexDisk, len(indexes))
	for i, idx := range indexes {
		out[i] = indexDisk{Name: idx.Name, Unique: idx.Unique, Columns: append([]int(nil), idx.Columns...)}
	}
	return out
}
func copyManifestIndexes(m map[string]storage.Hash) map[string]storage.Hash {
	if m == nil {
		return nil
	}
	out := make(map[string]storage.Hash, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
func cloneRow(row sql.Row) sql.Row { return append(sql.Row(nil), row...) }

var _ sql.TableCreator = (*database)(nil)
var _ sql.TableDropper = (*database)(nil)
var _ sql.PrimaryKeyTable = (*table)(nil)
var _ sql.IndexAddressableTable = (*table)(nil)
var _ sql.ProjectedTable = (*table)(nil)
var _ sql.StatisticsTable = (*table)(nil)
var _ sql.IndexedTable = (*table)(nil)
var _ sql.Index = (*primaryIndex)(nil)
var _ sql.Index = (*secondaryIndex)(nil)
var _ sql.OrderedIndex = (*primaryIndex)(nil)
var _ sql.OrderedIndex = (*secondaryIndex)(nil)
var _ sql.IndexAlterableTable = (*table)(nil)
var _ sql.InsertableTable = (*table)(nil)
var _ sql.UpdatableTable = (*table)(nil)
var _ sql.DeletableTable = (*table)(nil)
var _ sql.AlterableTable = (*table)(nil)
var _ sql.CheckTable = (*table)(nil)
var _ sql.CheckAlterableTable = (*table)(nil)
var _ sql.TemporaryTable = (*table)(nil)
var _ sql.TransactionSession = (*session)(nil)
