package engine_test

import (
	"context"
	"errors"
	"os/exec"
	"testing"

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
	err := s.Exec(ctx, "ALTER TABLE t ADD COLUMN d DATE")
	if err == nil {
		t.Fatal("expected error adding unsupported DATE column")
	}
}

func TestAlterTableModifyToUnsupportedTypeRejected(t *testing.T) {
	eng, ctx := openEngine(t)
	s, _ := eng.NewSession()
	defer s.Close()

	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, val TEXT)"); err != nil {
		t.Fatal(err)
	}
	err := s.Exec(ctx, "ALTER TABLE t MODIFY COLUMN val DATE")
	if err == nil {
		t.Fatal("expected error modifying to unsupported DATE type")
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
