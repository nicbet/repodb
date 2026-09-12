package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

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
	name string
	repo *repository.Repository
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
		return errors.New("RepoDB M2 requires an explicit PRIMARY KEY")
	}
	for existing := range tx.tables {
		if strings.EqualFold(existing, name) {
			return sql.ErrTableAlreadyExists.New(name)
		}
	}
	if err := validateSchema(schema); err != nil {
		return err
	}
	tx.tables[name] = &tableState{schema: copySchema(schema), rows: make(map[string]sql.Row)}
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
			tx.dirty = true
			return nil
		}
	}
	return sql.ErrTableNotFound.New(name)
}

type transaction struct {
	writer   *repository.Writer
	tables   map[string]*tableState
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
	writer, err := s.db.repo.Begin(ctx)
	if err != nil {
		return nil, err
	}
	snapshot := writer.BaseSnapshot()
	tx := &transaction{writer: writer, tables: make(map[string]*tableState), readOnly: characteristic == sql.ReadOnly}
	for name, manifestTable := range snapshot.Manifest.Tables {
		state, err := loadTable(ctx, snapshot.Store(), manifestTable)
		if err != nil {
			return nil, fmt.Errorf("load table %s: %w", name, err)
		}
		tx.tables[name] = state
	}
	return tx, nil
}

func (s *session) CommitTransaction(ctx *sql.Context, opaque sql.Transaction) error {
	tx, ok := opaque.(*transaction)
	if !ok {
		return fmt.Errorf("invalid RepoDB transaction %T", opaque)
	}
	if !tx.dirty {
		return nil
	}
	manifest := repository.Manifest{DefaultDatabase: s.db.name, Tables: make(map[string]repository.Table, len(tx.tables))}
	reachable := make(map[storage.Hash]struct{})
	for name, state := range tx.tables {
		schemaData, err := encodeSchema(state.schema)
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
		manifest.Tables[name] = repository.Table{SchemaRoot: schemaRoot, DataRoot: tree.Root()}
	}
	hashes := make([]storage.Hash, 0, len(reachable))
	for hash := range reachable {
		hashes = append(hashes, hash)
	}
	if err := tx.writer.RetainOnly(hashes); err != nil {
		return err
	}
	_, err := tx.writer.CommitWithOutcome(ctx, manifest)
	return err
}

func (s *session) Rollback(*sql.Context, sql.Transaction) error { return nil }
func (s *session) CreateSavepoint(*sql.Context, sql.Transaction, string) error {
	return errors.New("RepoDB M2 does not support savepoints")
}
func (s *session) RollbackToSavepoint(*sql.Context, sql.Transaction, string) error {
	return errors.New("RepoDB M2 does not support savepoints")
}
func (s *session) ReleaseSavepoint(*sql.Context, sql.Transaction, string) error {
	return errors.New("RepoDB M2 does not support savepoints")
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
	schema sql.PrimaryKeySchema
	rows   map[string]sql.Row
}

type table struct {
	db    *database
	name  string
	state *tableState
}

func (t *table) Name() string       { return t.name }
func (t *table) String() string     { return t.name }
func (t *table) Schema() sql.Schema { return sourcedSchema(t.state.schema.Schema, t.name) }
func (t *table) PrimaryKeySchema() sql.PrimaryKeySchema {
	return sql.PrimaryKeySchema{Schema: sourcedSchema(t.state.schema.Schema, t.name), PkOrdinals: append([]int(nil), t.state.schema.PkOrdinals...)}
}
func (t *table) Collation() sql.CollationID { return sql.Collation_Default }
func (t *table) Partitions(*sql.Context) (sql.PartitionIter, error) {
	return sql.PartitionsToPartitionIter(singlePartition{}), nil
}
func (t *table) PartitionRows(*sql.Context, sql.Partition) (sql.RowIter, error) {
	keys := make([]string, 0, len(t.state.rows))
	for key := range t.state.rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rows := make([]sql.Row, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, cloneRow(t.state.rows[key]))
	}
	return sql.RowsToRowIter(rows...), nil
}
func (t *table) Inserter(*sql.Context) sql.RowInserter { return &editor{table: t} }
func (t *table) Updater(*sql.Context) sql.RowUpdater   { return &editor{table: t} }
func (t *table) Deleter(*sql.Context) sql.RowDeleter   { return &editor{table: t} }

type singlePartition struct{}

func sourcedSchema(schema sql.Schema, source string) sql.Schema {
	result := make(sql.Schema, len(schema))
	for i, column := range schema {
		copy := *column
		copy.Source = source
		result[i] = &copy
	}
	return result
}

func (singlePartition) Key() []byte { return []byte("all") }

type editor struct {
	table  *table
	before map[string]sql.Row
}

func (e *editor) StatementBegin(*sql.Context)          { e.before = cloneRows(e.table.state.rows) }
func (e *editor) StatementComplete(*sql.Context) error { return nil }
func (e *editor) DiscardChanges(*sql.Context, error) error {
	if e.before != nil {
		e.table.state.rows = e.before
	}
	return nil
}
func (e *editor) Close(*sql.Context) error { return nil }
func (e *editor) Insert(ctx *sql.Context, row sql.Row) error {
	key, err := encodeKey(e.table.state.schema, row)
	if err != nil {
		return err
	}
	if existing, ok := e.table.state.rows[string(key)]; ok {
		return sql.NewUniqueKeyErr(base64.RawStdEncoding.EncodeToString(key), true, existing)
	}
	e.table.state.rows[string(key)] = cloneRow(row)
	tx, _ := transactionFrom(ctx)
	tx.dirty = true
	return nil
}
func (e *editor) Delete(ctx *sql.Context, row sql.Row) error {
	key, err := encodeKey(e.table.state.schema, row)
	if err != nil {
		return err
	}
	if _, ok := e.table.state.rows[string(key)]; !ok {
		return sql.ErrDeleteRowNotFound.New()
	}
	delete(e.table.state.rows, string(key))
	tx, _ := transactionFrom(ctx)
	tx.dirty = true
	return nil
}
func (e *editor) Update(ctx *sql.Context, oldRow, newRow sql.Row) error {
	oldKey, err := encodeKey(e.table.state.schema, oldRow)
	if err != nil {
		return err
	}
	newKey, err := encodeKey(e.table.state.schema, newRow)
	if err != nil {
		return err
	}
	if !bytes.Equal(oldKey, newKey) {
		if existing, ok := e.table.state.rows[string(newKey)]; ok {
			return sql.NewUniqueKeyErr(base64.RawStdEncoding.EncodeToString(newKey), true, existing)
		}
		delete(e.table.state.rows, string(oldKey))
	}
	e.table.state.rows[string(newKey)] = cloneRow(newRow)
	tx, _ := transactionFrom(ctx)
	tx.dirty = true
	return nil
}

type schemaDisk struct {
	Columns []columnDisk `json:"columns"`
	PK      []int        `json:"primary_key"`
}
type columnDisk struct {
	Name     string `json:"name"`
	Type     int32  `json:"type"`
	Length   int64  `json:"length,omitempty"`
	Nullable bool   `json:"nullable"`
}

func encodeSchema(schema sql.PrimaryKeySchema) ([]byte, error) {
	d := schemaDisk{PK: append([]int(nil), schema.PkOrdinals...)}
	for _, col := range schema.Schema {
		cd := columnDisk{Name: col.Name, Type: int32(col.Type.Type()), Nullable: col.Nullable}
		if st, ok := col.Type.(sql.StringType); ok {
			cd.Length = st.Length()
		}
		d.Columns = append(d.Columns, cd)
	}
	return json.Marshal(d)
}

func decodeSchema(data []byte) (sql.PrimaryKeySchema, error) {
	var d schemaDisk
	if err := json.Unmarshal(data, &d); err != nil {
		return sql.PrimaryKeySchema{}, err
	}
	cols := make(sql.Schema, len(d.Columns))
	for i, cd := range d.Columns {
		typ, err := decodeType(querypb.Type(cd.Type), cd.Length)
		if err != nil {
			return sql.PrimaryKeySchema{}, fmt.Errorf("column %s: %w", cd.Name, err)
		}
		cols[i] = &sql.Column{Name: cd.Name, Type: typ, Nullable: cd.Nullable, PrimaryKey: contains(d.PK, i)}
	}
	return sql.PrimaryKeySchema{Schema: cols, PkOrdinals: d.PK}, nil
}

func decodeType(t querypb.Type, length int64) (sql.Type, error) {
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
		return types.CreateString(t, length, sql.Collation_Default)
	case querypb.Type_BLOB, querypb.Type_VARBINARY, querypb.Type_BINARY:
		if length <= 0 {
			length = 65535
		}
		return types.CreateBinary(t, length)
	default:
		return nil, fmt.Errorf("unsupported M2 SQL type %s", t.String())
	}
}

func validateSchema(schema sql.PrimaryKeySchema) error {
	for _, col := range schema.Schema {
		if col.AutoIncrement || col.Default != nil || col.Generated != nil {
			return fmt.Errorf("column %s uses unsupported M2 schema behavior", col.Name)
		}
		if _, err := decodeType(col.Type.Type(), func() int64 {
			if st, ok := col.Type.(sql.StringType); ok {
				return st.Length()
			}
			return 0
		}()); err != nil {
			return err
		}
	}
	return nil
}

type cellDisk struct {
	Null  bool   `json:"null,omitempty"`
	Value string `json:"value,omitempty"`
}

func encodeRow(schema sql.Schema, row sql.Row) ([]byte, error) {
	if len(row) != len(schema) {
		return nil, fmt.Errorf("row has %d values, schema has %d", len(row), len(schema))
	}
	cells := make([]cellDisk, len(row))
	for i, value := range row {
		if value == nil {
			cells[i].Null = true
			continue
		}
		raw, err := rawValue(schema[i].Type.Type(), value)
		if err != nil {
			return nil, err
		}
		cells[i].Value = base64.RawStdEncoding.EncodeToString(raw)
	}
	return json.Marshal(cells)
}
func decodeRow(schema sql.Schema, data []byte) (sql.Row, error) {
	var cells []cellDisk
	if err := json.Unmarshal(data, &cells); err != nil {
		return nil, err
	}
	if len(cells) != len(schema) {
		return nil, errors.New("stored row width does not match schema")
	}
	row := make(sql.Row, len(cells))
	for i, cell := range cells {
		if cell.Null {
			continue
		}
		raw, err := base64.RawStdEncoding.DecodeString(cell.Value)
		if err != nil {
			return nil, err
		}
		switch schema[i].Type.Type() {
		case querypb.Type_INT8, querypb.Type_INT16, querypb.Type_INT24, querypb.Type_INT32, querypb.Type_INT64:
			v, err := strconv.ParseInt(string(raw), 10, 64)
			if err != nil {
				return nil, err
			}
			row[i], _, err = schema[i].Type.Convert(context.Background(), v)
			if err != nil {
				return nil, err
			}
		case querypb.Type_UINT8, querypb.Type_UINT16, querypb.Type_UINT24, querypb.Type_UINT32, querypb.Type_UINT64:
			v, err := strconv.ParseUint(string(raw), 10, 64)
			if err != nil {
				return nil, err
			}
			row[i], _, err = schema[i].Type.Convert(context.Background(), v)
			if err != nil {
				return nil, err
			}
		case querypb.Type_FLOAT32, querypb.Type_FLOAT64:
			v, err := strconv.ParseFloat(string(raw), 64)
			if err != nil {
				return nil, err
			}
			row[i], _, err = schema[i].Type.Convert(context.Background(), v)
			if err != nil {
				return nil, err
			}
		case querypb.Type_BLOB, querypb.Type_VARBINARY, querypb.Type_BINARY:
			row[i] = append([]byte(nil), raw...)
		default:
			row[i] = string(raw)
		}
	}
	return row, nil
}

func encodeKey(schema sql.PrimaryKeySchema, row sql.Row) ([]byte, error) {
	var out []byte
	for _, ordinal := range schema.PkOrdinals {
		if ordinal < 0 || ordinal >= len(row) || row[ordinal] == nil {
			return nil, errors.New("primary key columns must be non-NULL")
		}
		raw, err := rawValue(schema.Schema[ordinal].Type.Type(), row[ordinal])
		if err != nil {
			return nil, err
		}
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(raw)))
		out = append(out, byte(schema.Schema[ordinal].Type.Type()>>8), byte(schema.Schema[ordinal].Type.Type()), size[0], size[1], size[2], size[3])
		out = append(out, raw...)
	}
	return out, nil
}

func rawValue(typ querypb.Type, value any) ([]byte, error) {
	switch typ {
	case querypb.Type_BLOB, querypb.Type_VARBINARY, querypb.Type_BINARY:
		switch value := value.(type) {
		case []byte:
			return append([]byte(nil), value...), nil
		case string:
			return []byte(value), nil
		default:
			return nil, fmt.Errorf("binary value has type %T", value)
		}
	case querypb.Type_INT8, querypb.Type_INT16, querypb.Type_INT24, querypb.Type_INT32, querypb.Type_INT64,
		querypb.Type_UINT8, querypb.Type_UINT16, querypb.Type_UINT24, querypb.Type_UINT32, querypb.Type_UINT64,
		querypb.Type_FLOAT32, querypb.Type_FLOAT64:
		return []byte(fmt.Sprint(value)), nil
	default:
		value, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("text value has type %T", value)
		}
		return []byte(value), nil
	}
}

func loadTable(ctx context.Context, store storage.Store, manifest repository.Table) (*tableState, error) {
	schemaData, err := store.Get(ctx, manifest.SchemaRoot)
	if err != nil {
		return nil, err
	}
	schema, err := decodeSchema(schemaData)
	if err != nil {
		return nil, err
	}
	tree, err := prolly.Open(store, manifest.DataRoot)
	if err != nil {
		return nil, err
	}
	entries, err := tree.Entries(ctx)
	if err != nil {
		return nil, err
	}
	state := &tableState{schema: schema, rows: make(map[string]sql.Row, len(entries))}
	for _, entry := range entries {
		row, err := decodeRow(schema.Schema, entry.Value)
		if err != nil {
			return nil, err
		}
		key, err := encodeKey(schema, row)
		if err != nil || !bytes.Equal(key, entry.Key) {
			return nil, errors.New("stored row key does not match row primary key")
		}
		state.rows[string(entry.Key)] = row
	}
	return state, nil
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
func cloneRow(row sql.Row) sql.Row { return append(sql.Row(nil), row...) }
func cloneRows(rows map[string]sql.Row) map[string]sql.Row {
	out := make(map[string]sql.Row, len(rows))
	for k, row := range rows {
		out[k] = cloneRow(row)
	}
	return out
}

var _ sql.TableCreator = (*database)(nil)
var _ sql.TableDropper = (*database)(nil)
var _ sql.PrimaryKeyTable = (*table)(nil)
var _ sql.InsertableTable = (*table)(nil)
var _ sql.UpdatableTable = (*table)(nil)
var _ sql.DeletableTable = (*table)(nil)
var _ sql.TransactionSession = (*session)(nil)
