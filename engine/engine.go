// Package engine provides RepoDB's persistent embedded SQL engine. The MySQL
// server is an adapter over this package and does not own a separate catalog.
package engine

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/apd/v3"
	sqle "github.com/dolthub/go-mysql-server"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/vitess/go/mysql"
	repodbgit "github.com/nicbet/repodb/common/git"
	"github.com/nicbet/repodb/common/prolly"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/common/storage"
	"github.com/shopspring/decimal"
)

const snapshotValidationVersion = 6

// validationCache remembers validated tables and Prolly subtrees.
//
// Tables are cached by content: repository and the table's schema, data and
// index roots. The roots name the content, so a table validated under one
// commit or journal generation stays valid under any other with the same
// roots. Each entry records the table's reachable objects with the Git object
// IDs they were read from, and a hit counts only for a snapshot that provides
// those same objects (repository.Snapshot.Provides): an object present only in
// a journal, or stored under another Git object, never vouches for a snapshot
// that lacks it.
//
// Nodes are Prolly nodes whose subtrees passed validation, so a table whose
// roots changed is validated only where its trees changed. A data tree's
// nodes are keyed by the table's schema root, because rows valid under one
// schema may not be under another; index trees need only structural checks
// and share one scope. Like tables, each entry records the Git object ID the
// node was read under, and a subtree counts for a snapshot only if that
// snapshot provides all of its nodes.
//
// processValidation serves every engine and sync in the process; a check
// (Check) uses a private instance, so it trusts nothing validated before it.
type validationCache struct {
	tablesMu     sync.Mutex
	tables       map[string]*list.Element
	tableOrder   *list.List
	tableObjects int

	nodesMu   sync.Mutex
	nodes     map[string]*list.Element
	nodeOrder *list.List
	nodeSize  int
}

func newValidationCache() *validationCache {
	return &validationCache{tables: make(map[string]*list.Element), tableOrder: list.New(), nodes: make(map[string]*list.Element), nodeOrder: list.New()}
}

var processValidation = newValidationCache()

// The table cache is bounded by entries and by the objects they record; the
// newest entry is always kept.
const (
	maxValidatedTables  = 1024
	maxValidatedObjects = 1 << 20
)

// maxValidatedNodeLinks bounds each node cache by nodes plus child links.
// Read it under the cache's nodesMu.
var maxValidatedNodeLinks = 1 << 20

type validationCacheEntry struct {
	key     string
	objects []repository.ObjectRef
}

func tableValidationKey(snapshot *repository.Snapshot, table repository.Table) string {
	var key strings.Builder
	fmt.Fprintf(&key, "%s\x00%d\x00%s\x00%s", snapshot.RepositoryIdentity(), snapshotValidationVersion, table.SchemaRoot, table.DataRoot)
	names := make([]string, 0, len(table.Indexes))
	for name := range table.Indexes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&key, "\x00%s=%s", name, table.Indexes[name])
	}
	return key.String()
}

// lookupTable returns the table's validated objects if snapshot provides all
// of them.
func (c *validationCache) lookupTable(snapshot *repository.Snapshot, key string) ([]repository.ObjectRef, bool) {
	c.tablesMu.Lock()
	element := c.tables[key]
	if element == nil {
		c.tablesMu.Unlock()
		return nil, false
	}
	c.tableOrder.MoveToFront(element)
	objects := element.Value.(validationCacheEntry).objects
	c.tablesMu.Unlock()
	if !snapshot.Provides(objects) {
		return nil, false
	}
	return objects, true
}

// rememberTable records (or replaces) the objects a table was validated with.
func (c *validationCache) rememberTable(key string, objects []repository.ObjectRef) {
	c.tablesMu.Lock()
	defer c.tablesMu.Unlock()
	if element := c.tables[key]; element != nil {
		c.tableObjects -= len(element.Value.(validationCacheEntry).objects)
		c.tableOrder.Remove(element)
	}
	c.tables[key] = c.tableOrder.PushFront(validationCacheEntry{key: key, objects: objects})
	c.tableObjects += len(objects)
	for c.tableOrder.Len() > 1 && (c.tableOrder.Len() > maxValidatedTables || c.tableObjects > maxValidatedObjects) {
		oldest := c.tableOrder.Back()
		entry := oldest.Value.(validationCacheEntry)
		delete(c.tables, entry.key)
		c.tableObjects -= len(entry.objects)
		c.tableOrder.Remove(oldest)
	}
}

const indexValidationScope = "index"

type validatedNodeEntry struct {
	key  string
	ref  repository.ObjectRef
	node prolly.ValidatedNode
}

func (e validatedNodeEntry) size() int { return 1 + len(e.node.Children) }

// snapshotNodeCache is the prolly.NodeCache of one tree scope as seen by one
// snapshot.
type snapshotNodeCache struct {
	cache    *validationCache
	snapshot *repository.Snapshot
	prefix   string
}

func (c *validationCache) nodeCache(snapshot *repository.Snapshot, scope string) snapshotNodeCache {
	return snapshotNodeCache{cache: c, snapshot: snapshot, prefix: fmt.Sprintf("%s\x00%d\x00%s\x00", snapshot.RepositoryIdentity(), snapshotValidationVersion, scope)}
}

// Subtree expands the cached subtree in memory; it misses if any node was
// evicted or the snapshot does not provide it.
func (c snapshotNodeCache) Subtree(hash storage.Hash) ([]prolly.NodeCount, bool) {
	var nodes []prolly.NodeCount
	var refs []repository.ObjectRef
	c.cache.nodesMu.Lock()
	for stack := []storage.Hash{hash}; len(stack) != 0; {
		next := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		element := c.cache.nodes[c.prefix+string(next)]
		if element == nil {
			c.cache.nodesMu.Unlock()
			return nil, false
		}
		c.cache.nodeOrder.MoveToFront(element)
		entry := element.Value.(validatedNodeEntry)
		nodes = append(nodes, prolly.NodeCount{Hash: next, Count: entry.node.Count})
		refs = append(refs, entry.ref)
		stack = append(stack, entry.node.Children...)
	}
	c.cache.nodesMu.Unlock()
	if !c.snapshot.Provides(refs) {
		return nil, false
	}
	performanceCounters.nodesReused.Add(uint64(len(nodes)))
	return nodes, true
}

func (c snapshotNodeCache) Remember(hash storage.Hash, node prolly.ValidatedNode) {
	performanceCounters.nodesValidated.Add(1)
	entry := validatedNodeEntry{key: c.prefix + string(hash), ref: c.snapshot.ObjectRefs([]storage.Hash{hash})[0], node: node}
	cache := c.cache
	cache.nodesMu.Lock()
	defer cache.nodesMu.Unlock()
	if element := cache.nodes[entry.key]; element != nil {
		cache.nodeSize -= element.Value.(validatedNodeEntry).size()
		cache.nodeOrder.Remove(element)
	}
	cache.nodes[entry.key] = cache.nodeOrder.PushFront(entry)
	cache.nodeSize += entry.size()
	for cache.nodeOrder.Len() > 1 && cache.nodeSize > maxValidatedNodeLinks {
		oldest := cache.nodeOrder.Back()
		evicted := oldest.Value.(validatedNodeEntry)
		delete(cache.nodes, evicted.key)
		cache.nodeSize -= evicted.size()
		cache.nodeOrder.Remove(oldest)
	}
}

type Engine struct {
	repo     *repository.Repository
	working  *repository.WorkingState
	database *database
	provider *provider
	sql      *sqle.Engine
	closed   atomic.Bool
}

type PersistenceMode string

const (
	PersistenceNativeGit PersistenceMode = "native-git"
	PersistenceJournal   PersistenceMode = "journal"
)

type Options struct {
	Persistence PersistenceMode
	// Durability of journal commits (journal persistence only); the zero
	// value is repository.DurabilityNormal. Native-git commits are Git
	// commits and always fully flushed.
	Durability repository.Durability
}

type Result struct {
	Columns []string
	Rows    [][]any
}

func Open(ctx context.Context, path string) (*Engine, error) {
	return OpenWithOptions(ctx, path, Options{})
}

func OpenWithOptions(ctx context.Context, path string, options Options) (*Engine, error) {
	repo, err := repository.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	return NewWithOptions(repo, options)
}

func New(repo *repository.Repository) (*Engine, error) {
	return NewWithOptions(repo, Options{})
}

func NewWithOptions(repo *repository.Repository, options Options) (*Engine, error) {
	if repo == nil {
		return nil, errors.New("repository is required")
	}
	mode := options.Persistence
	if mode == "" {
		mode = PersistenceJournal
	}
	var working *repository.WorkingState
	var snapshot *repository.Snapshot
	var err error
	switch mode {
	case PersistenceNativeGit:
		workingCheck, workingErr := repository.OpenWorkingState(repo)
		if workingErr != nil {
			return nil, workingErr
		}
		if workingCheck.Exists() {
			status, statusErr := workingCheck.Status(context.Background())
			if statusErr != nil {
				return nil, statusErr
			}
			if status.Dirty {
				return nil, fmt.Errorf("%w; open with journal persistence or checkpoint it first", repository.ErrWorkingStateDirty)
			}
		}
		snapshot, err = repo.Current(context.Background())
	case PersistenceJournal:
		working, err = repository.OpenWorkingStateWithOptions(repo, repository.WorkingOptions{Durability: options.Durability})
		if err == nil {
			snapshot, err = working.Current(context.Background())
		}
	default:
		return nil, fmt.Errorf("unsupported persistence mode %q", mode)
	}
	if err != nil {
		return nil, err
	}
	db := &database{name: snapshot.Manifest.DefaultDatabase, repo: repo, snapshot: snapshot}
	if err := ValidateSnapshot(context.Background(), snapshot); err != nil {
		return nil, err
	}
	provider := &provider{db: db}
	sqlEngine := sqle.NewDefault(provider)
	sqlEngine.Analyzer.Catalog.RegisterFunction(sql.NewEmptyContext(), sql.Function1{
		Name: "repodb_recover_commit",
		Fn: func(_ *sql.Context, child sql.Expression) sql.Expression {
			return &recoverCommitExpression{repo: repo, child: child}
		},
	})
	db.working = working
	return &Engine{repo: repo, working: working, database: db, provider: provider, sql: sqlEngine}, nil
}

// ValidateSnapshot verifies every persisted schema, Prolly descendant, row,
// and primary-key encoding before integration publishes a fetched snapshot.
// A table already validated with the same roots is skipped when this snapshot
// provides the same objects; within a changed table, subtrees validated before
// are skipped the same way (see validationCache).
func ValidateSnapshot(ctx context.Context, snapshot *repository.Snapshot) error {
	if snapshot == nil {
		return errors.New("snapshot is required")
	}
	for name, table := range snapshot.Manifest.Tables {
		if _, err := processValidation.validateTable(ctx, snapshot, name, table); err != nil {
			return err
		}
	}
	return nil
}

// validateTable validates one table unless the cache vouches for it, and
// returns the table's reachable objects.
func (c *validationCache) validateTable(ctx context.Context, snapshot *repository.Snapshot, name string, table repository.Table) ([]storage.Hash, error) {
	key := tableValidationKey(snapshot, table)
	if refs, ok := c.lookupTable(snapshot, key); ok {
		return refHashes(refs), nil
	}
	objects, err := c.walkTable(ctx, snapshot, name, table)
	if err != nil {
		return nil, err
	}
	c.rememberTable(key, snapshot.ObjectRefs(objects))
	return objects, nil
}

// walkTable decodes and key-checks every row not covered by the node cache,
// streaming the data tree leaf by leaf, and returns the table's reachable
// objects.
func (c *validationCache) walkTable(ctx context.Context, snapshot *repository.Snapshot, name string, table repository.Table) ([]storage.Hash, error) {
	store := snapshot.Store()
	if !table.DataRoot.Valid() {
		if table.SchemaRoot.Valid() {
			// Snapshots read objects lazily, so read the schema here to
			// check it even though no rows need it.
			if _, err := loadTableMetadata(ctx, store, table); err != nil {
				return nil, fmt.Errorf("validate SQL table %s schema: %w", name, err)
			}
			return []storage.Hash{table.SchemaRoot}, nil
		}
		return nil, nil
	}
	performanceCounters.tablesValidated.Add(1)
	state, err := loadTableMetadata(ctx, store, table)
	if err != nil {
		return nil, fmt.Errorf("validate SQL table %s: %w", name, err)
	}
	hashes, err := prolly.Validate(ctx, store, table.DataRoot, c.nodeCache(snapshot, string(table.SchemaRoot)), func(entries []prolly.Entry) error {
		return validateRows(state, entries)
	})
	if err != nil {
		return nil, fmt.Errorf("validate SQL table %s: %w", name, err)
	}
	objects := append([]storage.Hash{table.SchemaRoot}, hashes...)
	indexes := c.nodeCache(snapshot, indexValidationScope)
	for _, idxRoot := range table.Indexes {
		idxHashes, err := prolly.Validate(ctx, store, idxRoot, indexes, nil)
		if err != nil {
			return nil, fmt.Errorf("validate SQL table %s index: %w", name, err)
		}
		objects = append(objects, idxHashes...)
	}
	return objects, nil
}

// validateRows decodes each entry of a data leaf and checks its key.
func validateRows(state *tableState, entries []prolly.Entry) error {
	for _, entry := range entries {
		performanceCounters.rowsDecoded.Add(1)
		row, err := decodeRow(state.schema.Schema, entry.Value)
		if err != nil {
			return err
		}
		key, err := encodeKey(state.schema, row)
		if err != nil || !bytes.Equal(key, entry.Key) {
			return errors.New("stored row key does not match row primary key")
		}
	}
	return nil
}

func validatedTableObjects(snapshot *repository.Snapshot, table string) ([]storage.Hash, bool) {
	manifestTable, ok := snapshot.Manifest.Tables[table]
	if !ok {
		return nil, false
	}
	refs, ok := processValidation.lookupTable(snapshot, tableValidationKey(snapshot, manifestTable))
	if !ok {
		return nil, false
	}
	return refHashes(refs), true
}

func refHashes(refs []repository.ObjectRef) []storage.Hash {
	hashes := make([]storage.Hash, len(refs))
	for i, ref := range refs {
		hashes[i] = ref.Hash
	}
	return hashes
}

func (e *Engine) Repository() *repository.Repository     { return e.repo }
func (e *Engine) WorkingState() *repository.WorkingState { return e.working }
func (e *Engine) SQLEngine() *sqle.Engine                { return e.sql }

// Checkpoint publishes the journal's durable working generation to Git and
// refreshes this engine's validated snapshot. It is unavailable in native-Git
// persistence mode, where every SQL transaction already publishes a snapshot.
func (e *Engine) Checkpoint(ctx context.Context, message string) (repository.CommitResult, error) {
	if e.working == nil {
		return repository.CommitResult{Outcome: repository.OutcomeRejected}, errors.New("checkpoint requires journal persistence")
	}
	snapshot, err := e.working.Current(ctx)
	if err != nil {
		return repository.CommitResult{Outcome: repository.OutcomeRejected}, err
	}
	pending := snapshot.PendingEdits()
	var result repository.CommitResult
	if pending != nil {
		result, err = e.checkpointTypedEdits(ctx, message, snapshot, pending)
	} else {
		result, err = e.working.Checkpoint(ctx, message)
	}
	if result.Snapshot != nil {
		if validateErr := ValidateSnapshot(ctx, result.Snapshot); validateErr != nil {
			return result, errors.Join(err, validateErr)
		}
		e.database.mu.Lock()
		e.database.snapshot = result.Snapshot
		e.database.mu.Unlock()
	}
	return result, err
}

func (e *Engine) checkpointTypedEdits(ctx context.Context, message string, snapshot *repository.Snapshot, pending map[string]repository.PendingRows) (repository.CommitResult, error) {
	base, err := e.repo.SnapshotCommit(ctx, snapshot.Commit)
	if err != nil {
		return repository.CommitResult{Outcome: repository.OutcomeRejected}, err
	}
	writer, err := e.repo.BeginSnapshot(base)
	if err != nil {
		return repository.CommitResult{Outcome: repository.OutcomeRejected}, err
	}
	manifest := repository.Manifest{DefaultDatabase: snapshot.Manifest.DefaultDatabase, Tables: make(map[string]repository.Table, len(snapshot.Manifest.Tables))}
	reachable := make(map[storage.Hash]struct{})
	for name, table := range snapshot.Manifest.Tables {
		rowEdits, hasPending := pending[name]
		baseTable, basePresent := base.Manifest.Tables[name]
		schemaChanged := !basePresent || baseTable.SchemaRoot != table.SchemaRoot
		if !hasPending && !schemaChanged {
			manifest.Tables[name] = table
			objects, cached := validatedTableObjects(base, name)
			if !cached {
				hashes, err := prolly.Reachable(ctx, base.Store(), table.DataRoot)
				if err != nil {
					return repository.CommitResult{Outcome: repository.OutcomeRejected}, err
				}
				objects = append([]storage.Hash{table.SchemaRoot}, hashes...)
			}
			for _, hash := range objects {
				reachable[hash] = struct{}{}
			}
			continue
		}
		if !hasPending && schemaChanged {
			if table.SchemaRoot.Valid() {
				schemaData, err := snapshot.Store().Get(ctx, table.SchemaRoot)
				if err != nil {
					return repository.CommitResult{Outcome: repository.OutcomeRejected}, fmt.Errorf("checkpoint schema for %s: %w", name, err)
				}
				if _, putErr := writer.Put(ctx, schemaData); putErr != nil {
					return repository.CommitResult{Outcome: repository.OutcomeRejected}, putErr
				}
				reachable[table.SchemaRoot] = struct{}{}
			}
			if table.DataRoot.Valid() {
				hashes, err := prolly.Reachable(ctx, base.Store(), table.DataRoot)
				if err != nil {
					return repository.CommitResult{Outcome: repository.OutcomeRejected}, err
				}
				for _, hash := range hashes {
					reachable[hash] = struct{}{}
				}
			}
			tbl := repository.Table{SchemaRoot: table.SchemaRoot, DataRoot: table.DataRoot}
			if table.DataRoot.Valid() {
				dataTree, err := prolly.Open(base.Store(), table.DataRoot)
				if err != nil {
					return repository.CommitResult{Outcome: repository.OutcomeRejected}, err
				}
				if idxRoots, idxHashes, err := checkpointIndexTrees(ctx, writer, table, dataTree, nil); err != nil {
					return repository.CommitResult{Outcome: repository.OutcomeRejected}, err
				} else if idxRoots != nil {
					tbl.Indexes = idxRoots
					for _, h := range idxHashes {
						reachable[h] = struct{}{}
					}
				}
			}
			manifest.Tables[name] = tbl
			continue
		}
		if table.SchemaRoot.Valid() {
			baseTable, basePresent := base.Manifest.Tables[name]
			if !basePresent || baseTable.SchemaRoot != table.SchemaRoot {
				schemaData, err := snapshot.Store().Get(ctx, table.SchemaRoot)
				if err != nil {
					return repository.CommitResult{Outcome: repository.OutcomeRejected}, fmt.Errorf("checkpoint schema for %s: %w", name, err)
				}
				if _, putErr := writer.Put(ctx, schemaData); putErr != nil {
					return repository.CommitResult{Outcome: repository.OutcomeRejected}, putErr
				}
			}
			reachable[table.SchemaRoot] = struct{}{}
		}
		// PendingRows iterates in key order, as Apply requires.
		edits := make([]prolly.Edit, 0, rowEdits.Len())
		for it := rowEdits.Iter(nil, nil); ; {
			re, ok := it.Next()
			if !ok {
				break
			}
			item := prolly.Edit{Key: append([]byte(nil), re.Key...), Delete: re.Delete}
			if !re.Delete {
				item.Value = append([]byte(nil), re.Value...)
			}
			edits = append(edits, item)
		}
		if table.DataRoot.Valid() {
			baseTree, err := prolly.Open(base.Store(), table.DataRoot)
			if err != nil {
				return repository.CommitResult{Outcome: repository.OutcomeRejected}, err
			}
			tree, err := prolly.Apply(ctx, writer, baseTree, edits)
			if err != nil {
				return repository.CommitResult{Outcome: repository.OutcomeRejected}, err
			}
			hashes, err := prolly.Reachable(ctx, writer, tree.Root())
			if err != nil {
				return repository.CommitResult{Outcome: repository.OutcomeRejected}, err
			}
			for _, hash := range hashes {
				reachable[hash] = struct{}{}
			}
			tbl := repository.Table{SchemaRoot: table.SchemaRoot, DataRoot: tree.Root()}
			var delta *indexDelta
			if basePresent {
				delta = &indexDelta{store: base.Store(), base: baseTable, data: baseTree, edits: rowEdits}
			}
			if idxRoots, idxHashes, err := checkpointIndexTrees(ctx, writer, table, tree, delta); err != nil {
				return repository.CommitResult{Outcome: repository.OutcomeRejected}, err
			} else if idxRoots != nil {
				tbl.Indexes = idxRoots
				for _, h := range idxHashes {
					reachable[h] = struct{}{}
				}
			}
			manifest.Tables[name] = tbl
		} else {
			entries := make([]prolly.Entry, 0, len(edits))
			for _, edit := range edits {
				if !edit.Delete {
					entries = append(entries, prolly.Entry{Key: edit.Key, Value: edit.Value})
				}
			}
			tree, err := prolly.Build(ctx, writer, entries, prolly.DefaultOptions)
			if err != nil {
				return repository.CommitResult{Outcome: repository.OutcomeRejected}, err
			}
			hashes, err := prolly.Reachable(ctx, writer, tree.Root())
			if err != nil {
				return repository.CommitResult{Outcome: repository.OutcomeRejected}, err
			}
			for _, hash := range hashes {
				reachable[hash] = struct{}{}
			}
			tbl := repository.Table{SchemaRoot: table.SchemaRoot, DataRoot: tree.Root()}
			if idxRoots, idxHashes, err := checkpointIndexTrees(ctx, writer, table, tree, nil); err != nil {
				return repository.CommitResult{Outcome: repository.OutcomeRejected}, err
			} else if idxRoots != nil {
				tbl.Indexes = idxRoots
				for _, h := range idxHashes {
					reachable[h] = struct{}{}
				}
			}
			manifest.Tables[name] = tbl
		}
	}
	hashes := make([]storage.Hash, 0, len(reachable))
	for hash := range reachable {
		hashes = append(hashes, hash)
	}
	if err := writer.RetainOnly(hashes); err != nil {
		return repository.CommitResult{Outcome: repository.OutcomeRejected}, err
	}
	return e.working.CheckpointPrepared(ctx, message, writer, manifest)
}

// checkpointIndexTrees reads the schema from the store, and if the table
// has index definitions, builds its index trees for dataTree. With a usable
// delta it applies the pending edits to the base index trees; otherwise it
// rebuilds them from the whole data tree.
func checkpointIndexTrees(ctx context.Context, store storage.Store, table repository.Table, dataTree *prolly.Tree, delta *indexDelta) (map[string]storage.Hash, []storage.Hash, error) {
	if !table.SchemaRoot.Valid() {
		return nil, nil, nil
	}
	schemaData, err := store.Get(ctx, table.SchemaRoot)
	if err != nil {
		return nil, nil, err
	}
	schema, _, indexes, err := decodeSchema(schemaData)
	if err != nil {
		return nil, nil, err
	}
	if len(indexes) == 0 {
		return nil, nil, nil
	}
	var idxRoots map[string]storage.Hash
	if delta.usable(table, indexes) {
		idxRoots, err = applyIndexEdits(ctx, store, schema, indexes, dataTree, delta)
	} else {
		idxRoots, err = rebuildIndexesFromTree(ctx, store, schema, indexes, dataTree)
	}
	if err != nil {
		return nil, nil, err
	}
	var allHashes []storage.Hash
	for _, root := range idxRoots {
		hashes, err := prolly.Reachable(ctx, store, root)
		if err != nil {
			return nil, nil, err
		}
		allHashes = append(allHashes, hashes...)
	}
	return idxRoots, allHashes, nil
}

// indexDelta is a table's base snapshot state plus the pending row edits a
// checkpoint applies to it.
type indexDelta struct {
	store storage.Store
	base  repository.Table
	data  *prolly.Tree
	edits repository.PendingRows
}

// usable reports whether the base index trees can be updated in place: the
// schema and data root are the ones the edits were made against, and every
// index has a persisted tree.
func (d *indexDelta) usable(table repository.Table, indexes []indexDisk) bool {
	if d == nil || d.data == nil || d.base.SchemaRoot != table.SchemaRoot || d.base.DataRoot != table.DataRoot {
		return false
	}
	for _, idx := range indexes {
		if !d.base.Indexes[idx.Name].Valid() {
			return false
		}
	}
	return true
}

// applyIndexEdits updates each base index tree with the index edits implied by
// the delta's row edits. Its roots equal rebuildIndexesFromTree's for dataTree,
// the base data tree with those row edits applied.
func applyIndexEdits(ctx context.Context, writer storage.Store, schema sql.PrimaryKeySchema, indexes []indexDisk, dataTree *prolly.Tree, delta *indexDelta) (map[string]storage.Hash, error) {
	count, err := dataTree.Count(ctx)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil // as rebuildIndexesFromTree: an empty table has no index trees
	}
	rows := make([]keyedRowEdit, 0, delta.edits.Len())
	for it := delta.edits.Iter(nil, nil); ; {
		re, ok := it.Next()
		if !ok {
			break
		}
		item := keyedRowEdit{pk: append([]byte(nil), re.Key...), edit: rowEdit{delete: re.Delete}}
		if !re.Delete {
			if item.edit.row, err = decodeRow(schema.Schema, re.Value); err != nil {
				return nil, fmt.Errorf("decode pending edit: %w", err)
			}
		}
		rows = append(rows, item)
	}
	derived, err := deriveIndexEdits(ctx, schema, indexes, delta.data, rows)
	if err != nil {
		return nil, err
	}
	roots := make(map[string]storage.Hash, len(indexes))
	for _, idx := range indexes {
		edits, err := orderIndexEdits(idx, derived[idx.Name])
		if err != nil {
			return nil, err
		}
		baseTree, err := prolly.Open(delta.store, delta.base.Indexes[idx.Name])
		if err != nil {
			return nil, err
		}
		tree, err := prolly.Apply(ctx, writer, baseTree, edits)
		if err != nil {
			return nil, err
		}
		roots[idx.Name] = tree.Root()
	}
	return roots, nil
}

// orderIndexEdits sorts derived index edits into the strictly ordered form
// prolly.Apply takes. For a repeated key the later edit wins, so an addition
// overrides the removal before it (a unique value moving between rows); two
// additions of one key mean the data holds a duplicate unique value.
func orderIndexEdits(idx indexDisk, edits []prolly.Edit) ([]prolly.Edit, error) {
	sort.SliceStable(edits, func(i, j int) bool { return bytes.Compare(edits[i].Key, edits[j].Key) < 0 })
	out := edits[:0]
	for _, edit := range edits {
		if n := len(out); n > 0 && bytes.Equal(out[n-1].Key, edit.Key) {
			if !out[n-1].Delete && !edit.Delete {
				return nil, fmt.Errorf("unique index %s: checkpoint produced duplicate key", idx.Name)
			}
			out[n-1] = edit
			continue
		}
		out = append(out, edit)
	}
	return out, nil
}

func (e *Engine) Close() error {
	if e.closed.Swap(true) {
		return nil
	}
	// Release the repository's long-lived object reader: on Windows it keeps
	// pack files open. Another user of the repository restarts it on demand.
	defer repodbgit.CloseReaders(e.repo.Root)
	return e.sql.Close()
}

func (e *Engine) NewSession() (*Session, error) {
	if e.closed.Load() {
		return nil, errors.New("RepoDB engine is closed")
	}
	base := sql.NewBaseSession()
	return &Session{engine: e, session: newSession(base, e.database)}, nil
}

// SessionBuilder constructs wire-protocol sessions over the same persistent
// catalog used by embedded callers.
func (e *Engine) SessionBuilder() func(context.Context, *mysql.Conn, string) (sql.Session, error) {
	return func(_ context.Context, conn *mysql.Conn, address string) (sql.Session, error) {
		client := sql.Client{}
		if user, ok := conn.UserData.(sql.MysqlConnectionUser); ok {
			client.User, client.Address = user.User, user.Host
		}
		client.Capabilities = conn.Capabilities
		return newSession(sql.NewBaseSessionWithClientServer(address, client, conn.ConnectionID), e.database), nil
	}
}

type Session struct {
	engine  *Engine
	session *session
	mu      sync.Mutex
	closed  bool
}

func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if tx := s.session.GetTransaction(); tx != nil {
		ctx := sql.NewContext(context.Background(), sql.WithSession(s.session))
		_ = s.session.Rollback(ctx, tx)
		s.session.SetTransaction(nil)
	}
	return nil
}

func (s *Session) Query(ctx context.Context, statement string, args ...any) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Result{}, errors.New("RepoDB session is closed")
	}
	if len(args) > 0 {
		var err error
		statement, err = bind(statement, args)
		if err != nil {
			return Result{}, err
		}
	}
	statement = NormalizeExplain(statement)
	sqlCtx := sql.NewContext(ctx, sql.WithSession(s.session))
	schema, iter, _, err := s.engine.sql.Query(sqlCtx, statement)
	if err != nil {
		return Result{}, err
	}
	result := Result{Columns: make([]string, len(schema))}
	for i, column := range schema {
		result.Columns[i] = column.Name
	}
	for {
		row, nextErr := iter.Next(sqlCtx)
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			_ = iter.Close(sqlCtx)
			return Result{}, nextErr
		}
		for j, val := range row {
			if idx, ok := val.(uint16); ok {
				if et, ok := schema[j].Type.(sql.EnumType); ok {
					row[j], _ = et.At(int(idx))
				}
			}
		}
		result.Rows = append(result.Rows, append([]any(nil), row...))
	}
	if err := iter.Close(sqlCtx); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (s *Session) Exec(ctx context.Context, statement string, args ...any) error {
	_, err := s.Query(ctx, statement, args...)
	return err
}

func (s *Session) Begin(ctx context.Context) (*Tx, error) {
	if err := s.Exec(ctx, "START TRANSACTION"); err != nil {
		return nil, err
	}
	return &Tx{session: s}, nil
}

type Tx struct {
	session *Session
	done    atomic.Bool
}

func (t *Tx) Query(ctx context.Context, statement string, args ...any) (Result, error) {
	if t.done.Load() {
		return Result{}, errors.New("transaction is closed")
	}
	return t.session.Query(ctx, statement, args...)
}
func (t *Tx) Exec(ctx context.Context, statement string, args ...any) error {
	_, err := t.Query(ctx, statement, args...)
	return err
}
func (t *Tx) Commit(ctx context.Context) error {
	if t.done.Swap(true) {
		return errors.New("transaction is closed")
	}
	return t.session.Exec(ctx, "COMMIT")
}
func (t *Tx) Rollback(ctx context.Context) error {
	if t.done.Swap(true) {
		return errors.New("transaction is closed")
	}
	return t.session.Exec(ctx, "ROLLBACK")
}

func bind(statement string, args []any) (string, error) {
	var out strings.Builder
	arg := 0
	inSingle, inDouble := false, false
	for i := 0; i < len(statement); i++ {
		c := statement[i]
		if c == '\\' && (inSingle || inDouble) && i+1 < len(statement) {
			out.WriteByte(c)
			i++
			out.WriteByte(statement[i])
			continue
		}
		if c == '\'' && !inDouble {
			inSingle = !inSingle
			out.WriteByte(c)
			continue
		}
		if c == '"' && !inSingle {
			inDouble = !inDouble
			out.WriteByte(c)
			continue
		}
		if c != '?' || inSingle || inDouble {
			out.WriteByte(c)
			continue
		}
		if arg >= len(args) {
			return "", errors.New("not enough SQL parameters")
		}
		literal, err := sqlLiteral(args[arg])
		if err != nil {
			return "", err
		}
		out.WriteString(literal)
		arg++
	}
	if arg != len(args) {
		return "", fmt.Errorf("expected %d SQL parameters, got %d", arg, len(args))
	}
	return out.String(), nil
}

func sqlLiteral(value any) (string, error) {
	switch value := value.(type) {
	case nil:
		return "NULL", nil
	case bool:
		if value {
			return "TRUE", nil
		}
		return "FALSE", nil
	case int:
		return fmt.Sprintf("%d", value), nil
	case int8:
		return fmt.Sprintf("%d", value), nil
	case int16:
		return fmt.Sprintf("%d", value), nil
	case int32:
		return fmt.Sprintf("%d", value), nil
	case int64:
		return fmt.Sprintf("%d", value), nil
	case uint:
		return fmt.Sprintf("%d", value), nil
	case uint8:
		return fmt.Sprintf("%d", value), nil
	case uint16:
		return fmt.Sprintf("%d", value), nil
	case uint32:
		return fmt.Sprintf("%d", value), nil
	case uint64:
		return fmt.Sprintf("%d", value), nil
	case float32:
		return fmt.Sprintf("%g", value), nil
	case float64:
		return fmt.Sprintf("%g", value), nil
	case string:
		s := strings.ReplaceAll(value, `\`, `\\`)
		s = strings.ReplaceAll(s, "'", "''")
		return "'" + s + "'", nil
	case []byte:
		return "X'" + fmt.Sprintf("%x", value) + "'", nil
	case time.Time:
		return "'" + value.UTC().Format("2006-01-02 15:04:05.999999") + "'", nil
	case decimal.Decimal:
		return value.String(), nil
	case *apd.Decimal:
		return value.Text('f'), nil
	default:
		return "", fmt.Errorf("unsupported SQL parameter type %T", value)
	}
}
