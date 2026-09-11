// Package engine provides RepoDB's persistent embedded SQL engine. The MySQL
// server is an adapter over this package and does not own a separate catalog.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	sqle "github.com/dolthub/go-mysql-server"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/vitess/go/mysql"
	"github.com/nicbet/repodb/common/repository"
)

type Engine struct {
	repo     *repository.Repository
	database *database
	provider *provider
	sql      *sqle.Engine
	closed   atomic.Bool
}

type Result struct {
	Columns []string
	Rows    [][]any
}

func Open(ctx context.Context, path string) (*Engine, error) {
	repo, err := repository.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	return New(repo)
}

func New(repo *repository.Repository) (*Engine, error) {
	if repo == nil {
		return nil, errors.New("repository is required")
	}
	snapshot, err := repo.Current(context.Background())
	if err != nil {
		return nil, err
	}
	db := &database{name: snapshot.Manifest.DefaultDatabase, repo: repo}
	if err := ValidateSnapshot(context.Background(), snapshot); err != nil {
		return nil, err
	}
	provider := &provider{db: db}
	sqlEngine := sqle.NewDefault(provider)
	sqlEngine.Analyzer.Catalog.RegisterFunction(sql.NewEmptyContext(), sql.Function1{
		Name: "repodb_recover_commit",
		Fn: func(child sql.Expression) sql.Expression {
			return &recoverCommitExpression{repo: repo, child: child}
		},
	})
	return &Engine{repo: repo, database: db, provider: provider, sql: sqlEngine}, nil
}

// ValidateSnapshot verifies every persisted schema, Prolly descendant, row,
// and primary-key encoding before integration publishes a fetched snapshot.
func ValidateSnapshot(ctx context.Context, snapshot *repository.Snapshot) error {
	for name, table := range snapshot.Manifest.Tables {
		if _, err := loadTable(ctx, snapshot.Store(), table); err != nil {
			return fmt.Errorf("validate SQL table %s: %w", name, err)
		}
	}
	return nil
}

func (e *Engine) Repository() *repository.Repository { return e.repo }
func (e *Engine) SQLEngine() *sqle.Engine            { return e.sql }

func (e *Engine) Close() error {
	if e.closed.Swap(true) {
		return nil
	}
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
		return "'" + strings.ReplaceAll(value, "'", "''") + "'", nil
	case []byte:
		return "X'" + fmt.Sprintf("%x", value) + "'", nil
	default:
		return "", fmt.Errorf("unsupported SQL parameter type %T", value)
	}
}
