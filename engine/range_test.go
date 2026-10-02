package engine_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

func explainPlan(t *testing.T, eng *engine.Engine, query string) string {
	t.Helper()
	session, _ := eng.NewSession()
	defer session.Close()
	result, err := session.Query(context.Background(), "EXPLAIN PLAN "+query)
	if err != nil {
		t.Fatalf("EXPLAIN PLAN %s: %v", query, err)
	}
	var out strings.Builder
	for _, row := range result.Rows {
		fmt.Fprintln(&out, row...)
	}
	return out.String()
}

// The planner must answer ranges with index scans and drop sorts that the
// ascending key order already satisfies.
func TestRangeAndOrderPlansUseIndexes(t *testing.T) {
	_, eng := openCheckpointedIndexedTable(t)
	defer eng.Close()
	for _, tc := range []struct {
		query   string
		index   string
		noSort  bool
		wantAny []string
	}{
		{"SELECT id FROM t WHERE id BETWEEN 1 AND 2", "[t.id]", false, nil},
		{"SELECT id FROM t WHERE id > 1", "[t.id]", false, nil},
		{"SELECT id FROM t ORDER BY id LIMIT 1", "[t.id]", true, nil},
		{"SELECT id FROM t WHERE id >= 2 ORDER BY id LIMIT 5", "[t.id]", true, nil},
		{"SELECT id FROM t WHERE u > 5 ORDER BY u LIMIT 1", "[t.u]", true, nil},
		{"SELECT id FROM t WHERE n BETWEEN 'a' AND 'c'", "[t.n]", false, nil},
		{"SELECT id FROM t ORDER BY id DESC LIMIT 1", "[t.id]", true, []string{"reverse: true"}},
		{"SELECT id FROM t WHERE id < 2 ORDER BY id DESC", "[t.id]", true, []string{"reverse: true"}},
		{"SELECT id FROM t WHERE u > 5 ORDER BY u DESC LIMIT 1", "[t.u]", true, []string{"reverse: true"}},
	} {
		plan := explainPlan(t, eng, tc.query)
		if !strings.Contains(plan, "IndexedTableAccess") || !strings.Contains(plan, "index: "+tc.index) {
			t.Errorf("%s: plan does not use index %s:\n%s", tc.query, tc.index, plan)
		}
		if tc.noSort && (strings.Contains(plan, "Sort") || strings.Contains(plan, "TopN")) {
			t.Errorf("%s: plan still sorts:\n%s", tc.query, plan)
		}
		for _, want := range tc.wantAny {
			if !strings.Contains(plan, want) {
				t.Errorf("%s: plan lacks %q:\n%s", tc.query, want, plan)
			}
		}
	}
}

// rangeModelTable describes one table shape for TestRangeQueriesMatchFullScan.
type rangeModelTable struct {
	name   string
	ddl    string
	row    func(r *rand.Rand, id int) string // VALUES tuple
	pk     string                            // primary key column
	pkLits func(r *rand.Rand) string         // a literal comparable with pk
	idx    string                            // indexed nullable column
	idxLit func(r *rand.Rand) string
}

func rangeModelTables() []rangeModelTable {
	return []rangeModelTable{
		{
			name: "signed",
			ddl:  "CREATE TABLE signed (k BIGINT PRIMARY KEY, v INT, tag VARCHAR(10), KEY v_idx (v))",
			row: func(r *rand.Rand, id int) string {
				v := "NULL"
				if r.IntN(5) != 0 {
					v = fmt.Sprint(r.IntN(41) - 20)
				}
				return fmt.Sprintf("(%d, %s, 't%d')", id*7-150, v, id)
			},
			pk: "k", pkLits: func(r *rand.Rand) string { return fmt.Sprint(r.IntN(400) - 200) },
			idx: "v", idxLit: func(r *rand.Rand) string { return fmt.Sprint(r.IntN(45) - 22) },
		},
		{
			name: "decimals",
			ddl:  "CREATE TABLE decimals (k DECIMAL(10,2) PRIMARY KEY, v DECIMAL(8,3), KEY v_idx (v))",
			row: func(r *rand.Rand, id int) string {
				v := "NULL"
				if r.IntN(5) != 0 {
					v = fmt.Sprintf("%d.%03d", r.IntN(21)-10, r.IntN(1000))
				}
				return fmt.Sprintf("(%d.%02d, %s)", id-40, r.IntN(100), v)
			},
			pk: "k", pkLits: func(r *rand.Rand) string { return fmt.Sprintf("%d.%d", r.IntN(100)-50, r.IntN(10)) },
			idx: "v", idxLit: func(r *rand.Rand) string { return fmt.Sprintf("%d.%d", r.IntN(24)-12, r.IntN(10)) },
		},
		{
			name: "strings",
			ddl:  "CREATE TABLE strings (k VARCHAR(20) COLLATE utf8mb4_0900_ai_ci PRIMARY KEY, v VARCHAR(10), KEY v_idx (v))",
			row: func(r *rand.Rand, id int) string {
				v := "NULL"
				if r.IntN(5) != 0 {
					v = fmt.Sprintf("'%c%c'", 'a'+rune(r.IntN(4)), 'a'+rune(r.IntN(4)))
				}
				prefix := []string{"", "a", "Ab", "b", "Zz", "zZ"}[id%6]
				return fmt.Sprintf("('%s%03d', %s)", prefix, id, v)
			},
			pk: "k", pkLits: func(r *rand.Rand) string {
				return fmt.Sprintf("'%s%d'", []string{"", "a", "AB", "b", "z"}[r.IntN(5)], r.IntN(100))
			},
			idx: "v", idxLit: func(r *rand.Rand) string { return fmt.Sprintf("'%c'", 'a'+rune(r.IntN(5))) },
		},
		{
			name: "datetimes",
			ddl:  "CREATE TABLE datetimes (k DATETIME(6) PRIMARY KEY, v DATE, KEY v_idx (v))",
			row: func(r *rand.Rand, id int) string {
				v := "NULL"
				if r.IntN(5) != 0 {
					v = fmt.Sprintf("'19%02d-0%d-1%d'", 60+r.IntN(20), 1+r.IntN(9), r.IntN(10))
				}
				return fmt.Sprintf("('%d-01-01 00:00:00.%06d', %s)", 1950+id, r.IntN(1_000_000), v)
			},
			pk: "k", pkLits: func(r *rand.Rand) string { return fmt.Sprintf("'%d-06-15'", 1940+r.IntN(90)) },
			idx: "v", idxLit: func(r *rand.Rand) string { return fmt.Sprintf("'19%02d-05-05'", 58+r.IntN(24)) },
		},
	}
}

// Range, IN, IS NULL and ordered-LIMIT queries answered by index scans must
// return exactly what the same query returns when the index cannot be used
// (the column wrapped in a type- and collation-preserving no-op expression
// forces a filtered full scan). Covered
// for native-Git, journal with pending edits over checkpointed trees, and an
// open transaction with uncommitted writes.
func TestRangeQueriesMatchFullScan(t *testing.T) {
	for _, mode := range []string{"native-git", "journal", "open-transaction"} {
		for _, tbl := range rangeModelTables() {
			t.Run(mode+"/"+tbl.name, func(t *testing.T) {
				runRangeModel(t, mode, tbl)
			})
		}
	}
}

func runRangeModel(t *testing.T, mode string, tbl rangeModelTable) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	persistence := engine.PersistenceJournal
	if mode == "native-git" {
		persistence = engine.PersistenceNativeGit
	}
	eng, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: persistence})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	r := rand.New(rand.NewPCG(3, uint64(len(tbl.name))))
	var values []string
	for id := 0; id < 60; id++ {
		values = append(values, tbl.row(r, id))
	}
	execAll(t, eng, tbl.ddl, fmt.Sprintf("INSERT INTO %s VALUES %s", tbl.name, strings.Join(values, ", ")))
	if persistence == engine.PersistenceJournal {
		// Persist trees, then leave pending edits on top of them.
		if _, err := eng.Checkpoint(ctx, "base"); err != nil {
			t.Fatal(err)
		}
	}
	var more []string
	for id := 60; id < 80; id++ {
		more = append(more, tbl.row(r, id))
	}
	execAll(t, eng,
		fmt.Sprintf("INSERT INTO %s VALUES %s", tbl.name, strings.Join(more, ", ")),
		fmt.Sprintf("DELETE FROM %s WHERE %s IN (SELECT %s FROM (SELECT %s FROM %s ORDER BY %s LIMIT 5) d)", tbl.name, tbl.pk, tbl.pk, tbl.pk, tbl.name, tbl.pk),
		fmt.Sprintf("UPDATE %s SET %s = NULL WHERE %s IN (SELECT %s FROM (SELECT %s FROM %s ORDER BY %s DESC LIMIT 5) d)", tbl.name, tbl.idx, tbl.pk, tbl.pk, tbl.pk, tbl.name, tbl.pk),
	)

	session, _ := eng.NewSession()
	defer session.Close()
	query := func(q string) string {
		result, err := session.Query(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return fmt.Sprint(result.Rows)
	}
	if mode == "open-transaction" {
		tx, err := session.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		var extra []string
		for id := 80; id < 90; id++ {
			extra = append(extra, tbl.row(r, id))
		}
		for _, stmt := range []string{
			fmt.Sprintf("INSERT INTO %s VALUES %s", tbl.name, strings.Join(extra, ", ")),
			fmt.Sprintf("DELETE FROM %s WHERE %s IN (SELECT %s FROM (SELECT %s FROM %s ORDER BY %s LIMIT 3 OFFSET 10) d)", tbl.name, tbl.pk, tbl.pk, tbl.pk, tbl.name, tbl.pk),
		} {
			if err := tx.Exec(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		query = func(q string) string {
			result, err := tx.Query(ctx, q)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			return fmt.Sprint(result.Rows)
		}
	}

	pk, idx := tbl.pk, tbl.idx
	// scan forces a full scan of the same predicate: numeric columns via +0,
	// others via CONCAT, which the planner cannot answer with an index.
	// The wrapper must keep the column's type and collation, so it compares
	// exactly like the column itself.
	scanExpr := func(col string) string {
		switch {
		case tbl.name == "signed" || tbl.name == "decimals":
			return "(" + col + " + 0)"
		case tbl.name == "datetimes" && col == pk:
			return "(" + col + " + INTERVAL 0 SECOND)"
		case tbl.name == "datetimes":
			return "(" + col + " + INTERVAL 0 DAY)"
		case col == pk:
			return "(CONCAT(" + col + ") COLLATE utf8mb4_0900_ai_ci)"
		}
		return "CONCAT(" + col + ")"
	}
	type check struct{ indexed, scan string }
	var checks []check
	// Both sides order by the primary key; the reference orders by the wrapped
	// column so that it is a plain table scan plus sort, with no index code.
	// Each predicate is checked in both directions: DESC on a primary-key
	// predicate is answered by a reverse index scan.
	add := func(where string) {
		for _, dir := range []string{"", " DESC"} {
			checks = append(checks, check{
				indexed: fmt.Sprintf("SELECT * FROM %s WHERE %s ORDER BY %s%s", tbl.name, fmt.Sprintf(where, pk, idx), pk, dir),
				scan:    fmt.Sprintf("SELECT * FROM %s WHERE %s ORDER BY %s%s", tbl.name, fmt.Sprintf(where, scanExpr(pk), scanExpr(idx)), scanExpr(pk), dir),
			})
		}
	}
	for i := 0; i < 25; i++ {
		a, b := tbl.pkLits(r), tbl.pkLits(r)
		va, vb := tbl.idxLit(r), tbl.idxLit(r)
		add("%[1]s > " + a)
		add("%[1]s >= " + a)
		add("%[1]s < " + a)
		add("%[1]s <= " + a)
		add("%[1]s BETWEEN " + a + " AND " + b)
		add("%[1]s IN (" + a + ", " + b + ")")
		add("%[2]s > " + va)
		add("%[2]s BETWEEN " + va + " AND " + vb)
		add("%[2]s = " + va)
		add("%[2]s <= " + vb + " AND %[2]s >= " + va)
	}
	add("%[2]s IS NULL")
	add("%[2]s IS NOT NULL")
	for i, c := range checks {
		if i < 10 || i == len(checks)-1 {
			// The comparison is only meaningful if the two sides really take
			// different paths.
			if plan := explainPlan(t, eng, c.indexed); !strings.Contains(plan, "IndexedTableAccess") {
				t.Fatalf("%s does not use an index:\n%s", c.indexed, plan)
			}
			if plan := explainPlan(t, eng, c.scan); strings.Contains(plan, "IndexedTableAccess") {
				t.Fatalf("reference %s uses an index:\n%s", c.scan, plan)
			}
		}
		if got, want := query(c.indexed), query(c.scan); got != want {
			t.Fatalf("%s\n  index: %s\n  scan:  %s\n(reference: %s)", c.indexed, got, want, c.scan)
		}
	}
	// Ordered LIMITs, where the index order replaces the sort.
	for _, q := range []struct{ indexed, scan string }{
		{fmt.Sprintf("SELECT * FROM %s ORDER BY %s LIMIT 7", tbl.name, pk), fmt.Sprintf("SELECT * FROM %s ORDER BY %s LIMIT 7", tbl.name, scanExpr(pk))},
		func() struct{ indexed, scan string } {
			lit := tbl.pkLits(r)
			return struct{ indexed, scan string }{
				fmt.Sprintf("SELECT * FROM %s WHERE %s > %s ORDER BY %s LIMIT 4", tbl.name, pk, lit, pk),
				fmt.Sprintf("SELECT * FROM %s WHERE %s > %s ORDER BY %s LIMIT 4", tbl.name, scanExpr(pk), lit, scanExpr(pk)),
			}
		}(),
		{fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL ORDER BY %s, %s LIMIT 9", pk, tbl.name, idx, idx, pk), fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL ORDER BY %s, %s LIMIT 9", pk, tbl.name, scanExpr(idx), scanExpr(idx), scanExpr(pk))},
		// Descending forms, served by reverse scans.
		{fmt.Sprintf("SELECT * FROM %s ORDER BY %s DESC LIMIT 7", tbl.name, pk), fmt.Sprintf("SELECT * FROM %s ORDER BY %s DESC LIMIT 7", tbl.name, scanExpr(pk))},
		func() struct{ indexed, scan string } {
			lit := tbl.pkLits(r)
			return struct{ indexed, scan string }{
				fmt.Sprintf("SELECT * FROM %s WHERE %s < %s ORDER BY %s DESC LIMIT 4", tbl.name, pk, lit, pk),
				fmt.Sprintf("SELECT * FROM %s WHERE %s < %s ORDER BY %s DESC LIMIT 4", tbl.name, scanExpr(pk), lit, scanExpr(pk)),
			}
		}(),
		{fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL ORDER BY %s DESC, %s DESC LIMIT 9", pk, tbl.name, idx, idx, pk), fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL ORDER BY %s DESC, %s DESC LIMIT 9", pk, tbl.name, scanExpr(idx), scanExpr(idx), scanExpr(pk))},
		// NULL index values sort lowest, so they come last.
		{fmt.Sprintf("SELECT %s FROM %s ORDER BY %s DESC, %s DESC", pk, tbl.name, idx, pk), fmt.Sprintf("SELECT %s FROM %s ORDER BY %s DESC, %s DESC", pk, tbl.name, scanExpr(idx), scanExpr(pk))},
	} {
		if got, want := query(q.indexed), query(q.scan); got != want {
			t.Fatalf("%s\n  index: %s\n  scan:  %s", q.indexed, got, want)
		}
	}
}

// Bounds at the ends of the key space: an exclusive lower bound above the
// largest encodable key must select nothing, not everything.
func TestRangeQueriesAtIntegerExtremes(t *testing.T) {
	ctx := context.Background()
	root := gitRepository(t)
	if _, err := repository.Init(ctx, root); err != nil {
		t.Fatal(err)
	}
	eng, err := engine.OpenWithOptions(ctx, root, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	execAll(t, eng,
		"CREATE TABLE s (k BIGINT PRIMARY KEY, v BIGINT, KEY v_idx (v))",
		"INSERT INTO s VALUES (-9223372036854775808, -9223372036854775808), (-1, -1), (0, 0), (9223372036854775807, 9223372036854775807)",
		"CREATE TABLE u (k BIGINT UNSIGNED PRIMARY KEY, v BIGINT UNSIGNED, KEY v_idx (v))",
		"INSERT INTO u VALUES (0, 0), (1, 1), (18446744073709551615, 18446744073709551615)",
	)
	for _, tc := range []struct{ query, want string }{
		{"SELECT k FROM s WHERE k > 9223372036854775807", "[]"},
		{"SELECT k FROM s WHERE k >= 9223372036854775807", "[[9223372036854775807]]"},
		{"SELECT k FROM s WHERE k > 0 ORDER BY k", "[[9223372036854775807]]"},
		{"SELECT k FROM s WHERE k < -9223372036854775808", "[]"},
		{"SELECT k FROM s WHERE k <= -9223372036854775808", "[[-9223372036854775808]]"},
		{"SELECT k FROM s WHERE k BETWEEN -9223372036854775808 AND -1 ORDER BY k", "[[-9223372036854775808] [-1]]"},
		{"SELECT k FROM s WHERE v > 9223372036854775807", "[]"},
		{"SELECT k FROM s WHERE v >= 9223372036854775807", "[[9223372036854775807]]"},
		{"SELECT k FROM s WHERE v < -9223372036854775808", "[]"},
		{"SELECT k FROM u WHERE k > 18446744073709551615", "[]"},
		{"SELECT k FROM u WHERE k >= 18446744073709551615", "[[18446744073709551615]]"},
		{"SELECT k FROM u WHERE k > 1 ORDER BY k", "[[18446744073709551615]]"},
		{"SELECT k FROM u WHERE v > 18446744073709551615", "[]"},
		{"SELECT k FROM u WHERE v >= 18446744073709551615", "[[18446744073709551615]]"},
		{"SELECT k FROM u ORDER BY k LIMIT 1", "[[0]]"},
	} {
		if plan := explainPlan(t, eng, tc.query); !strings.Contains(plan, "IndexedTableAccess") && !strings.Contains(plan, "EmptyTable") {
			t.Errorf("%s does not use an index:\n%s", tc.query, plan)
		}
		if got := queryIDs(t, eng, tc.query); got != tc.want {
			t.Errorf("%s = %s, want %s", tc.query, got, tc.want)
		}
	}
}

// A plain EXPLAIN returns the plan tree: go-mysql-server's tabular EXPLAIN is
// a placeholder that cannot be sent over the MySQL protocol.
func TestPlainExplainReturnsPlan(t *testing.T) {
	_, eng := openCheckpointedIndexedTable(t)
	defer eng.Close()
	session, _ := eng.NewSession()
	defer session.Close()
	for _, q := range []string{"EXPLAIN SELECT id FROM t WHERE id > 1", "DESCRIBE SELECT id FROM t WHERE id > 1"} {
		result, err := session.Query(context.Background(), q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if len(result.Columns) != 1 || result.Columns[0] != "plan" || !strings.Contains(fmt.Sprint(result.Rows), "IndexedTableAccess(t)") {
			t.Fatalf("%s = %v %v, want plan rows", q, result.Columns, result.Rows)
		}
	}
	result, err := session.Query(context.Background(), "DESCRIBE t")
	if err != nil || len(result.Rows) != 3 {
		t.Fatalf("DESCRIBE t = %v, %v; want 3 column rows", result.Rows, err)
	}
}
