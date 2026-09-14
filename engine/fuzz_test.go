package engine

import (
	"bytes"
	"math"
	"testing"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
	querypb "github.com/dolthub/vitess/go/vt/proto/query"
)

func FuzzSqlLiteral(f *testing.F) {
	f.Add("")
	f.Add("hello")
	f.Add("it's a test")
	f.Add(`back\slash`)
	f.Add(`C:\Users\test`)
	f.Add(`{"key":"value","path":"C:\\foo"}`)
	f.Add("null\x00byte")
	f.Add("new\nline\ttab")
	f.Add("émojis 🎉 and ñ")
	f.Add("double''quote")
	f.Add(`mix\'of\''escapes`)
	f.Add("a\x00b\x00c")

	f.Fuzz(func(t *testing.T, input string) {
		lit, err := sqlLiteral(input)
		if err != nil {
			t.Fatal(err)
		}
		statement := "SELECT " + lit
		bound, err := bind(statement, nil)
		if err != nil {
			t.Fatal(err)
		}
		if bound != statement {
			t.Fatalf("bind mutated literal-only statement: got %q, want %q", bound, statement)
		}
	})
}

func FuzzBindNoPanic(f *testing.F) {
	f.Add("SELECT ?", "hello")
	f.Add("SELECT '?' FROM t WHERE x = ?", "val")
	f.Add(`SELECT "?" FROM t WHERE x = ?`, "val")
	f.Add("SELECT ?, ?", "a")
	f.Add("INSERT INTO t VALUES (?)", `it's`)
	f.Add(`SELECT '\?' FROM t`, "")
	f.Add("SELECT ?", `back\slash`)

	f.Fuzz(func(t *testing.T, statement, arg string) {
		// Must never panic regardless of input
		_, _ = bind(statement, []any{arg})
	})
}

func FuzzRowRoundTripInt64(f *testing.F) {
	f.Add(int64(0))
	f.Add(int64(1))
	f.Add(int64(-1))
	f.Add(int64(math.MaxInt64))
	f.Add(int64(math.MinInt64))
	f.Add(int64(42))

	schema := sql.Schema{
		{Name: "v", Type: types.Int64, Nullable: false},
	}

	f.Fuzz(func(t *testing.T, v int64) {
		row := sql.Row{v}
		data, err := encodeRow(schema, row)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeRow(schema, data)
		if err != nil {
			t.Fatal(err)
		}
		if decoded[0] != v {
			t.Fatalf("round-trip failed: got %v, want %v", decoded[0], v)
		}
	})
}

func FuzzRowRoundTripUint64(f *testing.F) {
	f.Add(uint64(0))
	f.Add(uint64(1))
	f.Add(uint64(math.MaxUint64))

	schema := sql.Schema{
		{Name: "v", Type: types.Uint64, Nullable: false},
	}

	f.Fuzz(func(t *testing.T, v uint64) {
		row := sql.Row{v}
		data, err := encodeRow(schema, row)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeRow(schema, data)
		if err != nil {
			t.Fatal(err)
		}
		if decoded[0] != v {
			t.Fatalf("round-trip failed: got %v, want %v", decoded[0], v)
		}
	})
}

func FuzzRowRoundTripFloat64(f *testing.F) {
	f.Add(float64(0))
	f.Add(float64(1.5))
	f.Add(float64(-1.5))
	f.Add(math.MaxFloat64)
	f.Add(math.SmallestNonzeroFloat64)

	schema := sql.Schema{
		{Name: "v", Type: types.Float64, Nullable: false},
	}

	f.Fuzz(func(t *testing.T, v float64) {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return
		}
		row := sql.Row{v}
		data, err := encodeRow(schema, row)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeRow(schema, data)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := decoded[0].(float64)
		if !ok {
			t.Fatalf("decoded type %T, want float64", decoded[0])
		}
		if got != v {
			t.Fatalf("round-trip failed: got %v, want %v", got, v)
		}
	})
}

func FuzzRowRoundTripString(f *testing.F) {
	f.Add("")
	f.Add("hello")
	f.Add("back\\slash")
	f.Add("null\x00byte")
	f.Add(`{"json":"value"}`)
	f.Add("émojis 🎉")

	varcharType, _ := types.CreateString(querypb.Type_VARCHAR, 65535, sql.Collation_Default)
	schema := sql.Schema{
		{Name: "v", Type: varcharType, Nullable: false},
	}

	f.Fuzz(func(t *testing.T, v string) {
		row := sql.Row{v}
		data, err := encodeRow(schema, row)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeRow(schema, data)
		if err != nil {
			t.Fatal(err)
		}
		if decoded[0] != v {
			t.Fatalf("round-trip failed: got %q, want %q", decoded[0], v)
		}
	})
}

func FuzzRowRoundTripBlob(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 1, 2, 255})
	f.Add([]byte("hello"))
	f.Add([]byte{0, 0, 0})

	blobType, _ := types.CreateBinary(querypb.Type_BLOB, 65535)
	schema := sql.Schema{
		{Name: "v", Type: blobType, Nullable: false},
	}

	f.Fuzz(func(t *testing.T, v []byte) {
		row := sql.Row{v}
		data, err := encodeRow(schema, row)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeRow(schema, data)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := decoded[0].([]byte)
		if !ok {
			t.Fatalf("decoded type %T, want []byte", decoded[0])
		}
		if !bytes.Equal(got, v) {
			t.Fatalf("round-trip failed: got %x, want %x", got, v)
		}
	})
}

func FuzzSchemaRoundTrip(f *testing.F) {
	f.Add("col1", true, "check_expr", "chk_name")
	f.Add("", false, "", "")
	f.Add("café", true, "(x > 0)", "pos_check")
	f.Add("col_with_default", false, "", "")

	f.Fuzz(func(t *testing.T, colName string, nullable bool, checkExpr, checkName string) {
		if colName == "" {
			colName = "c"
		}
		schema := sql.PrimaryKeySchema{
			Schema: sql.Schema{
				{Name: colName, Type: types.Int64, Nullable: nullable, PrimaryKey: true},
			},
			PkOrdinals: []int{0},
		}
		var checks []sql.CheckDefinition
		if checkExpr != "" {
			checks = []sql.CheckDefinition{
				{Name: checkName, CheckExpression: checkExpr, Enforced: true},
			}
		}
		data, err := encodeSchema(schema, checks)
		if err != nil {
			t.Fatal(err)
		}
		decoded, decodedChecks, err := decodeSchema(data)
		if err != nil {
			t.Fatal(err)
		}
		if len(decoded.Schema) != 1 {
			t.Fatalf("decoded %d columns, want 1", len(decoded.Schema))
		}
		if decoded.Schema[0].Name != colName {
			t.Fatalf("column name: got %q, want %q", decoded.Schema[0].Name, colName)
		}
		if decoded.Schema[0].Nullable != nullable {
			t.Fatalf("nullable: got %v, want %v", decoded.Schema[0].Nullable, nullable)
		}
		if len(decoded.PkOrdinals) != 1 || decoded.PkOrdinals[0] != 0 {
			t.Fatalf("pk ordinals: got %v, want [0]", decoded.PkOrdinals)
		}
		if checkExpr != "" {
			if len(decodedChecks) != 1 {
				t.Fatalf("decoded %d checks, want 1", len(decodedChecks))
			}
			if decodedChecks[0].CheckExpression != checkExpr {
				t.Fatalf("check expr: got %q, want %q", decodedChecks[0].CheckExpression, checkExpr)
			}
			if decodedChecks[0].Name != checkName {
				t.Fatalf("check name: got %q, want %q", decodedChecks[0].Name, checkName)
			}
		} else if len(decodedChecks) != 0 {
			t.Fatalf("decoded %d checks, want 0", len(decodedChecks))
		}
	})
}

func FuzzEncodeKey(f *testing.F) {
	f.Add(int64(0))
	f.Add(int64(1))
	f.Add(int64(-1))
	f.Add(int64(math.MaxInt64))
	f.Add(int64(math.MinInt64))

	schema := sql.PrimaryKeySchema{
		Schema: sql.Schema{
			{Name: "id", Type: types.Int64, Nullable: false, PrimaryKey: true},
		},
		PkOrdinals: []int{0},
	}

	f.Fuzz(func(t *testing.T, v int64) {
		row := sql.Row{v}
		key1, err := encodeKey(schema, row)
		if err != nil {
			t.Fatal(err)
		}
		key2, err := encodeKey(schema, row)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(key1, key2) {
			t.Fatalf("non-deterministic key: %x != %x", key1, key2)
		}
	})
}

func FuzzEncodeKeyString(f *testing.F) {
	f.Add("")
	f.Add("hello")
	f.Add("back\\slash")
	f.Add("null\x00byte")

	varcharType, _ := types.CreateString(querypb.Type_VARCHAR, 65535, sql.Collation_Default)
	schema := sql.PrimaryKeySchema{
		Schema: sql.Schema{
			{Name: "id", Type: varcharType, Nullable: false, PrimaryKey: true},
		},
		PkOrdinals: []int{0},
	}

	f.Fuzz(func(t *testing.T, v string) {
		row := sql.Row{v}
		key1, err := encodeKey(schema, row)
		if err != nil {
			t.Fatal(err)
		}
		key2, err := encodeKey(schema, row)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(key1, key2) {
			t.Fatalf("non-deterministic key: %x != %x", key1, key2)
		}
	})
}

func FuzzEncodeKeyDistinct(f *testing.F) {
	f.Add(int64(1), int64(2))
	f.Add(int64(0), int64(-1))
	f.Add(int64(math.MaxInt64), int64(math.MinInt64))

	schema := sql.PrimaryKeySchema{
		Schema: sql.Schema{
			{Name: "id", Type: types.Int64, Nullable: false, PrimaryKey: true},
		},
		PkOrdinals: []int{0},
	}

	f.Fuzz(func(t *testing.T, a, b int64) {
		if a == b {
			return
		}
		keyA, err := encodeKey(schema, sql.Row{a})
		if err != nil {
			t.Fatal(err)
		}
		keyB, err := encodeKey(schema, sql.Row{b})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(keyA, keyB) {
			t.Fatalf("distinct values %d and %d produced same key: %x", a, b, keyA)
		}
	})
}
