// Package query exposes the Vitess MySQL parser used by the server. Keeping
// parsing behind this package prevents parser-specific types leaking into the
// storage and Git layers.
package query

import "github.com/dolthub/vitess/go/vt/sqlparser"

func Parse(statement string) (sqlparser.Statement, error) {
	return sqlparser.Parse(statement)
}

func Canonical(statement string) (string, error) {
	parsed, err := Parse(statement)
	if err != nil {
		return "", err
	}
	return sqlparser.String(parsed), nil
}
