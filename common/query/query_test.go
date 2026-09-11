package query_test

import (
	"testing"

	"github.com/nicbet/repodb/common/query"
)

func TestCanonicalUsesMySQLGrammar(t *testing.T) {
	got, err := query.Canonical("select `name` from users where id=1")
	if err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Fatal("canonical query is empty")
	}
}
