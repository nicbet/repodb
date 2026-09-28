package engine

import (
	"context"
	"testing"
	"time"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
	querypb "github.com/dolthub/vitess/go/vt/proto/query"
)

// benchmarkRow returns a mixed-type schema and a row of values already
// converted to their column types, as rows are when they reach encodeRow.
func benchmarkRow(tb testing.TB) (sql.Schema, sql.Row) {
	tb.Helper()
	dec := types.MustCreateColumnDecimalType(10, 2)
	datetime := types.MustCreateDatetimeType(querypb.Type_DATETIME, 6)
	varchar := types.MustCreateStringWithDefaults(querypb.Type_VARCHAR, 64)
	schema := sql.Schema{
		{Name: "id", Type: types.Int64},
		{Name: "qty", Type: types.Int32},
		{Name: "name", Type: varchar},
		{Name: "score", Type: types.Float64},
		{Name: "price", Type: dec},
		{Name: "created", Type: datetime},
		{Name: "note", Type: types.Text, Nullable: true},
		{Name: "payload", Type: types.Blob},
	}
	raw := []any{int64(123456), int32(42), "widget-0042", 3.25, "1234.50", time.Date(2026, 9, 28, 12, 34, 56, 789000000, time.UTC), nil, []byte("0123456789abcdef")}
	row := make(sql.Row, len(schema))
	for i, v := range raw {
		if v == nil {
			continue
		}
		converted, _, err := schema[i].Type.Convert(context.Background(), v)
		if err != nil {
			tb.Fatal(err)
		}
		row[i] = converted
	}
	return schema, row
}

func BenchmarkRowCodec(b *testing.B) {
	schema, row := benchmarkRow(b)
	data, err := encodeRow(schema, row)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("encode", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := encodeRow(schema, row); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("decode", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		for range b.N {
			if _, err := decodeRow(schema, data); err != nil {
				b.Fatal(err)
			}
		}
	})
}
