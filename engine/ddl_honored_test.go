package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

func ddlSession(t *testing.T) (*engine.Session, context.Context) {
	t.Helper()
	eng, ctx := openEngine(t)
	s, err := eng.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Exec(ctx, "CREATE TABLE t (id BIGINT PRIMARY KEY, v VARCHAR(20), doc JSON)"); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func expectError(t *testing.T, s *engine.Session, ctx context.Context, statement, want string) {
	t.Helper()
	err := s.Exec(ctx, statement)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: err = %v, want error containing %q", statement, err, want)
	}
	if strings.Contains(err.Error(), "%!") {
		t.Fatalf("%s: garbled error %q", statement, err)
	}
}

func showCreate(t *testing.T, s *engine.Session, ctx context.Context, table string) string {
	t.Helper()
	result, err := s.Query(ctx, "SHOW CREATE TABLE "+table)
	if err != nil {
		t.Fatal(err)
	}
	return result.Rows[0][1].(string)
}

func TestCreateViewIsRejected(t *testing.T) {
	s, ctx := ddlSession(t)
	expectError(t, s, ctx, "CREATE VIEW v1 AS SELECT id FROM t", "views are not supported")
	if _, err := s.Query(ctx, "SELECT * FROM v1"); err == nil {
		t.Fatal("view v1 is visible after a rejected CREATE VIEW")
	}
}

func TestJSONKeyColumnsAreRejected(t *testing.T) {
	s, ctx := ddlSession(t)
	expectError(t, s, ctx, "CREATE TABLE j (id JSON PRIMARY KEY)", "cannot be part of a key")
	// go-mysql-server itself rejects JSON in composite keys, indexes and ALTER TABLE.
	expectError(t, s, ctx, "CREATE TABLE j2 (a BIGINT, b JSON, PRIMARY KEY (a, b))", "JSON column 'b'")
	if err := s.Exec(ctx, "CREATE TABLE j3 (id BIGINT PRIMARY KEY, a VARCHAR(10))"); err != nil {
		t.Fatal(err)
	}
	expectError(t, s, ctx, "ALTER TABLE j3 MODIFY id JSON", "JSON column 'id'")
	if err := s.Exec(ctx, "CREATE INDEX va ON j3 (a)"); err != nil {
		t.Fatal(err)
	}
	expectError(t, s, ctx, "ALTER TABLE j3 MODIFY a JSON", "JSON column 'a'")
	// A JSON non-key column works.
	if err := s.Exec(ctx, `INSERT INTO t VALUES (1, 'a', '{"k": 1}')`); err != nil {
		t.Fatal(err)
	}
}

func TestIndexPrefixLengthsAreRejected(t *testing.T) {
	s, ctx := ddlSession(t)
	for _, statement := range []string{
		"CREATE INDEX pre ON t (v(5))",
		"CREATE UNIQUE INDEX upre ON t (v(5))",
		"ALTER TABLE t ADD INDEX apre (v(5))",
		"CREATE TABLE p1 (id BIGINT PRIMARY KEY, v VARCHAR(20), INDEX (v(5)))",
		"CREATE TABLE p2 (v VARCHAR(20), PRIMARY KEY (v(5)))",
	} {
		expectError(t, s, ctx, statement, "prefix lengths are not supported")
	}
	if err := s.Exec(ctx, "CREATE INDEX whole ON t (v)"); err != nil {
		t.Fatal(err)
	}
}

func TestTimePrecisionIsPersisted(t *testing.T) {
	for _, mode := range []engine.PersistenceMode{engine.PersistenceJournal, engine.PersistenceNativeGit} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := context.Background()
			root := gitRepository(t)
			if _, err := repository.Init(ctx, root); err != nil {
				t.Fatal(err)
			}
			open := func() (*engine.Engine, *engine.Session) {
				eng, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: mode})
				if err != nil {
					t.Fatal(err)
				}
				s, err := eng.NewSession()
				if err != nil {
					t.Fatal(err)
				}
				return eng, s
			}
			eng, s := open()
			if err := s.Exec(ctx, "CREATE TABLE tm (id BIGINT PRIMARY KEY, x TIME(3), y TIME)"); err != nil {
				t.Fatal(err)
			}
			if err := s.Exec(ctx, "INSERT INTO tm VALUES (1, '12:34:56.789', '01:02:03')"); err != nil {
				t.Fatal(err)
			}
			if got := showCreate(t, s, ctx, "tm"); !strings.Contains(got, "`x` time(3)") {
				t.Fatalf("SHOW CREATE TABLE = %s, want time(3)", got)
			}
			if err := s.Exec(ctx, "ALTER TABLE tm MODIFY y TIME(6)"); err != nil {
				t.Fatal(err)
			}
			s.Close()
			eng.Close()

			eng, s = open()
			defer eng.Close()
			defer s.Close()
			got := showCreate(t, s, ctx, "tm")
			if !strings.Contains(got, "`x` time(3)") || !strings.Contains(got, "`y` time(6)") {
				t.Fatalf("SHOW CREATE TABLE after reopen = %s", got)
			}
			result, err := s.Query(ctx, "SELECT CAST(x AS CHAR) FROM tm WHERE id = 1")
			if err != nil || len(result.Rows) != 1 || result.Rows[0][0] != "12:34:56.789" {
				t.Fatalf("x = %#v, %v; want 12:34:56.789", result.Rows, err)
			}
		})
	}
}

func TestFulltextIndexesAreRejected(t *testing.T) {
	s, ctx := ddlSession(t)
	for _, statement := range []string{
		"CREATE FULLTEXT INDEX ft ON t (v)",
		"ALTER TABLE t ADD FULLTEXT INDEX ft (v)",
		"CREATE TABLE f (id BIGINT PRIMARY KEY, v TEXT, FULLTEXT KEY ft (v))",
	} {
		expectError(t, s, ctx, statement, "does not support FULLTEXT indexes")
	}
	if _, err := s.Query(ctx, "SELECT * FROM f"); err == nil {
		t.Fatal("table f exists after a rejected CREATE TABLE")
	}
}

func TestPrimaryKeyChangesAreRejected(t *testing.T) {
	s, ctx := ddlSession(t)
	before := showCreate(t, s, ctx, "t")
	expectError(t, s, ctx, "ALTER TABLE t DROP PRIMARY KEY", "changing a table's primary key is not supported")
	expectError(t, s, ctx, "ALTER TABLE t ADD PRIMARY KEY (v)", "primary key")
	if after := showCreate(t, s, ctx, "t"); after != before {
		t.Fatalf("schema changed:\n%s\n%s", before, after)
	}
}
