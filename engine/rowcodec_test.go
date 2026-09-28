package engine

import (
	"bytes"
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/apd/v3"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
	querypb "github.com/dolthub/vitess/go/vt/proto/query"
)

type rowCodecCase struct {
	name   string
	typ    sql.Type
	values []any // raw inputs, converted to typ before encoding
}

func rowCodecCases() []rowCodecCase {
	collated, err := types.CreateString(querypb.Type_VARCHAR, 64, sql.Collation_utf8mb4_0900_ai_ci)
	if err != nil {
		panic(err)
	}
	enum, err := types.CreateEnumType([]string{"red", "green", "blue"}, sql.Collation_Default)
	if err != nil {
		panic(err)
	}
	return []rowCodecCase{
		{"int8", types.Int8, []any{int64(math.MinInt8), int64(-1), int64(0), int64(math.MaxInt8)}},
		{"int16", types.Int16, []any{int64(math.MinInt16), int64(math.MaxInt16)}},
		{"int24", types.Int24, []any{int64(-1 << 23), int64(1<<23 - 1)}},
		{"int32", types.Int32, []any{int64(math.MinInt32), int64(math.MaxInt32)}},
		{"int64", types.Int64, []any{int64(math.MinInt64), int64(-64), int64(63), int64(math.MaxInt64)}},
		{"uint8", types.Uint8, []any{uint64(0), uint64(math.MaxUint8)}},
		{"uint16", types.Uint16, []any{uint64(math.MaxUint16)}},
		{"uint24", types.Uint24, []any{uint64(1<<24 - 1)}},
		{"uint32", types.Uint32, []any{uint64(math.MaxUint32)}},
		{"uint64", types.Uint64, []any{uint64(0), uint64(math.MaxUint64)}},
		{"float32", types.Float32, []any{float32(0), float32(math.Copysign(0, -1)), float32(-1.5), float32(math.MaxFloat32), float32(math.SmallestNonzeroFloat32), float32(math.Inf(1)), float32(math.Inf(-1))}},
		{"float64", types.Float64, []any{0.0, math.Copysign(0, -1), 3.25, -math.MaxFloat64, math.SmallestNonzeroFloat64, math.Inf(1), math.Inf(-1)}},
		{"decimal(10,2)", types.MustCreateColumnDecimalType(10, 2), []any{"0", "-0.00", "1.5", "1234.50", "-99999999.99", "99999999.99"}},
		{"decimal(18,0)", types.MustCreateColumnDecimalType(18, 0), []any{"999999999999999999", "-999999999999999999"}},
		{"decimal(19,0)", types.MustCreateColumnDecimalType(19, 0), []any{"9223372036854775807", "-9223372036854775808", "9999999999999999999", "-9999999999999999999"}},
		{"decimal(65,30)", types.MustCreateColumnDecimalType(65, 30), []any{"0", strings.Repeat("9", 35) + "." + strings.Repeat("9", 30), "-" + strings.Repeat("9", 35) + "." + strings.Repeat("9", 30), "0." + strings.Repeat("0", 29) + "1"}},
		{"date", types.Date, []any{"1000-01-01", "1969-12-31", "1970-01-01", "9999-12-31"}},
		{"datetime(6)", types.MustCreateDatetimeType(querypb.Type_DATETIME, 6), []any{"1000-01-01 00:00:00", "1969-12-31 23:59:59.999999", "2026-09-28 12:34:56.789012", "9999-12-31 23:59:59.999999"}},
		{"datetime(0)", types.MustCreateDatetimeType(querypb.Type_DATETIME, 0), []any{"2026-09-28 12:34:56"}},
		{"timestamp(3)", types.MustCreateDatetimeType(querypb.Type_TIMESTAMP, 3), []any{"1970-01-01 00:00:01.000", "2038-01-19 03:14:07.999"}},
		{"time", types.Time, []any{"-838:59:59", "00:00:00", "838:59:59", "-00:00:00.000001"}},
		{"enum", enum, []any{"red", "blue"}},
		{"varchar", types.MustCreateStringWithDefaults(querypb.Type_VARCHAR, 64), []any{"", "hello", "héllo wörld 🎉", "a\x00b"}},
		{"varchar ci", collated, []any{"MiXeD Case"}},
		{"char", types.MustCreateStringWithDefaults(querypb.Type_CHAR, 8), []any{"", "ab"}},
		{"text", types.Text, []any{strings.Repeat("x", 60_000)}},
		{"blob", types.Blob, []any{[]byte{}, []byte{0, 1, 0xFF, 0}}},
		{"varbinary", types.MustCreateBinary(querypb.Type_VARBINARY, 16), []any{[]byte("bin")}},
		{"json", types.JSON, []any{`{"b": [1, 2, {"c": null}], "a": "x"}`, `[]`, `"just a string"`, `3.5`}},
	}
}

func mustConvert(t testing.TB, typ sql.Type, v any) any {
	t.Helper()
	out, _, err := typ.Convert(context.Background(), v)
	if err != nil {
		t.Fatalf("convert %v to %s: %v", v, typ, err)
	}
	return out
}

// sameCell reports whether a decoded value is the value that was encoded.
// Decimals compare by value and exponent: apd's internal big-integer
// representation, and the sign of zero, may differ between equal values.
func sameCell(want, got any) bool {
	if w, ok := want.(*apd.Decimal); ok {
		g, ok := got.(*apd.Decimal)
		return ok && w.Cmp(g) == 0 && w.Exponent == g.Exponent
	}
	if w, ok := want.(sql.JSONWrapper); ok {
		g, ok := got.(sql.JSONWrapper)
		if !ok {
			return false
		}
		ws, _ := types.JsonToMySqlString(context.Background(), w)
		gs, _ := types.JsonToMySqlString(context.Background(), g)
		return ws == gs
	}
	if w, ok := want.(float64); ok {
		g, ok := got.(float64)
		return ok && math.Float64bits(w) == math.Float64bits(g)
	}
	if w, ok := want.(float32); ok {
		g, ok := got.(float32)
		return ok && math.Float32bits(w) == math.Float32bits(g)
	}
	return reflect.DeepEqual(want, got)
}

func TestRowCodecRoundTripsEveryType(t *testing.T) {
	for _, tc := range rowCodecCases() {
		t.Run(tc.name, func(t *testing.T) {
			schema := sql.Schema{{Name: "v", Type: tc.typ, Nullable: true}}
			for _, raw := range append(tc.values, nil) {
				var want any
				if raw != nil {
					want = mustConvert(t, tc.typ, raw)
				}
				data, err := encodeRow(schema, sql.Row{want})
				if err != nil {
					t.Fatalf("encode %v: %v", raw, err)
				}
				got, err := decodeRow(schema, data)
				if err != nil {
					t.Fatalf("decode %v: %v", raw, err)
				}
				if !sameCell(want, got[0]) {
					t.Fatalf("%v: decoded %#v (%T), want %#v (%T)", raw, got[0], got[0], want, want)
				}
				if want != nil && reflect.TypeOf(got[0]) != reflect.TypeOf(want) {
					t.Fatalf("%v: decoded type %T, want %T", raw, got[0], want)
				}
				// A decoded value must be what the column type itself
				// produces, so converting it again is a no-op.
				if got[0] != nil && !sameCell(got[0], mustConvert(t, tc.typ, got[0])) {
					t.Fatalf("%v: decoded %#v is not a converted %s value", raw, got[0], tc.typ)
				}
				again, err := encodeRow(schema, got)
				if err != nil || !bytes.Equal(again, data) {
					t.Fatalf("%v: re-encoding changed the bytes: %x -> %x (%v)", raw, data, again, err)
				}
			}
		})
	}
}

// wideRow builds one row with a column per codec case, NULLs interleaved, so
// the NULL bitmap spans several bytes.
func wideRow(t testing.TB) (sql.Schema, sql.Row) {
	t.Helper()
	var schema sql.Schema
	var row sql.Row
	for i, tc := range rowCodecCases() {
		schema = append(schema, &sql.Column{Name: tc.name, Type: tc.typ, Nullable: true})
		if i%3 == 1 {
			row = append(row, nil)
			continue
		}
		row = append(row, mustConvert(t, tc.typ, tc.values[len(tc.values)-1]))
	}
	return schema, row
}

func TestRowCodecWideRow(t *testing.T) {
	schema, row := wideRow(t)
	if len(schema) <= 16 {
		t.Fatalf("wide row has %d columns; want a bitmap over 2 bytes", len(schema))
	}
	data, err := encodeRow(schema, row)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeRow(schema, data)
	if err != nil {
		t.Fatal(err)
	}
	for i := range row {
		if !sameCell(row[i], got[i]) {
			t.Fatalf("column %s: decoded %#v, want %#v", schema[i].Name, got[i], row[i])
		}
	}
}

// Merge compares row blobs byte for byte, so equal values must encode equally.
func TestRowCodecIsCanonical(t *testing.T) {
	dec := types.MustCreateColumnDecimalType(10, 2)
	schema := sql.Schema{{Name: "d", Type: dec}}
	encode := func(d *apd.Decimal) []byte {
		t.Helper()
		data, err := encodeRow(schema, sql.Row{d})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	// 1.5 with exponent -1, as an unconverted value would arrive, encodes
	// like the converted 1.50.
	if a, b := encode(apd.New(15, -1)), encode(mustConvert(t, dec, "1.50").(*apd.Decimal)); !bytes.Equal(a, b) {
		t.Fatalf("1.5 = %x, 1.50 = %x", a, b)
	}
	negZero := mustConvert(t, dec, "0").(*apd.Decimal)
	negZero.Negative = true
	if a, b := encode(negZero), encode(mustConvert(t, dec, "0").(*apd.Decimal)); !bytes.Equal(a, b) {
		t.Fatalf("-0 = %x, 0 = %x", a, b)
	}
	// Timestamps in different locations are the same instant.
	dt := types.MustCreateDatetimeType(querypb.Type_DATETIME, 6)
	at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	dtSchema := sql.Schema{{Name: "t", Type: dt}}
	a, _ := encodeRow(dtSchema, sql.Row{at})
	b, _ := encodeRow(dtSchema, sql.Row{at.In(time.FixedZone("x", 3600))})
	if !bytes.Equal(a, b) {
		t.Fatalf("same instant encodes as %x and %x", a, b)
	}
}

func TestRowCodecCompactIntegers(t *testing.T) {
	schema := sql.Schema{{Name: "id", Type: types.Int64}, {Name: "n", Type: types.Uint64}}
	data, err := encodeRow(schema, sql.Row{int64(42), uint64(7)})
	if err != nil {
		t.Fatal(err)
	}
	// 1 byte count + 1 byte bitmap + 1 byte per small integer.
	if len(data) != 4 {
		t.Fatalf("encoded %d bytes (%x), want 4", len(data), data)
	}
}

func TestRowDecodeRejectsMalformedRows(t *testing.T) {
	schema, row := wideRow(t)
	data, err := encodeRow(schema, row)
	if err != nil {
		t.Fatal(err)
	}
	for n := range len(data) {
		if _, err := decodeRow(schema, data[:n]); err == nil {
			t.Fatalf("decoding the first %d of %d bytes succeeded", n, len(data))
		}
	}
	if _, err := decodeRow(schema, append(append([]byte(nil), data...), 0)); err == nil {
		t.Fatal("decoding a row with a trailing byte succeeded")
	}
	if _, err := decodeRow(schema[:len(schema)-1], data); err == nil {
		t.Fatal("decoding with a narrower schema succeeded")
	}
	decSchema := sql.Schema{{Name: "d", Type: types.MustCreateColumnDecimalType(10, 2)}}
	if _, err := decodeRow(decSchema, []byte{1, 0, 7, 0}); err == nil {
		t.Fatal("decoding an unknown decimal form succeeded")
	}
}

func FuzzDecodeRow(f *testing.F) {
	schema, row := wideRow(f)
	data, err := encodeRow(schema, row)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	f.Add([]byte{})
	f.Add([]byte{0xFF, 0xFF, 0xFF})
	f.Fuzz(func(t *testing.T, data []byte) {
		decoded, err := decodeRow(schema, data)
		if err != nil {
			return
		}
		// Whatever decodes must encode and decode to the same values.
		again, err := encodeRow(schema, decoded)
		if err != nil {
			return // e.g. an enum index or DECIMAL beyond what the column allows
		}
		redecoded, err := decodeRow(schema, again)
		if err != nil {
			t.Fatalf("re-encoded row does not decode: %v", err)
		}
		for i := range decoded {
			if !sameCell(decoded[i], redecoded[i]) {
				t.Fatalf("column %s: %#v became %#v", schema[i].Name, decoded[i], redecoded[i])
			}
		}
	})
}
