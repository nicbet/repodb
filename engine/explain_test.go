package engine

import "testing"

func TestNormalizeExplain(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"EXPLAIN SELECT 1", "EXPLAIN PLAN SELECT 1"},
		{"explain select 1", "EXPLAIN PLAN select 1"},
		{"DESCRIBE SELECT * FROM t", "EXPLAIN PLAN SELECT * FROM t"},
		{"desc SELECT * FROM t", "EXPLAIN PLAN SELECT * FROM t"},
		{"  /* note */ EXPLAIN UPDATE t SET a = 1", "  /* note */ EXPLAIN PLAN UPDATE t SET a = 1"},
		{"-- note\nEXPLAIN SELECT 1", "-- note\nEXPLAIN PLAN SELECT 1"},
		{"EXPLAIN SELECT 1; SELECT 2", "EXPLAIN PLAN SELECT 1; SELECT 2"},
		// Unchanged.
		{"DESCRIBE t", "DESCRIBE t"},
		{"DESC t", "DESC t"},
		{"EXPLAIN t", "EXPLAIN t"},
		{"EXPLAIN PLAN SELECT 1", "EXPLAIN PLAN SELECT 1"},
		{"EXPLAIN FORMAT=TREE SELECT 1", "EXPLAIN FORMAT=TREE SELECT 1"},
		{"EXPLAIN ANALYZE SELECT 1", "EXPLAIN ANALYZE SELECT 1"},
		{"SELECT 'EXPLAIN SELECT 1'", "SELECT 'EXPLAIN SELECT 1'"},
		{"EXPLAIN SELEC garbage", "EXPLAIN SELEC garbage"},
		{"", ""},
		{"/* unterminated", "/* unterminated"},
	} {
		if got := NormalizeExplain(tc.in); got != tc.want {
			t.Errorf("NormalizeExplain(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
