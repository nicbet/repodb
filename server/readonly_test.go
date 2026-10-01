package server_test

import (
	"context"
	"database/sql"
	"errors"
	"os/exec"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/server"
)

// A write inside START TRANSACTION READ ONLY returns MySQL error 1792 over the
// wire, and the connection and server keep serving queries.
func TestMySQLReadOnlyTransactionRejectsWrites(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	git := exec.Command("git", "init", "--quiet", "-b", "main")
	git.Dir = root
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	repo, err := repository.Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Config{Address: "127.0.0.1:0", Repository: repo})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Start() }()
	defer srv.Close()

	connector, err := mysql.NewConnector(&mysql.Config{
		User: "root", Net: "tcp", Addr: srv.Address(), DBName: "repodb", AllowNativePasswords: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	// Transaction state is per connection, so pin one.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, statement := range []string{
		"CREATE TABLE t (id BIGINT PRIMARY KEY, v VARCHAR(20), n INT)",
		"INSERT INTO t VALUES (1, 'a', 0)",
		"START TRANSACTION READ ONLY",
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	for _, statement := range []string{
		"INSERT INTO t VALUES (99, 'ro', 0)",
		"UPDATE t SET n = 1 WHERE id = 1",
		"DELETE FROM t WHERE id = 1",
	} {
		_, err := conn.ExecContext(ctx, statement)
		var mysqlErr *mysql.MySQLError
		if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1792 {
			t.Errorf("%s: err = %v, want MySQL error 1792", statement, err)
		}
	}
	var count int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&count); err != nil || count != 1 {
		t.Fatalf("SELECT in read-only transaction = %d, %v; want 1", count, err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO t VALUES (2, 'b', 0)"); err != nil {
		t.Fatalf("write after read-only transaction: %v", err)
	}
	var n int
	if err := conn.QueryRowContext(ctx, "SELECT n FROM t WHERE id = 1").Scan(&n); err != nil || n != 0 {
		t.Fatalf("row 1 n = %d, %v; want 0", n, err)
	}
}
