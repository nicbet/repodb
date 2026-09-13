package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

type externalDB struct {
	dsn    string
	dbName string
	db     *sql.DB
}

func (e *externalDB) open() error {
	cfg, err := mysql.ParseDSN(e.dsn)
	if err != nil {
		return fmt.Errorf("parse DSN: %w", err)
	}
	cfg.DBName = ""
	cfg.MultiStatements = true
	adminDB, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return err
	}
	if _, err := adminDB.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", e.dbName)); err != nil {
		adminDB.Close()
		return err
	}
	if _, err := adminDB.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE `%s`", e.dbName)); err != nil {
		adminDB.Close()
		return err
	}
	adminDB.Close()

	cfg.DBName = e.dbName
	cfg.MultiStatements = false
	e.db, err = sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return err
	}
	return e.db.PingContext(ctx)
}

func (e *externalDB) close() error {
	if e.db == nil {
		return nil
	}
	return e.db.Close()
}

func (e *externalDB) exec(q string, args ...any) error {
	_, err := e.db.ExecContext(ctx, q, args...)
	return err
}

func (e *externalDB) queryValue(q string, args ...any) (string, error) {
	row := e.db.QueryRowContext(ctx, q, args...)
	var v string
	err := row.Scan(&v)
	return v, err
}

func (e *externalDB) queryRowCount(q string, args ...any) (int, error) {
	rows, err := e.db.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	cols, _ := rows.Columns()
	targets := make([]any, len(cols))
	pointers := make([]*string, len(cols))
	for i := range targets {
		targets[i] = &pointers[i]
	}
	for rows.Next() {
		if err := rows.Scan(targets...); err != nil {
			return 0, err
		}
		count++
	}
	return count, rows.Err()
}

func (e *externalDB) verify(q, value string) error {
	got, err := e.queryValue(q)
	if err != nil {
		return fmt.Errorf("%s: %w", q, err)
	}
	if got != value {
		return fmt.Errorf("%s: expected %q, got %q", q, value, got)
	}
	return nil
}

func (e *externalDB) newConn() (*sql.Conn, error) {
	return e.db.Conn(ctx)
}

func runExternal(r *report, dsn string) error {
	var failures []error
	for _, rows := range r.Config.Rows {
		if err := runExternalSize(r, dsn, rows); err != nil {
			failures = append(failures, fmt.Errorf("rows=%d: %w", rows, err))
		}
	}
	return errors.Join(failures...)
}

func runExternalSize(r *report, dsn string, rows int) error {
	dbName := fmt.Sprintf("repodb_bench_%d_%d", rows, time.Now().UnixNano()%100000)
	e := &externalDB{dsn: dsn, dbName: dbName}
	root := r.Root

	m := func(name string, n int, fn func(int, int) error) error {
		return r.measure(root, rows, name, 1, n, false, fn)
	}

	if err := m("open", 1, func(int, int) error { return e.open() }); err != nil {
		return err
	}
	defer e.close()

	if err := m("schema", 1, func(int, int) error {
		for _, q := range []string{
			"CREATE TABLE bench (id BIGINT PRIMARY KEY, value TEXT NOT NULL)",
			"CREATE TABLE authors (id BIGINT PRIMARY KEY, name TEXT NOT NULL)",
			"CREATE TABLE marker (id BIGINT PRIMARY KEY, value TEXT NOT NULL)",
			"INSERT INTO marker VALUES (1,'durable')",
			"INSERT INTO authors VALUES (1,'author')",
		} {
			if err := e.exec(q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	if err := m("bulk_load", 1, func(int, int) error {
		tx, err := e.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for start := 1; start <= rows; start += 500 {
			var q strings.Builder
			q.WriteString("INSERT INTO bench VALUES ")
			for id := start; id <= min(rows, start+499); id++ {
				if id > start {
					q.WriteByte(',')
				}
				fmt.Fprintf(&q, "(%d,'initial-01234567890123456789012345')", id)
			}
			if _, err := tx.ExecContext(ctx, q.String()); err != nil {
				return err
			}
		}
		return tx.Commit()
	}); err != nil {
		return err
	}

	if err := e.verify("SELECT COUNT(*) FROM bench", strconv.Itoa(rows)); err != nil {
		return err
	}

	n := r.Config.Requests

	for _, test := range []struct {
		name, q string
		count   int
	}{
		{"point_read", "SELECT value FROM bench WHERE id = 50", 1},
		{"point_miss", "SELECT value FROM bench WHERE id = 0", 0},
		{"range_read", "SELECT * FROM bench WHERE id >= 1 AND id <= 100", 100},
		{"ordered_limit", "SELECT * FROM bench ORDER BY id DESC LIMIT 20", 20},
		{"full_scan", "SELECT * FROM bench", rows},
		{"join_aggregate", "SELECT a.name, COUNT(*) FROM bench b JOIN authors a ON a.id = 1 GROUP BY a.name", 1},
	} {
		if err := m(test.name, n, func(int, int) error {
			count, err := e.queryRowCount(test.q)
			if err == nil && count != test.count {
				return fmt.Errorf("expected %d rows, got %d", test.count, count)
			}
			return err
		}); err != nil {
			return err
		}
	}

	if err := m("read_transaction", n, func(int, int) error {
		tx, err := e.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for i := 0; i < 10; i++ {
			var v string
			if err := tx.QueryRowContext(ctx, "SELECT value FROM bench WHERE id = ?", i+1).Scan(&v); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		return tx.Commit()
	}); err != nil {
		return err
	}

	for _, batch := range []int{1, 10, 100} {
		if err := m(fmt.Sprintf("update_batch_%d", batch), n, func(_ int, i int) error {
			tx, err := e.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			for id := 1; id <= batch; id++ {
				if _, err := tx.ExecContext(ctx, "UPDATE bench SET value = ? WHERE id = ?", fmt.Sprintf("batch-%d-generation-%d", batch, i), id); err != nil {
					return err
				}
			}
			return tx.Commit()
		}); err != nil {
			return err
		}
		if err := e.verify(fmt.Sprintf("SELECT value FROM bench WHERE id = %d", batch), fmt.Sprintf("batch-%d-generation-%d", batch, n-1)); err != nil {
			return err
		}
	}

	if err := m("insert", n, func(_ int, i int) error {
		return e.exec("INSERT INTO bench VALUES (?, 'inserted')", rows+i+1)
	}); err != nil {
		return err
	}
	if err := e.verify("SELECT COUNT(*) FROM bench", strconv.Itoa(rows+n)); err != nil {
		return err
	}

	if err := m("delete", n, func(_ int, i int) error {
		return e.exec("DELETE FROM bench WHERE id = ?", rows+i+1)
	}); err != nil {
		return err
	}
	if err := e.verify("SELECT COUNT(*) FROM bench", strconv.Itoa(rows)); err != nil {
		return err
	}

	if err := m("rollback", n, func(int, int) error {
		tx, err := e.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE marker SET value = 'rolled-back' WHERE id = 1"); err != nil {
			tx.Rollback()
			return err
		}
		return tx.Rollback()
	}); err != nil {
		return err
	}
	if err := e.verify("SELECT value FROM marker WHERE id = 1", "durable"); err != nil {
		return err
	}

	if err := externalConcurrency(r, root, rows, e); err != nil {
		return err
	}

	return nil
}

func externalConcurrency(r *report, root string, rows int, e *externalDB) error {
	if err := e.exec("CREATE TABLE counter (id BIGINT PRIMARY KEY, value BIGINT NOT NULL)"); err != nil {
		return err
	}
	if err := e.exec("INSERT INTO counter VALUES (1,0)"); err != nil {
		return err
	}

	var failures []error
	for _, clients := range r.Config.Clients {
		conns := make([]*sql.Conn, clients)
		for i := range conns {
			c, err := e.newConn()
			if err != nil {
				return err
			}
			conns[i] = c
			defer c.Close()
		}

		var successes atomic.Int64
		err := r.measure(root, rows, "mixed_80read_20write", clients, r.Config.Requests, false, func(w, i int) error {
			if i%5 != 0 {
				var v string
				err := conns[w].QueryRowContext(ctx, "SELECT value FROM bench WHERE id = ?", 1+(i+w)%rows).Scan(&v)
				if errors.Is(err, sql.ErrNoRows) {
					return nil
				}
				return err
			}
			_, err := conns[w].ExecContext(ctx, "UPDATE bench SET value = ? WHERE id = ?", fmt.Sprintf("clients-%d-worker-%d-request-%d", clients, w, i), 1+w%rows)
			if err == nil {
				successes.Add(1)
			}
			return err
		})
		if err != nil {
			return err
		}
		if successes.Load() == 0 {
			return errors.New("mixed workload committed no writes")
		}

		if _, err := e.db.ExecContext(ctx, "UPDATE counter SET value = 0 WHERE id = 1"); err != nil {
			return err
		}
		successes.Store(0)

		err = r.measure(root, rows, "contended_increment", clients, r.Config.Requests, false, func(w, i int) error {
			_, err := conns[w].ExecContext(ctx, "UPDATE counter SET value = value + 1 WHERE id = 1")
			if err == nil {
				successes.Add(1)
			}
			return err
		})
		if err != nil {
			return err
		}
		if err := e.verify("SELECT value FROM counter WHERE id = 1", strconv.FormatInt(successes.Load(), 10)); err != nil {
			err = fmt.Errorf("lost-update check: %w", err)
			r.Results[len(r.Results)-1].VerificationError = err.Error()
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func externalMySQLPointReadWrite(r *report, root string, rows int, e *externalDB) error {
	n := r.Config.Requests

	if err := r.measure(root, rows, "mysql_point_read", 1, n, false, func(int, int) error {
		return e.verify("SELECT value FROM marker WHERE id = 1", "durable")
	}); err != nil {
		return err
	}

	if err := r.measure(root, rows, "mysql_update", 1, n, false, func(_ int, i int) error {
		return e.exec(fmt.Sprintf("UPDATE bench SET value = 'wire-%d' WHERE id = 1", i))
	}); err != nil {
		return err
	}

	return e.verify("SELECT value FROM bench WHERE id = 1", fmt.Sprintf("wire-%d", n-1))
}

// cleanupExternal drops the benchmark database. Best effort.
func cleanupExternal(dsn, dbName string) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return
	}
	cfg.DBName = ""
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return
	}
	defer db.Close()
	db.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", dbName))
}

func printExternalUsage() {
	fmt.Fprintln(os.Stderr, `External baseline mode:
  dbbench -mode external -dsn 'user:pass@tcp(host:port)/' [flags]

Runs the portable SQL workloads (reads, writes, concurrency) against an
external MySQL-compatible server. Git-specific operations (sync, merge,
conflict, reopen) are skipped.

The DSN format follows the go-sql-driver/mysql standard:
  user:password@tcp(host:port)/
  root@tcp(127.0.0.1:3306)/
  root:@tcp(127.0.0.1:3336)/       (Dolt default)

A fresh database is created per fixture size and dropped on success.`)
}

