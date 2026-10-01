package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cockroachdb/apd/v3"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
	querypb "github.com/dolthub/vitess/go/vt/proto/query"
)

// Key encoding (format v2). Primary-key and secondary-index keys are the
// concatenation of one self-delimiting encoding per column, chosen so that
// bytes.Compare on two keys agrees with SQL ordering of the column values.
// That lets range scans and ORDER BY seek into the Prolly trees directly.
// Keys are never decoded: row values live in the row blob.
//
//	signed integers   8 bytes big-endian, sign bit flipped
//	unsigned integers 8 bytes big-endian
//	ENUM              2 bytes big-endian index (MySQL orders ENUM by index)
//	FLOAT/DOUBLE      8 bytes: IEEE-754 bits, all flipped if negative, else the
//	                  sign bit flipped; -0 is normalized to +0
//	DECIMAL           sign class byte, then (for non-zero) a sign-flipped
//	                  4-byte decimal exponent and digit bytes ending in 0x00;
//	                  bitwise inverted for negative values
//	DATE/DATETIME/    8 bytes: microseconds since the Unix epoch (UTC),
//	TIMESTAMP, TIME   sign bit flipped
//	strings, binary   bytes (per-rune collation weights for non-binary string
//	                  collations) with 0x00 escaped as 0x00 0xFF and a
//	                  0x00 0x01 terminator

const (
	decimalNegative byte = 0x01
	decimalZero     byte = 0x02
	decimalPositive byte = 0x03
)

// keyable reports whether appendKeyColumn can encode values of typ. Keep it in
// step with appendKeyColumn's cases.
func keyable(typ sql.Type) bool {
	switch typ.Type() {
	case querypb.Type_INT8, querypb.Type_INT16, querypb.Type_INT24, querypb.Type_INT32, querypb.Type_INT64,
		querypb.Type_UINT8, querypb.Type_UINT16, querypb.Type_UINT24, querypb.Type_UINT32, querypb.Type_UINT64,
		querypb.Type_FLOAT32, querypb.Type_FLOAT64,
		querypb.Type_DECIMAL,
		querypb.Type_DATE, querypb.Type_DATETIME, querypb.Type_TIMESTAMP,
		querypb.Type_TIME,
		querypb.Type_ENUM,
		querypb.Type_BLOB, querypb.Type_VARBINARY, querypb.Type_BINARY,
		querypb.Type_VARCHAR, querypb.Type_CHAR, querypb.Type_TEXT:
		return true
	}
	return false
}

// validateKeyColumns rejects primary-key columns whose type cannot be encoded
// as a key, so CREATE TABLE fails instead of every later write. go-mysql-server
// itself rejects JSON in composite keys, indexes and ALTER TABLE, but not a
// single-column JSON PRIMARY KEY.
func validateKeyColumns(schema sql.Schema, ordinals []int) error {
	for _, ord := range ordinals {
		col := schema[ord]
		if !keyable(col.Type) {
			return fmt.Errorf("column %s of type %s cannot be part of a key", col.Name, col.Type)
		}
	}
	return nil
}

// appendKeyColumn appends the order-preserving encoding of one non-NULL value
// of the given column type.
func appendKeyColumn(dst []byte, typ sql.Type, value any) ([]byte, error) {
	switch typ.Type() {
	case querypb.Type_INT8, querypb.Type_INT16, querypb.Type_INT24, querypb.Type_INT32, querypb.Type_INT64:
		v, ok := signedValue(value)
		if !ok {
			return nil, fmt.Errorf("integer key value has type %T", value)
		}
		return binary.BigEndian.AppendUint64(dst, uint64(v)^(1<<63)), nil
	case querypb.Type_UINT8, querypb.Type_UINT16, querypb.Type_UINT24, querypb.Type_UINT32, querypb.Type_UINT64:
		v, ok := unsignedValue(value)
		if !ok {
			return nil, fmt.Errorf("unsigned key value has type %T", value)
		}
		return binary.BigEndian.AppendUint64(dst, v), nil
	case querypb.Type_FLOAT32, querypb.Type_FLOAT64:
		var f float64
		switch v := value.(type) {
		case float32:
			f = float64(v)
		case float64:
			f = v
		default:
			return nil, fmt.Errorf("float key value has type %T", value)
		}
		if math.IsNaN(f) {
			return nil, errors.New("NaN cannot be a key value")
		}
		if f == 0 {
			f = 0 // normalize -0
		}
		bits := math.Float64bits(f)
		if bits&(1<<63) != 0 {
			bits = ^bits
		} else {
			bits |= 1 << 63
		}
		return binary.BigEndian.AppendUint64(dst, bits), nil
	case querypb.Type_DECIMAL:
		d, ok := value.(*apd.Decimal)
		if !ok {
			return nil, fmt.Errorf("decimal key value has type %T", value)
		}
		return appendDecimalKey(dst, d)
	case querypb.Type_DATE, querypb.Type_DATETIME, querypb.Type_TIMESTAMP:
		t, ok := value.(time.Time)
		if !ok {
			return nil, fmt.Errorf("datetime key value has type %T", value)
		}
		return binary.BigEndian.AppendUint64(dst, uint64(t.UnixMicro())^(1<<63)), nil
	case querypb.Type_TIME:
		ts, ok := value.(types.Timespan)
		if !ok {
			return nil, fmt.Errorf("time key value has type %T", value)
		}
		return binary.BigEndian.AppendUint64(dst, uint64(int64(ts))^(1<<63)), nil
	case querypb.Type_ENUM:
		v, ok := value.(uint16)
		if !ok {
			return nil, fmt.Errorf("enum key value has type %T", value)
		}
		return binary.BigEndian.AppendUint16(dst, v), nil
	case querypb.Type_BLOB, querypb.Type_VARBINARY, querypb.Type_BINARY:
		switch v := value.(type) {
		case []byte:
			return appendEscaped(dst, v), nil
		case string:
			return appendEscaped(dst, []byte(v)), nil
		default:
			return nil, fmt.Errorf("binary key value has type %T", value)
		}
	case querypb.Type_VARCHAR, querypb.Type_CHAR, querypb.Type_TEXT:
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("string key value has type %T", value)
		}
		return appendStringKey(dst, typ, s)
	default:
		return nil, fmt.Errorf("column type %s cannot be part of a key", typ)
	}
}

// appendStringKey mirrors go-mysql-server's StringType.Compare: CHAR values
// ignore trailing spaces, then strings compare rune by rune by collation
// weight, and a prefix sorts first. For the default binary collation the
// weight is the code point, which UTF-8 bytes already order, so the bytes are
// used directly. Other collations write each rune's weight as 4 sign-flipped
// big-endian bytes. (WriteWeightString is little-endian, fit for hashing and
// equality but not ordering.)
func appendStringKey(dst []byte, typ sql.Type, s string) ([]byte, error) {
	if types.IsChar(typ) {
		s = strings.TrimRight(s, " ")
	}
	collation := sql.Collation_Default
	if st, ok := typ.(sql.StringType); ok {
		collation = st.Collation()
	}
	if collation == sql.Collation_Default || collation == sql.Collation_binary {
		return appendEscaped(dst, []byte(s)), nil
	}
	weight := collation.Sorter()
	if weight == nil {
		return nil, fmt.Errorf("collation %s has no sort order", collation)
	}
	raw := make([]byte, 0, len(s)*4)
	for len(s) > 0 {
		r, n := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && n <= 1 {
			return nil, fmt.Errorf("malformed string in %s key", collation)
		}
		raw = binary.BigEndian.AppendUint32(raw, uint32(weight(r))^(1<<31))
		s = s[n:]
	}
	return appendEscaped(dst, raw), nil
}

func signedValue(value any) (int64, bool) {
	switch v := value.(type) {
	case int8:
		return int64(v), true
	case int16:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	}
	return 0, false
}

func unsignedValue(value any) (uint64, bool) {
	switch v := value.(type) {
	case uint8:
		return uint64(v), true
	case uint16:
		return uint64(v), true
	case uint32:
		return uint64(v), true
	case uint64:
		return v, true
	case uint:
		return uint64(v), true
	}
	return 0, false
}

// appendEscaped writes b with 0x00 escaped as 0x00 0xFF, then the 0x00 0x01
// terminator. A string therefore sorts before every string it prefixes, and
// the encoding is self-delimiting inside composite keys.
func appendEscaped(dst, b []byte) []byte {
	for _, c := range b {
		if c == 0x00 {
			dst = append(dst, 0x00, 0xFF)
			continue
		}
		dst = append(dst, c)
	}
	return append(dst, 0x00, 0x01)
}

// appendDecimalKey encodes a finite decimal as value = 0.d1d2…dn × 10^exp with
// d1 != 0 and no trailing zeros, so equal values (1.0, 1.00) encode equally.
// Positive: class, sign-flipped exponent, digits stored as digit+1, 0x00.
// Negative: the same bytes after the class, bitwise inverted, so larger
// magnitudes sort first. Digit bytes are 0x01–0x0A, so neither terminator
// (0x00, or 0xFF when inverted) can occur inside the digits.
func appendDecimalKey(dst []byte, d *apd.Decimal) ([]byte, error) {
	if d.Form != apd.Finite {
		return nil, errors.New("non-finite decimal cannot be a key value")
	}
	if d.IsZero() {
		return append(dst, decimalZero), nil
	}
	digits := d.Coeff.String()
	if digits[0] == '-' {
		digits = digits[1:]
	}
	exponent := int64(d.Exponent) + int64(len(digits))
	end := len(digits)
	for end > 0 && digits[end-1] == '0' {
		end--
	}
	digits = digits[:end]
	if exponent < math.MinInt32 || exponent > math.MaxInt32 {
		return nil, errors.New("decimal exponent out of range for a key")
	}
	body := binary.BigEndian.AppendUint32(nil, uint32(int32(exponent))^(1<<31))
	for i := 0; i < len(digits); i++ {
		body = append(body, digits[i]-'0'+1)
	}
	body = append(body, 0x00)
	if d.Negative {
		dst = append(dst, decimalNegative)
		for _, c := range body {
			dst = append(dst, ^c)
		}
		return dst, nil
	}
	dst = append(dst, decimalPositive)
	return append(dst, body...), nil
}

// keyPrefixEnd returns the smallest key greater than every key that starts
// with prefix, or nil if there is none (prefix is empty or all 0xFF).
func keyPrefixEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] != 0xFF {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}
