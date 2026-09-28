package engine

import (
	"bytes"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/apd/v3"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
	querypb "github.com/dolthub/vitess/go/vt/proto/query"
)

type keyCodecCase struct {
	name string
	typ  sql.Type
	gen  func(r *rand.Rand) any
	edge []any
}

func keyCodecCases(t *testing.T) []keyCodecCase {
	t.Helper()
	dec := types.MustCreateColumnDecimalType(30, 6)
	ci, err := types.CreateString(querypb.Type_VARCHAR, 100, sql.Collation_utf8mb4_0900_ai_ci)
	if err != nil {
		t.Fatal(err)
	}
	enum, err := types.CreateEnumType([]string{"zeta", "alpha", "mid"}, sql.Collation_Default)
	if err != nil {
		t.Fatal(err)
	}
	datetime := types.MustCreateDatetimeType(querypb.Type_DATETIME, 6)
	words := []string{"", "a", "aa", "ab", "b", "a\x00", "a\x00b", "\x00", "Zebra", "zebra", "ä", "Ä", "abc", "ab\x01", "日本"}
	return []keyCodecCase{
		{"int64", types.Int64, func(r *rand.Rand) any { return int64(r.Uint64()) >> r.IntN(64) },
			[]any{int64(math.MinInt64), int64(-10), int64(-9), int64(-1), int64(0), int64(1), int64(9), int64(10), int64(math.MaxInt64)}},
		{"int8", types.Int8, func(r *rand.Rand) any { return int8(r.IntN(256) - 128) },
			[]any{int8(-128), int8(-1), int8(0), int8(127)}},
		{"uint64", types.Uint64, func(r *rand.Rand) any { return r.Uint64() >> r.IntN(64) },
			[]any{uint64(0), uint64(9), uint64(10), uint64(math.MaxUint64)}},
		{"float64", types.Float64, func(r *rand.Rand) any { return (r.Float64() - 0.5) * math.Pow(10, float64(r.IntN(20)-10)) },
			[]any{math.Inf(-1), -math.MaxFloat64, -1.5, -1.0, -math.SmallestNonzeroFloat64, math.Copysign(0, -1), 0.0, math.SmallestNonzeroFloat64, 0.1, 1.0, 10.0, math.MaxFloat64, math.Inf(1)}},
		{"float32", types.Float32, func(r *rand.Rand) any { return float32((r.Float64() - 0.5) * 1000) },
			[]any{float32(-1.5), float32(0), float32(0.25), float32(3.5)}},
		{"decimal", dec, func(r *rand.Rand) any {
			d, _, _ := apd.NewFromString(fmt.Sprintf("%d.%06d", r.IntN(2_000_000)-1_000_000, r.IntN(1_000_000)))
			return d
		}, []any{
			apd.New(0, 0), apd.New(-10, 0), apd.New(-9, 0), apd.New(-1, 0), apd.New(-100, -2),
			apd.New(1, -6), apd.New(1, 0), apd.New(10, -1), apd.New(100, -2), apd.New(9, 0), apd.New(10, 0),
			apd.New(123456, -3), apd.New(1234560, -4), apd.New(-123456, -3), apd.New(-1234561, -4),
		}},
		{"datetime", datetime, func(r *rand.Rand) any {
			return time.UnixMicro(r.Int64N(2*300*365*86400*1_000_000) - 300*365*86400*1_000_000).UTC()
		}, []any{
			time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1969, 12, 31, 23, 59, 59, 999999000, time.UTC),
			time.Unix(0, 0).UTC(), time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
		}},
		{"time", types.Time, func(r *rand.Rand) any { return types.Timespan(r.Int64N(2*838*3600*1_000_000) - 838*3600*1_000_000) },
			[]any{types.Timespan(-1), types.Timespan(0), types.Timespan(1)}},
		{"enum", enum, func(r *rand.Rand) any { return uint16(r.IntN(3) + 1) }, []any{uint16(1), uint16(2), uint16(3)}},
		{"varchar-bin", types.LongText, func(r *rand.Rand) any {
			return words[r.IntN(len(words))] + words[r.IntN(len(words))]
		}, stringsAsAny(words)},
		{"varchar-ci", ci, func(r *rand.Rand) any {
			return words[r.IntN(len(words))] + strings.ToUpper(words[r.IntN(len(words))])
		}, stringsAsAny(words)},
		{"char", types.MustCreateString(querypb.Type_CHAR, 10, sql.Collation_Default), func(r *rand.Rand) any {
			return words[r.IntN(len(words))] + strings.Repeat(" ", r.IntN(3))
		}, []any{"", " ", "a", "a ", "a  ", "a b", "b"}},
		{"varbinary", types.LongBlob, func(r *rand.Rand) any {
			b := make([]byte, r.IntN(5))
			for i := range b {
				b[i] = byte(r.IntN(3)) * 0x7F // 0x00, 0x7F, 0xFE
			}
			return b
		}, []any{[]byte{}, []byte{0}, []byte{0, 0}, []byte{0, 1}, []byte{1}, []byte{0xFF}}},
	}
}

func stringsAsAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

func encodeColumn(t *testing.T, typ sql.Type, v any) []byte {
	t.Helper()
	b, err := appendKeyColumn(nil, typ, v)
	if err != nil {
		t.Fatalf("encode %v (%T): %v", v, v, err)
	}
	return b
}

// For every supported key type, byte order of the encoding must equal SQL
// order of the values, and equal values must encode identically.
func TestKeyCodecPreservesOrder(t *testing.T) {
	ctx := sql.NewEmptyContext()
	r := rand.New(rand.NewPCG(7, 11))
	for _, tc := range keyCodecCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			values := append([]any(nil), tc.edge...)
			for i := 0; i < 300; i++ {
				values = append(values, tc.gen(r))
			}
			for i, a := range values {
				ea := encodeColumn(t, tc.typ, a)
				for _, b := range values[i:] {
					eb := encodeColumn(t, tc.typ, b)
					want, err := tc.typ.Compare(ctx, a, b)
					if err != nil {
						t.Fatalf("compare %v, %v: %v", a, b, err)
					}
					if got := bytes.Compare(ea, eb); sign(got) != sign(want) {
						t.Fatalf("order mismatch for %#v vs %#v: SQL %d, bytes %d (%x vs %x)", a, b, want, got, ea, eb)
					}
				}
			}
		})
	}
}

// Composite keys: each column is self-delimiting, so the concatenation orders
// lexicographically by column even when a string column is a prefix of
// another.
func TestKeyCodecCompositeOrder(t *testing.T) {
	ctx := sql.NewEmptyContext()
	type pair struct {
		s string
		n int64
	}
	var values []pair
	for _, s := range []string{"", "a", "a\x00", "ab", "b"} {
		for _, n := range []int64{-5, 0, 7} {
			values = append(values, pair{s, n})
		}
	}
	encode := func(p pair) []byte {
		b := encodeColumn(t, types.LongText, p.s)
		return append(b, encodeColumn(t, types.Int64, p.n)...)
	}
	for _, a := range values {
		for _, b := range values {
			want, _ := types.LongText.Compare(ctx, a.s, b.s)
			if want == 0 {
				want, _ = types.Int64.Compare(ctx, a.n, b.n)
			}
			if got := bytes.Compare(encode(a), encode(b)); sign(got) != sign(want) {
				t.Fatalf("composite order mismatch for %+v vs %+v: SQL %d, bytes %d", a, b, want, got)
			}
		}
	}
}

func TestKeyCodecRejectsNonKeyValues(t *testing.T) {
	for _, tc := range []struct {
		typ   sql.Type
		value any
	}{
		{types.Float64, math.NaN()},
		{types.JSON, types.JSONDocument{Val: 1}},
		{types.Int64, "7"},
	} {
		if _, err := appendKeyColumn(nil, tc.typ, tc.value); err == nil {
			t.Errorf("appendKeyColumn(%s, %#v) succeeded, want error", tc.typ, tc.value)
		}
	}
}

func TestKeyPrefixEnd(t *testing.T) {
	for _, tc := range []struct{ in, want []byte }{
		{[]byte{0x01}, []byte{0x02}},
		{[]byte{0x01, 0xFF}, []byte{0x02}},
		{[]byte{0xFF, 0xFF}, nil},
		{nil, nil},
	} {
		if got := keyPrefixEnd(tc.in); !bytes.Equal(got, tc.want) || (got == nil) != (tc.want == nil) {
			t.Errorf("keyPrefixEnd(%x) = %x, want %x", tc.in, got, tc.want)
		}
	}
}
