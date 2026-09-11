// Package client implements a small RepoDB client over the standard MySQL wire
// protocol. Existing MySQL clients can be used instead.
package client

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/nicbet/repodb/common/repository"
)

type Config struct {
	Address  string
	Database string
	User     string
	Password string
}

type Client struct{ db *sql.DB }

type Result struct {
	Columns []string
	Rows    [][]*string
}

type CommitError struct {
	Outcome   repository.CommitOutcome
	Candidate string
	Err       error
}

func (e *CommitError) Error() string { return e.Err.Error() }
func (e *CommitError) Unwrap() error { return e.Err }

var commitErrorPattern = regexp.MustCompile(`\[repodb-commit outcome=(committed|unknown|rejected) candidate=([0-9a-f]+)\]`)

func Open(config Config) (*Client, error) {
	if config.Address == "" {
		config.Address = "127.0.0.1:3306"
	}
	if config.User == "" {
		config.User = "root"
	}
	connector, err := mysql.NewConnector(&mysql.Config{
		User:                 config.User,
		Passwd:               config.Password,
		Net:                  "tcp",
		Addr:                 config.Address,
		DBName:               config.Database,
		AllowNativePasswords: true,
		MultiStatements:      false,
	})
	if err != nil {
		return nil, fmt.Errorf("configure MySQL client: %w", err)
	}
	return &Client{db: sql.OpenDB(connector)}, nil
}

func (c *Client) Close() error { return c.db.Close() }

func (c *Client) Ping(ctx context.Context) error { return c.db.PingContext(ctx) }

func (c *Client) Exec(ctx context.Context, statement string, args ...any) (sql.Result, error) {
	result, err := c.db.ExecContext(ctx, statement, args...)
	return result, classifyError(err)
}

func (c *Client) Query(ctx context.Context, statement string, args ...any) (Result, error) {
	rows, err := c.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return Result{}, classifyError(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return Result{}, err
	}
	result := Result{Columns: columns}
	for rows.Next() {
		values := make([]*string, len(columns))
		targets := make([]any, len(values))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return Result{}, err
		}
		result.Rows = append(result.Rows, values)
	}
	return result, classifyError(rows.Err())
}

// RecoverCommit asks the server whether candidate was published into the
// current local data history. It is safe after an unknown commit error.
func (c *Client) RecoverCommit(ctx context.Context, candidate string) (repository.CommitOutcome, error) {
	result, err := c.Query(ctx, "SELECT repodb_recover_commit(?)", candidate)
	if err != nil {
		return repository.OutcomeUnknown, err
	}
	if len(result.Rows) != 1 || len(result.Rows[0]) != 1 || result.Rows[0][0] == nil {
		return repository.OutcomeUnknown, errors.New("invalid RepoDB recovery response")
	}
	switch strings.ToLower(*result.Rows[0][0]) {
	case "committed":
		return repository.OutcomeCommitted, nil
	case "rejected":
		return repository.OutcomeRejected, nil
	case "unknown":
		return repository.OutcomeUnknown, nil
	default:
		return repository.OutcomeUnknown, fmt.Errorf("invalid RepoDB recovery outcome %q", *result.Rows[0][0])
	}
}

func classifyError(err error) error {
	if err == nil {
		return nil
	}
	match := commitErrorPattern.FindStringSubmatch(err.Error())
	if match == nil {
		return err
	}
	outcome := repository.OutcomeUnknown
	if match[1] == "committed" {
		outcome = repository.OutcomeCommitted
	} else if match[1] == "rejected" {
		outcome = repository.OutcomeRejected
	}
	return &CommitError{Outcome: outcome, Candidate: match[2], Err: err}
}
