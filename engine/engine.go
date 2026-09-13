// Package engine provides RepoDB's persistent embedded SQL engine. The MySQL
// server is an adapter over this package and does not own a separate catalog.
package engine

import (
	"container/list"
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
	"github.com/nicbet/repodb/common/prolly"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/common/storage"
)

const snapshotValidationVersion = 1

var validatedSnapshots = struct {
	sync.Mutex
	entries map[string]*list.Element
	order   *list.List
}{entries: make(map[string]*list.Element), order: list.New()}

const maxValidatedSnapshots = 128

type validationCacheEntry struct {
	key          string
	tableObjects map[string][]storage.Hash
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
		mode = PersistenceNativeGit
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
		working, err = repository.OpenWorkingState(repo)
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
		Fn: func(child sql.Expression) sql.Expression {
			return &recoverCommitExpression{repo: repo, child: child}
		},
	})
	db.working = working
	return &Engine{repo: repo, working: working, database: db, provider: provider, sql: sqlEngine}, nil
}

// ValidateSnapshot verifies every persisted schema, Prolly descendant, row,
// and primary-key encoding before integration publishes a fetched snapshot.
func ValidateSnapshot(ctx context.Context, snapshot *repository.Snapshot) error {
	if snapshot == nil {
		return errors.New("snapshot is required")
	}
	key := fmt.Sprintf("%s\x00%s\x00%d\x00%d", snapshot.RepositoryIdentity(), snapshot.Commit, snapshot.Generation(), snapshotValidationVersion)
	validatedSnapshots.Lock()
	if element := validatedSnapshots.entries[key]; element != nil {
		validatedSnapshots.order.MoveToFront(element)
		validatedSnapshots.Unlock()
		return nil
	}
	validatedSnapshots.Unlock()
	tableObjects := make(map[string][]storage.Hash, len(snapshot.Manifest.Tables))
	for name, table := range snapshot.Manifest.Tables {
		if _, err := loadTable(ctx, snapshot.Store(), table); err != nil {
			return fmt.Errorf("validate SQL table %s: %w", name, err)
		}
		hashes, err := prolly.Reachable(ctx, snapshot.Store(), table.DataRoot)
		if err != nil {
			return fmt.Errorf("validate SQL table %s reachability: %w", name, err)
		}
		tableObjects[name] = append([]storage.Hash{table.SchemaRoot}, hashes...)
	}
	validatedSnapshots.Lock()
	if element := validatedSnapshots.entries[key]; element != nil {
		validatedSnapshots.order.MoveToFront(element)
	} else {
		entry := validationCacheEntry{key: key, tableObjects: tableObjects}
		validatedSnapshots.entries[key] = validatedSnapshots.order.PushFront(entry)
		if validatedSnapshots.order.Len() > maxValidatedSnapshots {
			oldest := validatedSnapshots.order.Back()
			delete(validatedSnapshots.entries, oldest.Value.(validationCacheEntry).key)
			validatedSnapshots.order.Remove(oldest)
		}
	}
	validatedSnapshots.Unlock()
	return nil
}

func validatedTableObjects(snapshot *repository.Snapshot, table string) ([]storage.Hash, bool) {
	key := fmt.Sprintf("%s\x00%s\x00%d\x00%d", snapshot.RepositoryIdentity(), snapshot.Commit, snapshot.Generation(), snapshotValidationVersion)
	validatedSnapshots.Lock()
	defer validatedSnapshots.Unlock()
	element := validatedSnapshots.entries[key]
	if element == nil {
		return nil, false
	}
	validatedSnapshots.order.MoveToFront(element)
	hashes, ok := element.Value.(validationCacheEntry).tableObjects[table]
	return append([]storage.Hash(nil), hashes...), ok
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
	result, err := e.working.Checkpoint(ctx, message)
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
