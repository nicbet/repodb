package engine_test

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

func TestPersistentEmbeddedSQLCommitRollbackAndParameters(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}

	eng, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := eng.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "CREATE TABLE issues (id BIGINT PRIMARY KEY, title VARCHAR(200) NOT NULL, closed BOOLEAN NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO issues VALUES (?, ?, ?)", int64(1), "can't reproduce", false); err != nil {
		t.Fatal(err)
	}
	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(ctx, "INSERT INTO issues VALUES (?, ?, ?)", int64(2), "rolled back", true); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	_ = eng.Close()

	reopened, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reader, _ := reopened.NewSession()
	defer reader.Close()
	result, err := reader.Query(ctx, "SELECT id, title, closed FROM issues ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][1] != "can't reproduce" {
		t.Fatalf("rows = %#v", result.Rows)
	}
	status := exec.Command("git", "status", "--porcelain=v1")
	status.Dir = root
	if output, err := status.CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatalf("SQL changed source worktree: %q, %v", output, err)
	}
}

func TestStringParameterBackslashRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id INT PRIMARY KEY, data TEXT)"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		input string
	}{
		{"bare backslash", `a\b`},
		{"escaped quote", `hello \"world\"`},
		{"double backslash", `a\\b`},
		{"newline escape", `line1\nline2`},
		{"tab escape", `col1\tcol2`},
		{"json payload", `{"msg":"hello \"world\"","path":"C:\\Users\\test","nl":"a\nb"}`},
		{"single quote and backslash", `it's a \path`},
	}

	for i, tc := range cases {
		if err := s.Exec(ctx, "INSERT INTO t (id, data) VALUES (?, ?)", i, tc.input); err != nil {
			t.Fatalf("%s: insert: %v", tc.name, err)
		}
	}

	result, err := s.Query(ctx, "SELECT id, data FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != len(cases) {
		t.Fatalf("got %d rows, want %d", len(result.Rows), len(cases))
	}
	for i, tc := range cases {
		got := result.Rows[i][1].(string)
		if got != tc.input {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.input)
		}
	}
}

func openEngine(t *testing.T) (*engine.Engine, context.Context) {
	t.Helper()
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	return eng, ctx
}

func TestAlterTableAddColumn(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 'alice')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (2, 'bob')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "ALTER TABLE t ADD COLUMN age BIGINT"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id, name, age FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(result.Rows))
	}
	if result.Rows[0][2] != nil {
		t.Errorf("row 1 age = %v, want nil", result.Rows[0][2])
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (3, 'charlie', 30)"); err != nil {
		t.Fatal(err)
	}
	result, err = s.Query(ctx, "SELECT id, name, age FROM t WHERE id = 3")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][2] != int64(30) {
		t.Fatalf("row 3 = %#v", result.Rows)
	}
}

func TestAlterTableAddColumnFirst(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 'alice')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "ALTER TABLE t ADD COLUMN tag TEXT FIRST"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT tag, id, name FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0] != nil {
		t.Errorf("tag = %v, want nil", result.Rows[0][0])
	}
}

func TestAlterTableAddColumnAfter(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, name TEXT, email TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 'alice', 'a@b.com')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "ALTER TABLE t ADD COLUMN age BIGINT AFTER `name`"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id, name, age, email FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][2] != nil {
		t.Errorf("age = %v, want nil", result.Rows[0][2])
	}
	if result.Rows[0][3] != "a@b.com" {
		t.Errorf("email = %v, want a@b.com", result.Rows[0][3])
	}
}

func TestAlterTableAddNotNullRejectsWithRows(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "ALTER TABLE t ADD COLUMN name TEXT NOT NULL")
	if err == nil {
		t.Fatal("expected error adding NOT NULL column to table with rows")
	}
}

func TestAlterTableAddPrimaryKeyColumnRejected(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "ALTER TABLE t ADD COLUMN id2 BIGINT PRIMARY KEY")
	if err == nil {
		t.Fatal("expected error adding column with PRIMARY KEY constraint")
	}
}

func TestAlterTableAddUnsupportedTypeRejected(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "ALTER TABLE t ADD COLUMN y YEAR")
	if err == nil {
		t.Fatal("expected error adding unsupported YEAR column")
	}
}

func TestAlterTableModifyToUnsupportedTypeRejected(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, val TEXT)"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "ALTER TABLE t MODIFY COLUMN val YEAR")
	if err == nil {
		t.Fatal("expected error modifying to unsupported YEAR type")
	}
}

func TestAlterTableModifyNullableToNotNullWithNullsRejected(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "ALTER TABLE t MODIFY COLUMN name TEXT NOT NULL")
	if err == nil {
		t.Fatal("expected error changing nullable column with NULLs to NOT NULL")
	}
}

func TestAlterTableModifyNullableToNotNullNoNulls(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 'alice')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "ALTER TABLE t MODIFY COLUMN name TEXT NOT NULL"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "INSERT INTO t (id) VALUES (2)")
	if err == nil {
		t.Fatal("expected error inserting NULL into NOT NULL column after MODIFY")
	}
}

func TestAlterTableAddNotNullEmptyTable(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "ALTER TABLE t ADD COLUMN name TEXT NOT NULL"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 'alice')"); err != nil {
		t.Fatal(err)
	}
}

func TestAlterTableDropColumn(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, name TEXT, age BIGINT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 'alice', 30)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (2, 'bob', 25)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "ALTER TABLE t DROP COLUMN age"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id, name FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(result.Rows))
	}
	if result.Rows[0][1] != "alice" {
		t.Errorf("row 1 name = %v, want alice", result.Rows[0][1])
	}
}

func TestAlterTableDropPKColumnRejected(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "ALTER TABLE t DROP COLUMN id")
	if err == nil {
		t.Fatal("expected error dropping PK column")
	}
}

func TestAlterTableModifyVarcharNarrowingValidates(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, name VARCHAR(100))"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 'this string is longer than ten characters')"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "ALTER TABLE t MODIFY COLUMN name VARCHAR(10)")
	if err == nil {
		t.Fatal("expected error narrowing VARCHAR with oversized values")
	}
}

func TestAlterTableModifyPKLengthRejected(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id VARCHAR(100) PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "ALTER TABLE t MODIFY COLUMN id VARCHAR(50)")
	if err == nil {
		t.Fatal("expected error changing PK column length")
	}
}

func TestAlterTableModifyColumnType(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, val BIGINT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 42)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "ALTER TABLE t MODIFY COLUMN val TEXT"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id, val FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][1] != "42" {
		t.Errorf("val = %v (%T), want '42'", result.Rows[0][1], result.Rows[0][1])
	}
}

func TestAlterTableModifyPKTypeRejected(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "ALTER TABLE t MODIFY COLUMN id TEXT")
	if err == nil {
		t.Fatal("expected error changing PK type")
	}
}

func TestAlterTableModifyColumnFirst(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, name TEXT, age BIGINT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 'alice', 30)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "ALTER TABLE t MODIFY COLUMN age BIGINT FIRST"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT age, id, name FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(result.Rows))
	}
	if result.Rows[0][0] != int64(30) {
		t.Errorf("age = %v, want 30", result.Rows[0][0])
	}
	if result.Rows[0][2] != "alice" {
		t.Errorf("name = %v, want alice", result.Rows[0][2])
	}
}

func TestAlterTableModifyColumnAfter(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, b TEXT, c TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 'bee', 'cee')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "ALTER TABLE t MODIFY COLUMN b TEXT AFTER c"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id, c, b FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][1] != "cee" {
		t.Errorf("c = %v, want cee", result.Rows[0][1])
	}
	if result.Rows[0][2] != "bee" {
		t.Errorf("b = %v, want bee", result.Rows[0][2])
	}
}

func TestAlterTableModifyNarrowingConversionRejected(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, val BIGINT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 99999)"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "ALTER TABLE t MODIFY COLUMN val TINYINT")
	if err == nil {
		t.Fatal("expected error for out-of-range narrowing conversion")
	}
}

func TestAlterTableModifyReorderPersists(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}

	eng, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, a TEXT, b TEXT, c TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 'aa', 'bb', 'cc')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "ALTER TABLE t MODIFY COLUMN a TEXT AFTER c"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	eng.Close()

	eng2, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()
	s2, _ := eng2.NewSession()
	defer s2.Close()
	result, err := s2.Query(ctx, "SELECT id, b, c, a FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][1] != "bb" {
		t.Errorf("b = %v, want bb", result.Rows[0][1])
	}
	if result.Rows[0][2] != "cc" {
		t.Errorf("c = %v, want cc", result.Rows[0][2])
	}
	if result.Rows[0][3] != "aa" {
		t.Errorf("a = %v, want aa", result.Rows[0][3])
	}
}

func TestAlterTableRenameColumn(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 'alice')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "ALTER TABLE t RENAME COLUMN name TO full_name"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id, full_name FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][1] != "alice" {
		t.Errorf("full_name = %v, want alice", result.Rows[0][1])
	}
}

func TestAlterTablePersistsThroughReopen(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}

	eng, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 'alice')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "ALTER TABLE t ADD COLUMN age BIGINT"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (2, 'bob', 25)"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	eng.Close()

	eng2, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()
	s2, _ := eng2.NewSession()
	defer s2.Close()
	result, err := s2.Query(ctx, "SELECT id, name, age FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(result.Rows))
	}
	if result.Rows[0][2] != nil {
		t.Errorf("row 1 age = %v, want nil", result.Rows[0][2])
	}
	if result.Rows[1][2] != int64(25) {
		t.Errorf("row 2 age = %v, want 25", result.Rows[1][2])
	}
}

func TestAlterTablePersistsNativeGit(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}

	nativeGit := engine.Options{Persistence: engine.PersistenceNativeGit}
	eng, err := engine.OpenWithOptions(ctx, root, nativeGit)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 'alice')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "ALTER TABLE t ADD COLUMN age BIGINT"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (2, 'bob', 25)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "ALTER TABLE t DROP COLUMN name"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	eng.Close()

	eng2, err := engine.OpenWithOptions(ctx, root, nativeGit)
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()
	s2, _ := eng2.NewSession()
	defer s2.Close()
	result, err := s2.Query(ctx, "SELECT id, age FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(result.Rows))
	}
	if result.Rows[0][1] != nil {
		t.Errorf("row 1 age = %v, want nil", result.Rows[0][1])
	}
	if result.Rows[1][1] != int64(25) {
		t.Errorf("row 2 age = %v, want 25", result.Rows[1][1])
	}
}

func TestStaleEmbeddedTransactionIsRejected(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, _ := engine.Open(ctx, root)
	defer eng.Close()
	a, _ := eng.NewSession()
	b, _ := eng.NewSession()
	if err := a.Exec(ctx, "CREATE TABLE issues (id BIGINT PRIMARY KEY, title TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	ta, _ := a.Begin(ctx)
	tb, _ := b.Begin(ctx)
	if err := ta.Exec(ctx, "INSERT INTO issues VALUES (1, 'winner')"); err != nil {
		t.Fatal(err)
	}
	if err := tb.Exec(ctx, "INSERT INTO issues VALUES (2, 'stale')"); err != nil {
		t.Fatal(err)
	}
	if err := ta.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tb.Commit(ctx); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("stale commit = %v", err)
	}
	reader, _ := eng.NewSession()
	result, err := reader.Query(ctx, "SELECT title FROM issues")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != "winner" {
		t.Fatalf("rows = %#v", result.Rows)
	}
}

func TestStatementFailureDoesNotAbortOrLeakPartialChanges(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, _ := engine.Open(ctx, root)
	defer eng.Close()
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE issues (id BIGINT PRIMARY KEY, title TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(ctx, "INSERT INTO issues VALUES (1, 'one')"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(ctx, "INSERT INTO issues VALUES (1, 'duplicate')"); err == nil {
		t.Fatal("duplicate insert succeeded")
	}
	if err := tx.Exec(ctx, "INSERT INTO issues VALUES (2, 'two')"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id, title FROM issues ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 || result.Rows[0][1] != "one" || result.Rows[1][1] != "two" {
		t.Fatalf("rows = %#v", result.Rows)
	}
	snapshot, err := eng.Repository().Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Manifest.Objects) > 4 {
		t.Fatalf("live inventory retained obsolete chunks: %d objects", len(snapshot.Manifest.Objects))
	}
}

func TestDeletePublishesRowRemoval(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, _ := engine.Open(ctx, root)
	defer eng.Close()
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE issues (id BIGINT PRIMARY KEY, title TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO issues VALUES (1, 'remove'), (2, 'keep')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "DELETE FROM issues WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id, title FROM issues ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != int64(2) || result.Rows[0][1] != "keep" {
		t.Fatalf("rows = %#v", result.Rows)
	}
}

func TestDDLUsesMySQLImplicitCommit(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, _ := engine.Open(ctx, root)
	defer eng.Close()
	s, _ := eng.NewSession()
	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(ctx, "CREATE TABLE durable_ddl (id BIGINT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(ctx, "SELECT * FROM durable_ddl"); err != nil {
		t.Fatalf("DDL was rolled back, want MySQL implicit commit: %v", err)
	}
}

func TestEmbeddedCommitOutcomeAndRecovery(t *testing.T) {
	for _, test := range []struct {
		name               string
		point              repository.PublicationPoint
		outcome, recovered repository.CommitOutcome
	}{
		{"committed", repository.AfterRefPublication, repository.OutcomeCommitted, repository.OutcomeCommitted},
		{"unknown", repository.DuringRefPublication, repository.OutcomeUnknown, repository.OutcomeRejected},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			root := gitRepository(t)
			repo, err := repository.Init(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			eng, _ := engine.NewWithOptions(repo, engine.Options{Persistence: engine.PersistenceNativeGit})
			defer eng.Close()
			session, _ := eng.NewSession()
			if err := session.Exec(ctx, "CREATE TABLE outcomes (id BIGINT PRIMARY KEY)"); err != nil {
				t.Fatal(err)
			}
			tx, _ := session.Begin(ctx)
			if err := tx.Exec(ctx, "INSERT INTO outcomes VALUES (1)"); err != nil {
				t.Fatal(err)
			}
			repo.SetPublicationFaultInjector(func(point repository.PublicationPoint) error {
				if point == test.point {
					return errors.New("injected SQL commit fault")
				}
				return nil
			})
			err = tx.Commit(ctx)
			var commitErr *repository.CommitError
			if !errors.As(err, &commitErr) || commitErr.Outcome != test.outcome || commitErr.Commit == "" {
				t.Fatalf("commit error = %#v (%v)", commitErr, err)
			}
			repo.SetPublicationFaultInjector(nil)
			recovered, err := repo.RecoverCommit(ctx, commitErr.Commit)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.Outcome != test.recovered {
				t.Fatalf("recovered = %v, want %v", recovered.Outcome, test.recovered)
			}
		})
	}
}

func TestMetadataCacheHitsOnRepeatedReads(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, _ := engine.Open(ctx, root)
	defer eng.Close()
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE items (id BIGINT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO items VALUES (1, 'cached')"); err != nil {
		t.Fatal(err)
	}
	engine.ResetPerformanceCounters()
	for i := 0; i < 5; i++ {
		result, err := s.Query(ctx, "SELECT value FROM items WHERE id = 1")
		if err != nil || len(result.Rows) != 1 || result.Rows[0][0] != "cached" {
			t.Fatalf("read %d: rows=%#v err=%v", i, result.Rows, err)
		}
	}
	counters := engine.ReadPerformanceCounters()
	if counters.MetadataCacheHits < 4 {
		t.Fatalf("expected at least 4 metadata cache hits, got %d hits / %d misses", counters.MetadataCacheHits, counters.MetadataCacheMisses)
	}
}

func TestCreateTableWithDefaultLiteral(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, name VARCHAR(100) DEFAULT 'unknown', score BIGINT DEFAULT 0)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t (id, name) VALUES (2, 'alice')"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id, name, score FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(result.Rows))
	}
	if result.Rows[0][1] != "unknown" {
		t.Errorf("row 1 name = %v, want 'unknown'", result.Rows[0][1])
	}
	if result.Rows[0][2] != int64(0) {
		t.Errorf("row 1 score = %v, want 0", result.Rows[0][2])
	}
	if result.Rows[1][1] != "alice" {
		t.Errorf("row 2 name = %v, want 'alice'", result.Rows[1][1])
	}
	if result.Rows[1][2] != int64(0) {
		t.Errorf("row 2 score = %v, want 0", result.Rows[1][2])
	}
}

func TestCreateTableWithDefaultExpression(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, val BIGINT DEFAULT (1 + 1))"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id, val FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(result.Rows))
	}
	if result.Rows[0][1] != int64(2) {
		t.Errorf("val = %v, want 2", result.Rows[0][1])
	}
}

func TestExpressionDefaultRoundTripThroughReopen(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}

	eng, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, val BIGINT DEFAULT ((2+2)/2))"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT val FROM t WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0] != int64(2) {
		t.Fatalf("before reopen: val = %v, want 2", result.Rows[0][0])
	}
	s.Close()
	eng.Close()

	eng2, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()
	s2, _ := eng2.NewSession()
	defer s2.Close()

	if err := s2.Exec(ctx, "INSERT INTO t (id) VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	result, err = s2.Query(ctx, "SELECT val FROM t WHERE id = 2")
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0] != int64(2) {
		t.Errorf("after reopen: val = %v, want 2 — expression default lost grouping during round-trip", result.Rows[0][0])
	}
}

func TestInsertExplicitDefaultKeyword(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, name VARCHAR(100) DEFAULT 'fallback')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, DEFAULT)"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT name FROM t WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != "fallback" {
		t.Errorf("name = %v, want 'fallback'", result.Rows[0][0])
	}
}

func TestAlterTableAddNotNullWithDefault(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 'alice')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (2, 'bob')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "ALTER TABLE t ADD COLUMN score BIGINT NOT NULL DEFAULT 0"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id, name, score FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(result.Rows))
	}
	if result.Rows[0][2] != int64(0) {
		t.Errorf("row 1 score = %v, want 0", result.Rows[0][2])
	}
	if result.Rows[1][2] != int64(0) {
		t.Errorf("row 2 score = %v, want 0", result.Rows[1][2])
	}
}

func TestAlterTableAddNotNullNoDefaultStillRejected(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "ALTER TABLE t ADD COLUMN name TEXT NOT NULL")
	if err == nil {
		t.Fatal("expected error adding NOT NULL column without DEFAULT to table with rows")
	}
}

func TestDefaultPersistsThroughReopen(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}

	eng, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, name VARCHAR(100) DEFAULT 'unknown')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	eng.Close()

	eng2, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()
	s2, _ := eng2.NewSession()
	defer s2.Close()

	if err := s2.Exec(ctx, "INSERT INTO t (id) VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	result, err := s2.Query(ctx, "SELECT id, name FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(result.Rows))
	}
	if result.Rows[0][1] != "unknown" {
		t.Errorf("row 1 name = %v, want 'unknown'", result.Rows[0][1])
	}
	if result.Rows[1][1] != "unknown" {
		t.Errorf("row 2 name = %v, want 'unknown'", result.Rows[1][1])
	}
}

func TestDefaultPersistsNativeGit(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}

	nativeGit := engine.Options{Persistence: engine.PersistenceNativeGit}
	eng, err := engine.OpenWithOptions(ctx, root, nativeGit)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, score BIGINT DEFAULT 42)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	eng.Close()

	eng2, err := engine.OpenWithOptions(ctx, root, nativeGit)
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()
	s2, _ := eng2.NewSession()
	defer s2.Close()

	if err := s2.Exec(ctx, "INSERT INTO t (id) VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	result, err := s2.Query(ctx, "SELECT id, score FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(result.Rows))
	}
	if result.Rows[0][1] != int64(42) {
		t.Errorf("row 1 score = %v, want 42", result.Rows[0][1])
	}
	if result.Rows[1][1] != int64(42) {
		t.Errorf("row 2 score = %v, want 42", result.Rows[1][1])
	}
}

func TestCheckConstraintOnCreateTable(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, age BIGINT, CHECK (age >= 0))"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 25)"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "INSERT INTO t VALUES (2, -1)")
	if err == nil {
		t.Fatal("expected check constraint violation")
	}
	result, err := s.Query(ctx, "SELECT COUNT(*) FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0] != int64(1) {
		t.Errorf("count = %v, want 1", result.Rows[0][0])
	}
	// Unnamed checks get a generated name like t_chk_1
	ddl, err := s.Query(ctx, "SHOW CREATE TABLE t")
	if err != nil {
		t.Fatal(err)
	}
	create := ddl.Rows[0][1].(string)
	if !strings.Contains(create, "t_chk_1") {
		t.Errorf("expected generated check name t_chk_1 in SHOW CREATE TABLE, got: %s", create)
	}
	// Generated name is droppable
	if err := s.Exec(ctx, "ALTER TABLE t DROP CONSTRAINT t_chk_1"); err != nil {
		t.Fatal("failed to drop unnamed check by generated name:", err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (2, -1)"); err != nil {
		t.Fatal("should succeed after dropping check:", err)
	}
}

func TestCheckConstraintNamed(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, score BIGINT, CONSTRAINT score_positive CHECK (score > 0))"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 10)"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "INSERT INTO t VALUES (2, 0)")
	if err == nil {
		t.Fatal("expected check constraint violation for score_positive")
	}
}

func TestCheckConstraintOnUpdate(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, age BIGINT, CHECK (age >= 0))"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 25)"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "UPDATE t SET age = -5 WHERE id = 1")
	if err == nil {
		t.Fatal("expected check constraint violation on UPDATE")
	}
	result, err := s.Query(ctx, "SELECT age FROM t WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0] != int64(25) {
		t.Errorf("age = %v, want 25 (should not have changed)", result.Rows[0][0])
	}
}

func TestAlterTableAddCheck(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, val BIGINT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 10)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "ALTER TABLE t ADD CONSTRAINT val_positive CHECK (val > 0)"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "INSERT INTO t VALUES (2, -1)")
	if err == nil {
		t.Fatal("expected check constraint violation after ALTER TABLE ADD CHECK")
	}
}

func TestAlterTableAddCheckRejectsExistingViolations(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, val BIGINT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, -5)"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "ALTER TABLE t ADD CHECK (val >= 0)")
	if err == nil {
		t.Fatal("expected error: existing rows violate the check constraint")
	}
}

func TestAlterTableDropCheck(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, val BIGINT, CONSTRAINT val_chk CHECK (val > 0))"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "INSERT INTO t VALUES (1, -1)")
	if err == nil {
		t.Fatal("expected check violation before drop")
	}
	if err := s.Exec(ctx, "ALTER TABLE t DROP CHECK val_chk"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, -1)"); err != nil {
		t.Fatal("should succeed after dropping check:", err)
	}
}

func TestCheckConstraintPersistsThroughReopen(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}

	eng, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, age BIGINT, CHECK (age >= 0))"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 25)"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	eng.Close()

	eng2, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()
	s2, _ := eng2.NewSession()
	defer s2.Close()

	if err := s2.Exec(ctx, "INSERT INTO t VALUES (2, 30)"); err != nil {
		t.Fatal(err)
	}
	err = s2.Exec(ctx, "INSERT INTO t VALUES (3, -1)")
	if err == nil {
		t.Fatal("expected check constraint violation after reopen")
	}
}

func TestCheckConstraintPersistsNativeGit(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}

	nativeGit := engine.Options{Persistence: engine.PersistenceNativeGit}
	eng, err := engine.OpenWithOptions(ctx, root, nativeGit)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, val BIGINT, CONSTRAINT pos CHECK (val > 0))"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 42)"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	eng.Close()

	eng2, err := engine.OpenWithOptions(ctx, root, nativeGit)
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()
	s2, _ := eng2.NewSession()
	defer s2.Close()

	err = s2.Exec(ctx, "INSERT INTO t VALUES (2, -1)")
	if err == nil {
		t.Fatal("expected check constraint violation after native-git reopen")
	}
	if err := s2.Exec(ctx, "INSERT INTO t VALUES (2, 100)"); err != nil {
		t.Fatal(err)
	}
}

func TestDropCheckDoesNotCorruptCache(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, a BIGINT, b BIGINT, CONSTRAINT chk_a CHECK (a > 0), CONSTRAINT chk_b CHECK (b > 0))"); err != nil {
		t.Fatal(err)
	}
	// Insert a valid row to ensure the table is committed
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 10, 20)"); err != nil {
		t.Fatal(err)
	}
	// Drop the first check — must not corrupt the second
	if err := s.Exec(ctx, "ALTER TABLE t DROP CONSTRAINT chk_a"); err != nil {
		t.Fatal(err)
	}
	// chk_b should still be enforced
	err := s.Exec(ctx, "INSERT INTO t VALUES (2, -1, -1)")
	if err == nil {
		t.Fatal("expected chk_b violation after dropping chk_a")
	}
	// chk_a should no longer be enforced
	if err := s.Exec(ctx, "INSERT INTO t VALUES (2, -1, 5)"); err != nil {
		t.Fatal("chk_a should be gone:", err)
	}
}

func TestShowCreateTableIncludesCheck(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, val BIGINT, CONSTRAINT val_range CHECK (val BETWEEN 1 AND 100))"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SHOW CREATE TABLE t")
	if err != nil {
		t.Fatal(err)
	}
	ddl := result.Rows[0][1].(string)
	if !strings.Contains(ddl, "val_range") {
		t.Errorf("SHOW CREATE TABLE missing check name, got: %s", ddl)
	}
}

func TestNotNullOnNonPKColumn(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, name VARCHAR(100) NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 'alice')"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "INSERT INTO t VALUES (2, NULL)")
	if err == nil {
		t.Fatal("expected NOT NULL violation")
	}
}

func TestTemporalTypeDDL(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE events (id BIGINT PRIMARY KEY, d DATE, t TIME, dt DATETIME, ts TIMESTAMP)"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SHOW CREATE TABLE events")
	if err != nil {
		t.Fatal(err)
	}
	ddl := result.Rows[0][1].(string)
	for _, keyword := range []string{"date", "time", "datetime", "timestamp"} {
		if !strings.Contains(strings.ToLower(ddl), keyword) {
			t.Errorf("SHOW CREATE TABLE missing %s, got: %s", keyword, ddl)
		}
	}
}

func TestTemporalTypeDDLWithPrecision(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE events (id BIGINT PRIMARY KEY, dt DATETIME(3), ts TIMESTAMP(6))"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO events VALUES (1, '2024-06-15 10:30:00.123', '2024-06-15 10:30:00.123456')"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT dt, ts FROM events WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	dt := result.Rows[0][0].(time.Time)
	ts := result.Rows[0][1].(time.Time)
	if dt.Nanosecond()/1000000 != 123 {
		t.Errorf("DATETIME(3) milliseconds = %d, want 123", dt.Nanosecond()/1000000)
	}
	if ts.Nanosecond()/1000 != 123456 {
		t.Errorf("TIMESTAMP(6) microseconds = %d, want 123456", ts.Nanosecond()/1000)
	}
}

func TestDateInsertSelectRoundTrip(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, d DATE)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, '2024-06-15')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (2, '1999-12-31')"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id, d FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(result.Rows))
	}
	d1 := result.Rows[0][1].(time.Time)
	d2 := result.Rows[1][1].(time.Time)
	if d1.Format("2006-01-02") != "2024-06-15" {
		t.Errorf("row 1 date = %v, want 2024-06-15", d1)
	}
	if d2.Format("2006-01-02") != "1999-12-31" {
		t.Errorf("row 2 date = %v, want 1999-12-31", d2)
	}
}

func TestDatetimeInsertSelectRoundTrip(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, dt DATETIME)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, '2024-06-15 14:30:45')"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT dt FROM t WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	dt := result.Rows[0][0].(time.Time)
	if dt.Year() != 2024 || dt.Month() != 6 || dt.Day() != 15 || dt.Hour() != 14 || dt.Minute() != 30 || dt.Second() != 45 {
		t.Errorf("datetime = %v, want 2024-06-15 14:30:45", dt)
	}
}

func TestTimestampInsertSelectRoundTrip(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, ts TIMESTAMP)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, '2024-06-15 14:30:45')"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT ts FROM t WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	ts := result.Rows[0][0].(time.Time)
	if ts.Year() != 2024 || ts.Month() != 6 || ts.Day() != 15 {
		t.Errorf("timestamp = %v, want 2024-06-15 14:30:45", ts)
	}
}

func TestTimeInsertSelectRoundTrip(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, t TIME)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, '14:30:45')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (2, '-01:15:00')"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id, t FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(result.Rows))
	}
}

func TestTemporalOrderBy(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, dt DATETIME)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, '2024-06-15 10:00:00')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (2, '2020-01-01 00:00:00')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (3, '2024-06-15 23:59:59')"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id FROM t ORDER BY dt")
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(result.Rows))
	for i, row := range result.Rows {
		ids[i] = row[0].(int64)
	}
	if ids[0] != 2 || ids[1] != 1 || ids[2] != 3 {
		t.Errorf("ORDER BY dt = %v, want [2 1 3]", ids)
	}
}

func TestTemporalParameterBinding(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, dt DATETIME)"); err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2024, 6, 15, 14, 30, 45, 0, time.UTC)
	if err := s.Exec(ctx, "INSERT INTO t VALUES (?, ?)", int64(1), ts); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT dt FROM t WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	got := result.Rows[0][0].(time.Time)
	if !got.Equal(ts) {
		t.Errorf("parameter binding: got %v, want %v", got, ts)
	}
}

func TestTemporalParameterBindingNonUTC(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, dt DATETIME)"); err != nil {
		t.Fatal(err)
	}
	eastern := time.FixedZone("EST", -5*3600)
	ts := time.Date(2024, 6, 15, 14, 30, 45, 0, eastern)
	if err := s.Exec(ctx, "INSERT INTO t VALUES (?, ?)", int64(1), ts); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT dt FROM t WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	got := result.Rows[0][0].(time.Time)
	if !got.Equal(ts) {
		t.Errorf("non-UTC parameter binding: got %v (unix %d), want %v (unix %d)", got, got.Unix(), ts, ts.Unix())
	}
}

func TestTemporalNullable(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, d DATE, dt DATETIME)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (2, '2024-06-15', '2024-06-15 10:00:00')"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id, d, dt FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][1] != nil {
		t.Errorf("row 1 d = %v, want nil", result.Rows[0][1])
	}
	if result.Rows[0][2] != nil {
		t.Errorf("row 1 dt = %v, want nil", result.Rows[0][2])
	}
	if result.Rows[1][1] == nil {
		t.Error("row 2 d = nil, want non-nil")
	}
}

func TestTemporalPrimaryKey(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (ts DATETIME PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES ('2024-06-15 10:00:00', 'first')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES ('2024-06-15 11:00:00', 'second')"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "INSERT INTO t VALUES ('2024-06-15 10:00:00', 'duplicate')")
	if err == nil {
		t.Fatal("expected duplicate key error")
	}
	result, err := s.Query(ctx, "SELECT name FROM t WHERE ts = '2024-06-15 10:00:00'")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != "first" {
		t.Fatalf("point lookup = %#v", result.Rows)
	}
}

func TestTemporalPersistsThroughReopen(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}

	eng, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, d DATE, dt DATETIME, ts TIMESTAMP, t TIME)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, '2024-06-15', '2024-06-15 14:30:45', '2024-06-15 14:30:45', '14:30:45')"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	eng.Close()

	eng2, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()
	s2, _ := eng2.NewSession()
	defer s2.Close()

	result, err := s2.Query(ctx, "SELECT d, dt, ts, t FROM t WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	d := result.Rows[0][0].(time.Time)
	if d.Format("2006-01-02") != "2024-06-15" {
		t.Errorf("date after reopen = %v, want 2024-06-15", d)
	}
	dt := result.Rows[0][1].(time.Time)
	if dt.Hour() != 14 || dt.Minute() != 30 || dt.Second() != 45 {
		t.Errorf("datetime after reopen = %v", dt)
	}
}

func TestTemporalPersistsNativeGit(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}

	nativeGit := engine.Options{Persistence: engine.PersistenceNativeGit}
	eng, err := engine.OpenWithOptions(ctx, root, nativeGit)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, d DATE, dt DATETIME)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, '2024-06-15', '2024-06-15 14:30:45')"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	eng.Close()

	eng2, err := engine.OpenWithOptions(ctx, root, nativeGit)
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()
	s2, _ := eng2.NewSession()
	defer s2.Close()

	result, err := s2.Query(ctx, "SELECT d, dt FROM t WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	d := result.Rows[0][0].(time.Time)
	if d.Format("2006-01-02") != "2024-06-15" {
		t.Errorf("date after native-git reopen = %v", d)
	}
	dt := result.Rows[0][1].(time.Time)
	if dt.Hour() != 14 || dt.Minute() != 30 || dt.Second() != 45 {
		t.Errorf("datetime after native-git reopen = %v", dt)
	}
}

func TestTemporalDefault(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, d DATE DEFAULT '2024-01-01')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT d FROM t WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	d := result.Rows[0][0].(time.Time)
	if d.Format("2006-01-02") != "2024-01-01" {
		t.Errorf("default date = %v, want 2024-01-01", d)
	}
}

func TestJSONTypeDDL(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, data JSON)"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SHOW CREATE TABLE t")
	if err != nil {
		t.Fatal(err)
	}
	ddl := result.Rows[0][1].(string)
	if !strings.Contains(strings.ToLower(ddl), "json") {
		t.Errorf("SHOW CREATE TABLE missing json, got: %s", ddl)
	}
}

func TestJSONInsertSelectRoundTrip(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, data JSON)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, `INSERT INTO t VALUES (1, '{"name":"alice","age":30}')`); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, `INSERT INTO t VALUES (2, '[1, 2, 3]')`); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, `INSERT INTO t VALUES (3, '"just a string"')`); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id, data FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(result.Rows))
	}
	obj := fmt.Sprint(result.Rows[0][1])
	if obj != `{"age": 30, "name": "alice"}` {
		t.Errorf("row 1 JSON object = %v, want exact {\"age\": 30, \"name\": \"alice\"}", obj)
	}
	arr := fmt.Sprint(result.Rows[1][1])
	if arr != `[1, 2, 3]` {
		t.Errorf("row 2 JSON array = %v, want exact [1, 2, 3]", arr)
	}
	scalar := fmt.Sprint(result.Rows[2][1])
	if scalar != `"just a string"` {
		t.Errorf("row 3 JSON scalar = %v, want exact \"just a string\"", scalar)
	}
}

func TestJSONExtractFunction(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, data JSON)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, `INSERT INTO t VALUES (1, '{"name":"alice","score":95}')`); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, `SELECT JSON_UNQUOTE(JSON_EXTRACT(data, '$.name')) FROM t WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(result.Rows))
	}
	got := result.Rows[0][0]
	if got != "alice" {
		t.Errorf("JSON_EXTRACT name = %v (%T), want 'alice'", got, got)
	}
}

func TestJSONObjectFunction(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, data JSON)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, `INSERT INTO t VALUES (1, JSON_OBJECT('key', 'value'))`); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, `SELECT JSON_UNQUOTE(JSON_EXTRACT(data, '$.key')) FROM t WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(result.Rows))
	}
	got := result.Rows[0][0]
	if got != "value" {
		t.Errorf("JSON_OBJECT key = %v (%T), want 'value'", got, got)
	}
}

func TestJSONInvalidRejected(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, data JSON)"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, `INSERT INTO t VALUES (1, '{not valid json}')`)
	if err == nil {
		t.Fatal("expected error inserting invalid JSON")
	}
}

func TestJSONNullable(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, data JSON)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT data FROM t WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0] != nil {
		t.Errorf("data = %v, want nil", result.Rows[0][0])
	}
}

func TestJSONPersistsThroughReopen(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}

	eng, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, data JSON)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, `INSERT INTO t VALUES (1, '{"key":"value","nested":{"a":1}}')`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	eng.Close()

	eng2, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()
	s2, _ := eng2.NewSession()
	defer s2.Close()

	result, err := s2.Query(ctx, `SELECT JSON_UNQUOTE(JSON_EXTRACT(data, '$.key')), JSON_EXTRACT(data, '$.nested.a') FROM t WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("got %d rows after reopen, want 1", len(result.Rows))
	}
	if result.Rows[0][0] != "value" {
		t.Errorf("key after reopen = %v, want 'value'", result.Rows[0][0])
	}
	nested := fmt.Sprint(result.Rows[0][1])
	if nested != "1" {
		t.Errorf("nested.a after reopen = %v, want 1", nested)
	}
}

func TestDecimalDDL(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, price DECIMAL(10,2), quantity NUMERIC(5,3))"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SHOW CREATE TABLE t")
	if err != nil {
		t.Fatal(err)
	}
	ddl := strings.ToLower(result.Rows[0][1].(string))
	if !strings.Contains(ddl, "decimal") {
		t.Errorf("SHOW CREATE TABLE missing decimal, got: %s", ddl)
	}
}

func TestDecimalBareType(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, val DECIMAL)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 1234567890)"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT val FROM t WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(result.Rows[0][0])
	if got != "1234567890" {
		t.Errorf("bare DECIMAL = %v, want 1234567890", got)
	}
}

func TestDecimalRoundTrip(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, price DECIMAL(10,2))"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 99.99)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (2, 0.10)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (3, 12345678.50)"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id, price FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(result.Rows))
	}
	expected := []string{"99.99", "0.1", "12345678.5"}
	for i, want := range expected {
		got := fmt.Sprint(result.Rows[i][1])
		if got != want {
			t.Errorf("row %d price = %v, want %v", i+1, got, want)
		}
	}
}

func TestDecimalPrecisionOverflow(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, val DECIMAL(5,2))"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 999.99)"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "INSERT INTO t VALUES (2, 1000.00)")
	if err == nil {
		t.Fatal("expected error inserting value exceeding DECIMAL(5,2) precision")
	}
}

func TestDecimalScaleRounding(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, val DECIMAL(5,2))"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 1.999)"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT val FROM t WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(result.Rows[0][0])
	if got != "2" && got != "2.00" {
		t.Errorf("DECIMAL(5,2) of 1.999 = %v, want 2.00 (rounded)", got)
	}
}

func TestDecimalArithmetic(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, price DECIMAL(10,2), qty DECIMAL(10,3))"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 19.99, 3.000)"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT price * qty FROM t WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(result.Rows[0][0])
	if got != "59.97" && got != "59.970" && got != "59.97000" {
		t.Errorf("price * qty = %v, want 59.97", got)
	}
}

func TestDecimalOrderBy(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, val DECIMAL(10,2))"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 10.50)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (2, 1.25)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (3, 100.00)"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT id FROM t ORDER BY val")
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(result.Rows))
	for i, row := range result.Rows {
		ids[i] = row[0].(int64)
	}
	if ids[0] != 2 || ids[1] != 1 || ids[2] != 3 {
		t.Errorf("ORDER BY val = %v, want [2 1 3]", ids)
	}
}

func TestDecimalNullable(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, val DECIMAL(10,2))"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT val FROM t WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows[0][0] != nil {
		t.Errorf("val = %v, want nil", result.Rows[0][0])
	}
}

func TestDecimalPersistsThroughReopen(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}

	eng, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := eng.NewSession()
	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, price DECIMAL(10,2))"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (1, 99.99)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t VALUES (2, 0.01)"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	eng.Close()

	eng2, err := engine.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()
	s2, _ := eng2.NewSession()
	defer s2.Close()

	result, err := s2.Query(ctx, "SELECT id, price FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("got %d rows after reopen, want 2", len(result.Rows))
	}
	if got := fmt.Sprint(result.Rows[0][1]); got != "99.99" {
		t.Errorf("price after reopen = %v, want 99.99", got)
	}
	if got := fmt.Sprint(result.Rows[1][1]); got != "0.01" {
		t.Errorf("price after reopen = %v, want 0.01", got)
	}
}

func TestDecimalDefault(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, price DECIMAL(10,2) DEFAULT 9.99)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "SELECT price FROM t WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(result.Rows[0][0]); got != "9.99" {
		t.Errorf("default price = %v, want 9.99", got)
	}
}

func gitRepository(t testing.TB) string {
	t.Helper()
	root := t.TempDir()
	cmd := exec.Command("git", "init", "--quiet", "-b", "main")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	return root
}
