// Package client implements a small RepoDB client over the standard MySQL wire
// protocol. Existing MySQL clients can be used instead.
package client

import (
	"context"
	"database/sql"
	"fmt"

	mysql "github.com/go-sql-driver/mysql"
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
	return c.db.ExecContext(ctx, statement, args...)
}

func (c *Client) Query(ctx context.Context, statement string, args ...any) (Result, error) {
	rows, err := c.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return Result{}, err
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
	return result, rows.Err()
}
