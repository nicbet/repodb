package server

import (
	"context"

	mysqlserver "github.com/dolthub/go-mysql-server/server"
	"github.com/dolthub/vitess/go/mysql"
	"github.com/dolthub/vitess/go/sqltypes"
	querypb "github.com/dolthub/vitess/go/vt/proto/query"
	"github.com/dolthub/vitess/go/vt/sqlparser"

	"github.com/nicbet/repodb/engine"
)

// explainInterceptor serves a plain EXPLAIN/DESCRIBE of a statement as
// EXPLAIN PLAN (see engine.NormalizeExplain). go-mysql-server's tabular
// EXPLAIN placeholder cannot be encoded for the MySQL protocol.
type explainInterceptor struct{}

var _ mysqlserver.Interceptor = explainInterceptor{}

func (explainInterceptor) Priority() int { return 0 }

func (explainInterceptor) Query(ctx context.Context, chain mysqlserver.Chain, c *mysql.Conn, query string, callback func(*sqltypes.Result, bool) error) error {
	return chain.ComQuery(ctx, c, engine.NormalizeExplain(query), callback)
}

func (explainInterceptor) MultiQuery(ctx context.Context, chain mysqlserver.Chain, c *mysql.Conn, query string, callback func(*sqltypes.Result, bool) error) (string, error) {
	return chain.ComMultiQuery(ctx, c, engine.NormalizeExplain(query), callback)
}

// ParsedQuery is part of the Interceptor interface but is never invoked by
// go-mysql-server's interceptor chain; it forwards unchanged.
func (explainInterceptor) ParsedQuery(chain mysqlserver.Chain, c *mysql.Conn, query string, _ sqlparser.Statement, callback func(*sqltypes.Result, bool) error) error {
	return chain.ComQuery(context.Background(), c, query, callback)
}

func (explainInterceptor) Prepare(ctx context.Context, chain mysqlserver.Chain, c *mysql.Conn, query string, prepare *mysql.PrepareData) ([]*querypb.Field, error) {
	return chain.ComPrepare(ctx, c, query, prepare)
}

func (explainInterceptor) StmtExecute(ctx context.Context, chain mysqlserver.Chain, c *mysql.Conn, prepare *mysql.PrepareData, callback func(*sqltypes.Result) error) error {
	return chain.ComStmtExecute(ctx, c, prepare, callback)
}
