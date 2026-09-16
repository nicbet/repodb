package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"sort"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/nicbet/repodb/common/prolly"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/common/storage"
)

type Resolution string

const (
	TakeBase   Resolution = "base"
	TakeLocal  Resolution = "local"
	TakeRemote Resolution = "remote"
	TakeDelete Resolution = "delete"
)

type MergeConflict struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Table         string `json:"table,omitempty"`
	Key           string `json:"key,omitempty"`
	BasePresent   bool   `json:"base_present"`
	LocalPresent  bool   `json:"local_present"`
	RemotePresent bool   `json:"remote_present"`
	Base          []byte `json:"base,omitempty"`
	Local         []byte `json:"local,omitempty"`
	Remote        []byte `json:"remote,omitempty"`
}

type tableVersion struct {
	present  bool
	table    repository.Table
	snapshot *repository.Snapshot
}

// MergeSnapshots performs a deterministic, conservative three-way merge of
// snapshots that the caller has already validated. Row values are indivisible:
// competing changes to one primary key are conflicts.
func MergeSnapshots(ctx context.Context, writer *repository.Writer, base, local, remote *repository.Snapshot, resolutions map[string]Resolution) (repository.Manifest, []storage.Hash, []MergeConflict, error) {
	manifest := repository.Manifest{DefaultDatabase: local.Manifest.DefaultDatabase, Tables: map[string]repository.Table{}}
	if local.Manifest.DefaultDatabase == remote.Manifest.DefaultDatabase {
		manifest.DefaultDatabase = local.Manifest.DefaultDatabase
	} else if local.Manifest.DefaultDatabase == base.Manifest.DefaultDatabase {
		manifest.DefaultDatabase = remote.Manifest.DefaultDatabase
	} else if remote.Manifest.DefaultDatabase == base.Manifest.DefaultDatabase {
		manifest.DefaultDatabase = local.Manifest.DefaultDatabase
	} else {
		conflict := MergeConflict{ID: "catalog:default-database", Kind: "catalog", BasePresent: true, LocalPresent: true, RemotePresent: true, Base: []byte(base.Manifest.DefaultDatabase), Local: []byte(local.Manifest.DefaultDatabase), Remote: []byte(remote.Manifest.DefaultDatabase)}
		switch resolutions[conflict.ID] {
		case TakeBase:
			manifest.DefaultDatabase = base.Manifest.DefaultDatabase
		case TakeLocal:
			manifest.DefaultDatabase = local.Manifest.DefaultDatabase
		case TakeRemote:
			manifest.DefaultDatabase = remote.Manifest.DefaultDatabase
		case "":
			return manifest, nil, []MergeConflict{conflict}, nil
		default:
			return manifest, nil, nil, fmt.Errorf("resolve %s: invalid resolution %q", conflict.ID, resolutions[conflict.ID])
		}
	}

	namesSet := map[string]struct{}{}
	for name := range base.Manifest.Tables {
		namesSet[name] = struct{}{}
	}
	for name := range local.Manifest.Tables {
		namesSet[name] = struct{}{}
	}
	for name := range remote.Manifest.Tables {
		namesSet[name] = struct{}{}
	}
	names := make([]string, 0, len(namesSet))
	for name := range namesSet {
		names = append(names, name)
	}
	sort.Strings(names)

	reachable := map[storage.Hash]struct{}{}
	var conflicts []MergeConflict
	for _, name := range names {
		b := version(base, name)
		l := version(local, name)
		r := version(remote, name)
		if equalVersion(l, r) {
			if err := copyTable(ctx, writer, name, l, &manifest, reachable); err != nil {
				return manifest, nil, nil, err
			}
			continue
		}
		if equalVersion(l, b) {
			if err := copyTable(ctx, writer, name, r, &manifest, reachable); err != nil {
				return manifest, nil, nil, err
			}
			continue
		}
		if equalVersion(r, b) {
			if err := copyTable(ctx, writer, name, l, &manifest, reachable); err != nil {
				return manifest, nil, nil, err
			}
			continue
		}

		// Different existence or schemas make row interpretation ambiguous.
		if !b.present || !l.present || !r.present || b.table.SchemaRoot != l.table.SchemaRoot || b.table.SchemaRoot != r.table.SchemaRoot {
			conflict, err := schemaConflict(ctx, name, b, l, r)
			if err != nil {
				return manifest, nil, nil, err
			}
			choice, ok := resolutions[conflict.ID]
			if !ok {
				conflicts = append(conflicts, conflict)
				continue
			}
			selected, err := selectVersion(choice, b, l, r)
			if err != nil {
				return manifest, nil, nil, fmt.Errorf("resolve %s: %w", conflict.ID, err)
			}
			if err := copyTable(ctx, writer, name, selected, &manifest, reachable); err != nil {
				return manifest, nil, nil, err
			}
			continue
		}

		schemaData, err := l.snapshot.Store().Get(ctx, l.table.SchemaRoot)
		if err != nil {
			return manifest, nil, nil, err
		}
		schema, _, mergeIndexDefs, err := decodeSchema(schemaData)
		if err != nil {
			return manifest, nil, nil, err
		}
		if err := validateSchema(schema); err != nil {
			return manifest, nil, nil, err
		}
		builder, err := prolly.NewSortedBuilder(ctx, writer, prolly.DefaultOptions)
		if err != nil {
			return manifest, nil, nil, err
		}
		rowConflicts, err := mergeRows(ctx, name, b, l, r, resolutions, func(entry prolly.Entry) error {
			if err := validateEntry(schema, entry); err != nil {
				return fmt.Errorf("merged table %s violates constraints: %w", name, err)
			}
			return builder.Add(entry)
		})
		if err != nil {
			return manifest, nil, nil, err
		}
		conflicts = append(conflicts, rowConflicts...)
		if len(rowConflicts) != 0 {
			continue
		}
		tree, err := builder.Finish()
		if err != nil {
			return manifest, nil, nil, err
		}
		schemaRoot, err := writer.Put(ctx, schemaData)
		if err != nil {
			return manifest, nil, nil, err
		}
		hashes, err := prolly.Reachable(ctx, writer, tree.Root())
		if err != nil {
			return manifest, nil, nil, err
		}
		reachable[schemaRoot] = struct{}{}
		for _, hash := range hashes {
			reachable[hash] = struct{}{}
		}
		tbl := repository.Table{SchemaRoot: schemaRoot, DataRoot: tree.Root()}
		if len(mergeIndexDefs) > 0 {
			idxRoots, err := rebuildIndexesFromTree(ctx, writer, schema, mergeIndexDefs, tree)
			if err != nil {
				return manifest, nil, nil, fmt.Errorf("rebuild indexes for merged table %s: %w", name, err)
			}
			if idxRoots != nil {
				tbl.Indexes = idxRoots
				for _, idxRoot := range idxRoots {
					idxHashes, err := prolly.Reachable(ctx, writer, idxRoot)
					if err != nil {
						return manifest, nil, nil, err
					}
					for _, h := range idxHashes {
						reachable[h] = struct{}{}
					}
				}
			}
		}
		manifest.Tables[name] = tbl
	}
	if len(conflicts) != 0 {
		return manifest, nil, conflicts, nil
	}
	hashes := make([]storage.Hash, 0, len(reachable))
	for hash := range reachable {
		hashes = append(hashes, hash)
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i] < hashes[j] })
	return manifest, hashes, nil, nil
}

func version(snapshot *repository.Snapshot, name string) tableVersion {
	table, ok := snapshot.Manifest.Tables[name]
	return tableVersion{present: ok, table: table, snapshot: snapshot}
}

func equalVersion(a, b tableVersion) bool {
	return a.present == b.present && (!a.present || a.table.Equal(b.table))
}

func schemaConflict(ctx context.Context, name string, b, l, r tableVersion) (MergeConflict, error) {
	value := func(v tableVersion) ([]byte, error) {
		if !v.present {
			return nil, nil
		}
		return v.snapshot.Store().Get(ctx, v.table.SchemaRoot)
	}
	bv, err := value(b)
	if err != nil {
		return MergeConflict{}, err
	}
	lv, err := value(l)
	if err != nil {
		return MergeConflict{}, err
	}
	rv, err := value(r)
	if err != nil {
		return MergeConflict{}, err
	}
	return MergeConflict{ID: "schema:" + name, Kind: "schema", Table: name,
		BasePresent: b.present, LocalPresent: l.present, RemotePresent: r.present,
		Base: bv, Local: lv, Remote: rv}, nil
}

func selectVersion(choice Resolution, b, l, r tableVersion) (tableVersion, error) {
	switch choice {
	case TakeBase:
		return b, nil
	case TakeLocal:
		return l, nil
	case TakeRemote:
		return r, nil
	case TakeDelete:
		return tableVersion{}, nil
	default:
		return tableVersion{}, fmt.Errorf("invalid resolution %q", choice)
	}
}

func copyTable(ctx context.Context, writer *repository.Writer, name string, source tableVersion, manifest *repository.Manifest, reachable map[storage.Hash]struct{}) error {
	if !source.present {
		return nil
	}
	schemaData, err := source.snapshot.Store().Get(ctx, source.table.SchemaRoot)
	if err != nil {
		return err
	}
	if _, err := decodeAndValidateSchema(schemaData); err != nil {
		return fmt.Errorf("table %s has invalid schema: %w", name, err)
	}
	objects, cached := validatedTableObjects(source.snapshot, name)
	if !cached {
		hashes, err := prolly.Reachable(ctx, source.snapshot.Store(), source.table.DataRoot)
		if err != nil {
			return err
		}
		objects = append([]storage.Hash{source.table.SchemaRoot}, hashes...)
		for _, idxRoot := range source.table.Indexes {
			idxHashes, err := prolly.Reachable(ctx, source.snapshot.Store(), idxRoot)
			if err != nil {
				return err
			}
			objects = append(objects, idxHashes...)
		}
	}
	if err := writer.ImportObjects(ctx, source.snapshot, objects); err != nil {
		return err
	}
	for _, hash := range objects {
		reachable[hash] = struct{}{}
	}
	manifest.Tables[name] = source.table
	return nil
}

func iterator(ctx context.Context, v tableVersion) (*prolly.Iterator, error) {
	if !v.present {
		return nil, nil
	}
	tree, err := prolly.Open(v.snapshot.Store(), v.table.DataRoot)
	if err != nil {
		return nil, err
	}
	return tree.Iterator(ctx)
}

type rowCursor struct {
	iterator *prolly.Iterator
	entry    prolly.Entry
	present  bool
}

func newRowCursor(ctx context.Context, v tableVersion) (*rowCursor, error) {
	it, err := iterator(ctx, v)
	if err != nil {
		return nil, err
	}
	cursor := &rowCursor{iterator: it}
	if err := cursor.advance(); err != nil {
		return nil, err
	}
	return cursor, nil
}

func (c *rowCursor) advance() error {
	if c.iterator == nil {
		c.present = false
		return nil
	}
	entry, ok, err := c.iterator.Next()
	if err != nil {
		return err
	}
	c.entry, c.present = entry, ok
	return nil
}

func (c *rowCursor) close() {
	if c.iterator != nil {
		_ = c.iterator.Close()
	}
}

func mergeRows(ctx context.Context, table string, b, l, r tableVersion, resolutions map[string]Resolution, emit func(prolly.Entry) error) ([]MergeConflict, error) {
	bc, err := newRowCursor(ctx, b)
	if err != nil {
		return nil, err
	}
	defer bc.close()
	lc, err := newRowCursor(ctx, l)
	if err != nil {
		return nil, err
	}
	defer lc.close()
	rc, err := newRowCursor(ctx, r)
	if err != nil {
		return nil, err
	}
	defer rc.close()
	cursors := []*rowCursor{bc, lc, rc}
	var conflicts []MergeConflict
	for {
		var key []byte
		for _, cursor := range cursors {
			if cursor.present && (key == nil || bytes.Compare(cursor.entry.Key, key) < 0) {
				key = cursor.entry.Key
			}
		}
		if key == nil {
			break
		}
		var values [3][]byte
		var present [3]bool
		for i, cursor := range cursors {
			if cursor.present && bytes.Equal(cursor.entry.Key, key) {
				values[i], present[i] = cursor.entry.Value, true
				if err := cursor.advance(); err != nil {
					return nil, err
				}
			}
		}
		bv, lv, rv := values[0], values[1], values[2]
		bp, lp, rp := present[0], present[1], present[2]
		var value []byte
		selected := false
		switch {
		case sameValue(lv, lp, rv, rp):
			value, selected = lv, lp
		case sameValue(lv, lp, bv, bp):
			value, selected = rv, rp
		case sameValue(rv, rp, bv, bp):
			value, selected = lv, lp
		default:
			id := "row:" + table + ":" + base64.RawURLEncoding.EncodeToString(key)
			conflict := MergeConflict{ID: id, Kind: "row", Table: table, Key: base64.RawStdEncoding.EncodeToString(key), BasePresent: bp, LocalPresent: lp, RemotePresent: rp, Base: bv, Local: lv, Remote: rv}
			choice, ok := resolutions[id]
			if !ok {
				conflicts = append(conflicts, conflict)
				continue
			}
			switch choice {
			case TakeBase:
				value, selected = bv, bp
			case TakeLocal:
				value, selected = lv, lp
			case TakeRemote:
				value, selected = rv, rp
			case TakeDelete:
				selected = false
			default:
				return nil, fmt.Errorf("resolve %s: invalid resolution %q", id, choice)
			}
		}
		if selected {
			if err := emit(prolly.Entry{Key: key, Value: value}); err != nil {
				return nil, err
			}
		}
	}
	return conflicts, nil
}

func sameValue(a []byte, ap bool, b []byte, bp bool) bool {
	return ap == bp && (!ap || bytes.Equal(a, b))
}

func validateEntries(schemaData []byte, entries []prolly.Entry) error {
	schema, err := decodeAndValidateSchema(schemaData)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := validateEntry(schema, entry); err != nil {
			return err
		}
	}
	return nil
}

func decodeAndValidateSchema(schemaData []byte) (sql.PrimaryKeySchema, error) {
	schema, _, _, err := decodeSchema(schemaData)
	if err != nil {
		return sql.PrimaryKeySchema{}, err
	}
	if err := validateSchema(schema); err != nil {
		return sql.PrimaryKeySchema{}, err
	}
	return schema, nil
}

func validateEntry(schema sql.PrimaryKeySchema, entry prolly.Entry) error {
	row, err := decodeRow(schema.Schema, entry.Value)
	if err != nil {
		return err
	}
	for i, column := range schema.Schema {
		if !column.Nullable && row[i] == nil {
			return fmt.Errorf("column %s is NOT NULL", column.Name)
		}
	}
	key, err := encodeKey(schema, row)
	if err != nil {
		return err
	}
	if !bytes.Equal(key, entry.Key) {
		return fmt.Errorf("stored row primary key does not match tree key")
	}
	return nil
}

func rebuildIndexesFromTree(ctx context.Context, writer storage.Store, schema sql.PrimaryKeySchema, indexes []indexDisk, dataTree *prolly.Tree) (map[string]storage.Hash, error) {
	entries, err := dataTree.Entries(ctx)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	idxEntries := make(map[string][]prolly.Entry, len(indexes))
	for _, entry := range entries {
		row, err := decodeRow(schema.Schema, entry.Value)
		if err != nil {
			return nil, err
		}
		for _, idx := range indexes {
			idxKey, _, err := encodeIndexKey(schema, row, idx.Columns, entry.Key, idx.Unique)
			if err != nil {
				return nil, err
			}
			idxEntries[idx.Name] = append(idxEntries[idx.Name], prolly.Entry{Key: idxKey, Value: entry.Key})
		}
	}
	roots := make(map[string]storage.Hash, len(indexes))
	for _, idx := range indexes {
		ie := idxEntries[idx.Name]
		sort.Slice(ie, func(i, j int) bool { return bytes.Compare(ie[i].Key, ie[j].Key) < 0 })
		if idx.Unique {
			for i := 1; i < len(ie); i++ {
				if bytes.Equal(ie[i-1].Key, ie[i].Key) {
					return nil, fmt.Errorf("unique index %s: merge produced duplicate key", idx.Name)
				}
			}
		}
		tree, err := prolly.Build(ctx, writer, ie, prolly.DefaultOptions)
		if err != nil {
			return nil, err
		}
		roots[idx.Name] = tree.Root()
	}
	return roots, nil
}
