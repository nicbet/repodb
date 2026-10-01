package engine_test

import (
	"testing"

	gmssql "github.com/dolthub/go-mysql-server/sql"
)

// A write inside START TRANSACTION READ ONLY is rejected with the read-only
// transaction error instead of panicking, and reads still work.
func TestReadOnlyTransactionRejectsWrites(t *testing.T) {
	eng, ctx := openEngine(t)
	s, err := eng.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, v VARCHAR(20), n INT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 'a', 0)"); err != nil {
		t.Fatal(err)
	}

	if err := s.Exec(ctx, "START TRANSACTION READ ONLY"); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO t VALUES (99, 'ro', 0)",
		"UPDATE t SET n = 1 WHERE id = 1",
		"DELETE FROM t WHERE id = 1",
		"INSERT INTO t SELECT id + 100, v, n FROM t",
	} {
		err := s.Exec(ctx, statement)
		if !gmssql.ErrReadOnlyTransaction.Is(err) {
			t.Errorf("%s: err = %v, want read-only transaction error", statement, err)
		}
	}
	result, err := s.Query(ctx, "SELECT id, v, n FROM t ORDER BY id")
	if err != nil {
		t.Fatalf("SELECT in read-only transaction: %v", err)
	}
	if len(result.Rows) != 1 || result.Rows[0][1] != "a" {
		t.Fatalf("rows in read-only transaction = %#v", result.Rows)
	}
	if err := s.Exec(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}

	result, err = s.Query(ctx, "SELECT id, v, n FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][2] != int32(0) {
		t.Fatalf("rows after read-only transaction = %#v", result.Rows)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (2, 'b', 0)"); err != nil {
		t.Fatalf("write after read-only transaction: %v", err)
	}
}
