package engine

import (
	"context"
	"strings"

	"github.com/dolthub/vitess/go/vt/sqlparser"
)

// NormalizeExplain rewrites a plain EXPLAIN, DESCRIBE or DESC of a statement
// to EXPLAIN PLAN, and returns every other query unchanged.
//
// go-mysql-server's tabular EXPLAIN is a placeholder: it returns one dummy row
// holding the string "NULL" in every column, including the unsigned "rows"
// column, which cannot be encoded for the MySQL protocol and fails with
// `strconv.ParseUint: parsing "NULL"`. The plan tree is what a user asks for.
// DESCRIBE <table>, EXPLAIN PLAN, EXPLAIN FORMAT=… and EXPLAIN ANALYZE are
// left alone. Only the leading keyword changes, so a multi-statement query
// keeps its remainder.
func NormalizeExplain(query string) string {
	start, end := leadingKeyword(query)
	switch strings.ToUpper(query[start:end]) {
	case "EXPLAIN", "DESCRIBE", "DESC":
	default:
		return query // the common case: no parse
	}
	stmt, _, err := sqlparser.ParseOne(context.Background(), query)
	if err != nil {
		return query
	}
	explain, ok := stmt.(*sqlparser.Explain)
	if !ok || explain.Plan || explain.Analyze || explain.ExplainFormat != "" {
		return query
	}
	rewritten := query[:start] + "EXPLAIN PLAN" + query[end:]
	if stmt, _, err := sqlparser.ParseOne(context.Background(), rewritten); err != nil {
		return query
	} else if explain, ok := stmt.(*sqlparser.Explain); !ok || !explain.Plan {
		return query
	}
	return rewritten
}

// leadingKeyword returns the byte range of the first word after whitespace
// and SQL comments.
func leadingKeyword(query string) (int, int) {
	i := 0
	for i < len(query) {
		switch {
		case query[i] == ' ' || query[i] == '\t' || query[i] == '\n' || query[i] == '\r':
			i++
		case strings.HasPrefix(query[i:], "/*"):
			end := strings.Index(query[i+2:], "*/")
			if end < 0 {
				return len(query), len(query)
			}
			i += 2 + end + 2
		case strings.HasPrefix(query[i:], "--") || query[i] == '#':
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				return len(query), len(query)
			}
			i += end + 1
		default:
			j := i
			for j < len(query) && (query[j] >= 'a' && query[j] <= 'z' || query[j] >= 'A' && query[j] <= 'Z') {
				j++
			}
			return i, j
		}
	}
	return i, i
}
