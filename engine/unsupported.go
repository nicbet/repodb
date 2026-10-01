package engine

import (
	"errors"
	"fmt"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/fulltext"
)

// RepoDB implements some go-mysql-server interfaces only to reject the
// statements they serve. Without them, go-mysql-server either accepts the
// statement with behavior RepoDB cannot honor (session-local views) or fails
// with a garbled error (FULLTEXT, primary-key changes).

var (
	errViewsUnsupported            = errors.New("views are not supported (rdb-5c1808)")
	errFulltextUnsupported         = errors.New("FULLTEXT indexes are not supported")
	errPrimaryKeyChangeUnsupported = errors.New("changing a table's primary key is not supported; RepoDB tables always have one")
)

// CreateView rejects views. Without sql.ViewDatabase, go-mysql-server keeps
// views in a session-local registry, so they would vanish silently.
func (d *database) CreateView(*sql.Context, string, string, string) error {
	return errViewsUnsupported
}

func (d *database) DropView(_ *sql.Context, name string) error {
	return sql.ErrViewDoesNotExist.New(d.Name(), name)
}

func (d *database) GetViewDefinition(*sql.Context, string) (sql.ViewDefinition, bool, error) {
	return sql.ViewDefinition{}, false, nil
}

func (d *database) AllViews(*sql.Context) ([]sql.ViewDefinition, error) { return nil, nil }

// CreateFulltextTableNames is never reached: go-mysql-server first checks the
// table for FULLTEXT support, which RepoDB tables lack, and reports
// sql.ErrFullTextNotSupported. Implementing fulltext.Database only replaces
// the garbled "tables cannot be created on database %!s(MISSING)" it reports
// for databases without FULLTEXT support.
func (d *database) CreateFulltextTableNames(*sql.Context, string, string) (fulltext.IndexTableNames, error) {
	return fulltext.IndexTableNames{}, errFulltextUnsupported
}

// CreateIndexedTable is go-mysql-server's CREATE TABLE path when the
// primary key has an index definition. It exists to reject primary-key prefix
// lengths, which CreateTable's schema does not carry.
func (d *database) CreateIndexedTable(ctx *sql.Context, name string, schema sql.PrimaryKeySchema, idxDef sql.IndexDef, collation sql.CollationID, comment string) error {
	if err := rejectIndexPrefixes(idxDef.Columns); err != nil {
		return err
	}
	return d.CreateTable(ctx, name, schema, collation, comment)
}

func (*table) CreatePrimaryKey(*sql.Context, []sql.IndexColumn) error {
	return errPrimaryKeyChangeUnsupported
}

func (*table) DropPrimaryKey(*sql.Context) error { return errPrimaryKeyChangeUnsupported }

// rejectIndexPrefixes rejects index prefix lengths such as INDEX (v(5)).
// RepoDB indexes whole values, so a prefix would silently change what a
// UNIQUE index enforces.
func rejectIndexPrefixes(columns []sql.IndexColumn) error {
	for _, col := range columns {
		if col.Length > 0 {
			return fmt.Errorf("index prefix lengths are not supported (%s(%d))", col.Name, col.Length)
		}
	}
	return nil
}

var _ sql.ViewDatabase = (*database)(nil)
var _ fulltext.Database = (*database)(nil)
var _ sql.IndexedTableCreator = (*database)(nil)
var _ sql.PrimaryKeyAlterableTable = (*table)(nil)
