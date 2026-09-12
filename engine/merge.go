package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"sort"

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

// MergeSnapshots performs a deterministic, conservative three-way merge. Row
// values are indivisible: competing changes to one primary key are conflicts.
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

		entries, rowConflicts, err := mergeRows(ctx, name, b, l, r, resolutions)
		if err != nil {
			return manifest, nil, nil, err
		}
		conflicts = append(conflicts, rowConflicts...)
		if len(rowConflicts) != 0 {
			continue
		}
		schemaData, err := l.snapshot.Store().Get(ctx, l.table.SchemaRoot)
		if err != nil {
			return manifest, nil, nil, err
		}
		if err := validateEntries(schemaData, entries); err != nil {
			return manifest, nil, nil, fmt.Errorf("merged table %s violates constraints: %w", name, err)
		}
		schemaRoot, err := writer.Put(ctx, schemaData)
		if err != nil {
			return manifest, nil, nil, err
		}
		tree, err := prolly.Build(ctx, writer, entries, prolly.DefaultOptions)
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
		manifest.Tables[name] = repository.Table{SchemaRoot: schemaRoot, DataRoot: tree.Root()}
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
	return a.present == b.present && (!a.present || a.table == b.table)
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
	tree, err := prolly.Open(source.snapshot.Store(), source.table.DataRoot)
	if err != nil {
		return err
	}
	entries, err := tree.Entries(ctx)
	if err != nil {
		return err
	}
	if err := validateEntries(schemaData, entries); err != nil {
		return fmt.Errorf("table %s violates constraints: %w", name, err)
	}
	schemaRoot, err := writer.Put(ctx, schemaData)
	if err != nil {
		return err
	}
	newTree, err := prolly.Build(ctx, writer, entries, prolly.DefaultOptions)
	if err != nil {
		return err
	}
	hashes, err := prolly.Reachable(ctx, writer, newTree.Root())
	if err != nil {
		return err
	}
	reachable[schemaRoot] = struct{}{}
	for _, hash := range hashes {
		reachable[hash] = struct{}{}
	}
	manifest.Tables[name] = repository.Table{SchemaRoot: schemaRoot, DataRoot: newTree.Root()}
	return nil
}

func rawEntries(ctx context.Context, v tableVersion) (map[string][]byte, error) {
	if !v.present {
		return map[string][]byte{}, nil
	}
	tree, err := prolly.Open(v.snapshot.Store(), v.table.DataRoot)
	if err != nil {
		return nil, err
	}
	entries, err := tree.Entries(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		out[string(entry.Key)] = append([]byte(nil), entry.Value...)
	}
	return out, nil
}

func mergeRows(ctx context.Context, table string, b, l, r tableVersion, resolutions map[string]Resolution) ([]prolly.Entry, []MergeConflict, error) {
	bm, err := rawEntries(ctx, b)
	if err != nil {
		return nil, nil, err
	}
	lm, err := rawEntries(ctx, l)
	if err != nil {
		return nil, nil, err
	}
	rm, err := rawEntries(ctx, r)
	if err != nil {
		return nil, nil, err
	}
	keysSet := map[string]struct{}{}
	for key := range bm {
		keysSet[key] = struct{}{}
	}
	for key := range lm {
		keysSet[key] = struct{}{}
	}
	for key := range rm {
		keysSet[key] = struct{}{}
	}
	keys := make([]string, 0, len(keysSet))
	for key := range keysSet {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var entries []prolly.Entry
	var conflicts []MergeConflict
	for _, key := range keys {
		bv, bp := bm[key]
		lv, lp := lm[key]
		rv, rp := rm[key]
		var value []byte
		present := false
		switch {
		case sameValue(lv, lp, rv, rp):
			value, present = lv, lp
		case sameValue(lv, lp, bv, bp):
			value, present = rv, rp
		case sameValue(rv, rp, bv, bp):
			value, present = lv, lp
		default:
			id := "row:" + table + ":" + base64.RawURLEncoding.EncodeToString([]byte(key))
			conflict := MergeConflict{ID: id, Kind: "row", Table: table, Key: base64.RawStdEncoding.EncodeToString([]byte(key)), BasePresent: bp, LocalPresent: lp, RemotePresent: rp, Base: bv, Local: lv, Remote: rv}
			choice, ok := resolutions[id]
			if !ok {
				conflicts = append(conflicts, conflict)
				continue
			}
			switch choice {
			case TakeBase:
				value, present = bv, bp
			case TakeLocal:
				value, present = lv, lp
			case TakeRemote:
				value, present = rv, rp
			case TakeDelete:
				present = false
			default:
				return nil, nil, fmt.Errorf("resolve %s: invalid resolution %q", id, choice)
			}
		}
		if present {
			entries = append(entries, prolly.Entry{Key: []byte(key), Value: append([]byte(nil), value...)})
		}
	}
	return entries, conflicts, nil
}

func sameValue(a []byte, ap bool, b []byte, bp bool) bool {
	return ap == bp && (!ap || bytes.Equal(a, b))
}

func validateEntries(schemaData []byte, entries []prolly.Entry) error {
	schema, err := decodeSchema(schemaData)
	if err != nil {
		return err
	}
	if err := validateSchema(schema); err != nil {
		return err
	}
	for _, entry := range entries {
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
	}
	return nil
}
