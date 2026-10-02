package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/cockroachdb/apd/v3"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
	querypb "github.com/dolthub/vitess/go/vt/proto/query"
)

// Row encoding (format v3). A row blob is
//
//	uvarint  column count
//	bytes    NULL bitmap, ceil(n/8) bytes; bit i%8 of byte i/8 set = column i is NULL
//	cells    one per non-NULL column, in schema order
//
// with each cell encoded by column type:
//
//	signed integers   zigzag varint
//	unsigned integers uvarint
//	ENUM              uvarint index
//	FLOAT             4 bytes little-endian IEEE-754 bits
//	DOUBLE            8 bytes little-endian IEEE-754 bits
//	DATE/DATETIME/    8 bytes little-endian int64 microseconds since the Unix
//	TIMESTAMP         epoch (UTC)
//	TIME              zigzag varint microseconds
//	DECIMAL           the unscaled value (value × 10^scale): tag 0 then a zigzag
//	                  varint when it fits int64, else tag 1, a sign byte, and a
//	                  uvarint-length big-endian magnitude
//	JSON              uvarint length + canonical MySQL JSON text
//	strings, binary   uvarint length + bytes
//
// Three-way merge compares row blobs byte for byte, so every encoding is a
// function of the value: equal rows always encode to equal bytes. Values reach
// encodeRow already converted to their column type, so decodeRow rebuilds the
// Go value each column type produces without calling Type.Convert (except for
// JSON, whose document has to be parsed).

const (
	decimalSmall byte = 0
	decimalBig   byte = 1
)

var errTruncatedRow = errors.New("stored row is truncated")

func encodeRow(schema sql.Schema, row sql.Row) ([]byte, error) {
	if len(row) != len(schema) {
		return nil, fmt.Errorf("row has %d values, schema has %d", len(row), len(schema))
	}
	bitmap := (len(row) + 7) / 8
	out := binary.AppendUvarint(make([]byte, 0, 16+bitmap+8*len(row)), uint64(len(row)))
	nulls := len(out)
	out = append(out, make([]byte, bitmap)...)
	for i, value := range row {
		if value == nil {
			out[nulls+i/8] |= 1 << (i % 8)
			continue
		}
		var err error
		if out, err = appendCell(out, schema[i].Type, value); err != nil {
			return nil, fmt.Errorf("column %s: %w", schema[i].Name, err)
		}
	}
	return out, nil
}

func appendCell(dst []byte, typ sql.Type, value any) ([]byte, error) {
	switch typ.Type() {
	case querypb.Type_INT8, querypb.Type_INT16, querypb.Type_INT24, querypb.Type_INT32, querypb.Type_INT64:
		v, ok := signedValue(value)
		if !ok {
			return nil, fmt.Errorf("integer value has type %T", value)
		}
		return binary.AppendVarint(dst, v), nil
	case querypb.Type_UINT8, querypb.Type_UINT16, querypb.Type_UINT24, querypb.Type_UINT32, querypb.Type_UINT64:
		v, ok := unsignedValue(value)
		if !ok {
			return nil, fmt.Errorf("unsigned value has type %T", value)
		}
		return binary.AppendUvarint(dst, v), nil
	case querypb.Type_ENUM:
		v, ok := value.(uint16)
		if !ok {
			return nil, fmt.Errorf("enum value has type %T", value)
		}
		return binary.AppendUvarint(dst, uint64(v)), nil
	case querypb.Type_FLOAT32:
		switch v := value.(type) {
		case float32:
			return binary.LittleEndian.AppendUint32(dst, math.Float32bits(v)), nil
		case float64:
			return binary.LittleEndian.AppendUint32(dst, math.Float32bits(float32(v))), nil
		}
		return nil, fmt.Errorf("float value has type %T", value)
	case querypb.Type_FLOAT64:
		switch v := value.(type) {
		case float64:
			return binary.LittleEndian.AppendUint64(dst, math.Float64bits(v)), nil
		case float32:
			return binary.LittleEndian.AppendUint64(dst, math.Float64bits(float64(v))), nil
		}
		return nil, fmt.Errorf("double value has type %T", value)
	case querypb.Type_DATE, querypb.Type_DATETIME, querypb.Type_TIMESTAMP:
		t, ok := value.(time.Time)
		if !ok {
			return nil, fmt.Errorf("datetime value has type %T", value)
		}
		return binary.LittleEndian.AppendUint64(dst, uint64(t.UnixMicro())), nil
	case querypb.Type_TIME:
		ts, ok := value.(types.Timespan)
		if !ok {
			return nil, fmt.Errorf("time value has type %T", value)
		}
		return binary.AppendVarint(dst, int64(ts)), nil
	case querypb.Type_DECIMAL:
		d, ok := value.(*apd.Decimal)
		if !ok {
			return nil, fmt.Errorf("decimal value has type %T", value)
		}
		dt, ok := typ.(sql.DecimalType)
		if !ok {
			return nil, fmt.Errorf("decimal column has type %T", typ)
		}
		return appendDecimalCell(dst, d, int32(dt.Scale()))
	case querypb.Type_JSON:
		jw, ok := value.(sql.JSONWrapper)
		if !ok {
			return nil, fmt.Errorf("json value has type %T", value)
		}
		// The context only matters for lazily loaded JSON; RepoDB values are in memory.
		s, err := types.JsonToMySqlString(context.Background(), jw)
		if err != nil {
			return nil, err
		}
		return appendBytesCell(dst, s), nil
	case querypb.Type_BLOB, querypb.Type_VARBINARY, querypb.Type_BINARY:
		switch v := value.(type) {
		case []byte:
			return appendBytesCell(dst, v), nil
		case string:
			return appendBytesCell(dst, v), nil
		}
		return nil, fmt.Errorf("binary value has type %T", value)
	default:
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("text value has type %T", value)
		}
		return appendBytesCell(dst, s), nil
	}
}

func appendBytesCell[T string | []byte](dst []byte, b T) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(b)))
	return append(dst, b...)
}

// appendDecimalCell writes d as its unscaled integer at the column scale.
// Column conversion already rounds values to that scale; rounding here as well
// keeps the encoding canonical if a value ever arrives with another exponent.
func appendDecimalCell(dst []byte, d *apd.Decimal, scale int32) ([]byte, error) {
	if d.Form != apd.Finite {
		return nil, fmt.Errorf("decimal value %s is not finite", d)
	}
	if d.Exponent != -scale {
		var err error
		if d, err = sql.DecimalRound(d, scale); err != nil {
			return nil, err
		}
	}
	negative := d.Negative && d.Coeff.Sign() != 0 // -0 encodes as 0
	if d.Coeff.IsInt64() {
		v := d.Coeff.Int64()
		if negative {
			v = -v
		}
		return binary.AppendVarint(append(dst, decimalSmall), v), nil
	}
	sign := byte(0)
	if negative {
		sign = 1
	}
	return appendBytesCell(append(dst, decimalBig, sign), d.Coeff.Bytes()), nil
}

func decodeRow(schema sql.Schema, data []byte) (sql.Row, error) {
	width, n := binary.Uvarint(data)
	if n <= 0 {
		return nil, errTruncatedRow
	}
	if width != uint64(len(schema)) {
		return nil, errors.New("stored row width does not match schema")
	}
	pos := n + (len(schema)+7)/8
	if pos > len(data) {
		return nil, errTruncatedRow
	}
	d := rowDecoder{data: data, pos: pos}
	nulls := data[n:pos]
	row := make(sql.Row, len(schema))
	for i, col := range schema {
		if nulls[i/8]&(1<<(i%8)) != 0 {
			continue
		}
		var err error
		if row[i], err = d.cell(col.Type); err != nil {
			return nil, fmt.Errorf("column %s: %w", col.Name, err)
		}
	}
	if d.pos != len(data) {
		return nil, errors.New("stored row has trailing bytes")
	}
	return row, nil
}

// rowProjection selects and orders a subset of a table's columns.
type rowProjection struct {
	ordinals []int // table column ordinals, in output order
	slot     []int // per table column: its first output position, or -1
	last     int   // highest projected ordinal, or -1
	repeats  bool  // some column is projected more than once
}

// newRowProjection returns nil when ordinals select every column in order.
func newRowProjection(width int, ordinals []int) *rowProjection {
	if len(ordinals) == width {
		identity := true
		for i, ordinal := range ordinals {
			identity = identity && ordinal == i
		}
		if identity {
			return nil
		}
	}
	p := &rowProjection{ordinals: ordinals, slot: make([]int, width), last: -1}
	for i := range p.slot {
		p.slot[i] = -1
	}
	for i, ordinal := range ordinals {
		if p.slot[ordinal] >= 0 {
			p.repeats = true
			continue
		}
		p.slot[ordinal] = i
		p.last = max(p.last, ordinal)
	}
	return p
}

// project returns the projected columns of a full row. A nil projection
// returns row itself.
func (p *rowProjection) project(row sql.Row) sql.Row {
	if p == nil || row == nil {
		return row
	}
	out := make(sql.Row, len(p.ordinals))
	for i, ordinal := range p.ordinals {
		out[i] = row[ordinal]
	}
	return out
}

// decodeRowProjected decodes only the projected columns of a stored row,
// skipping the cells of the others and stopping after the last one needed.
// With a nil projection it is decodeRow. A projection drops decodeRow's width
// and trailing-bytes checks for the cells it skips; ValidateSnapshot decodes
// every stored row in full.
func decodeRowProjected(schema sql.Schema, data []byte, p *rowProjection) (sql.Row, error) {
	if p == nil {
		return decodeRow(schema, data)
	}
	out := make(sql.Row, len(p.ordinals))
	if p.last < 0 {
		return out, nil
	}
	width, n := binary.Uvarint(data)
	if n <= 0 {
		return nil, errTruncatedRow
	}
	if width != uint64(len(schema)) {
		return nil, errors.New("stored row width does not match schema")
	}
	pos := n + (len(schema)+7)/8
	if pos > len(data) {
		return nil, errTruncatedRow
	}
	d := rowDecoder{data: data, pos: pos}
	nulls := data[n:pos]
	for i := 0; i <= p.last; i++ {
		if nulls[i/8]&(1<<(i%8)) != 0 {
			continue
		}
		col := schema[i]
		var err error
		if slot := p.slot[i]; slot >= 0 {
			out[slot], err = d.cell(col.Type)
		} else {
			err = d.skip(col.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", col.Name, err)
		}
	}
	if p.repeats {
		for i, ordinal := range p.ordinals {
			out[i] = out[p.slot[ordinal]]
		}
	}
	return out, nil
}

type rowDecoder struct {
	data []byte
	pos  int
	// text is a string copy of data, made on the first string cell so that
	// every string cell in the row shares one allocation.
	text string
}

func (d *rowDecoder) varint() (int64, error) {
	v, n := binary.Varint(d.data[d.pos:])
	if n <= 0 {
		return 0, errTruncatedRow
	}
	d.pos += n
	return v, nil
}

func (d *rowDecoder) uvarint() (uint64, error) {
	v, n := binary.Uvarint(d.data[d.pos:])
	if n <= 0 {
		return 0, errTruncatedRow
	}
	d.pos += n
	return v, nil
}

func (d *rowDecoder) fixed(size int) ([]byte, error) {
	if len(d.data)-d.pos < size {
		return nil, errTruncatedRow
	}
	b := d.data[d.pos : d.pos+size]
	d.pos += size
	return b, nil
}

// span returns the bounds of a length-prefixed cell.
func (d *rowDecoder) span() (int, int, error) {
	length, err := d.uvarint()
	if err != nil {
		return 0, 0, err
	}
	if length > uint64(len(d.data)-d.pos) {
		return 0, 0, errTruncatedRow
	}
	start := d.pos
	d.pos += int(length)
	return start, d.pos, nil
}

func (d *rowDecoder) string() (string, error) {
	start, end, err := d.span()
	if err != nil {
		return "", err
	}
	if d.text == "" {
		d.text = string(d.data)
	}
	return d.text[start:end], nil
}

func (d *rowDecoder) cell(typ sql.Type) (any, error) {
	switch t := typ.Type(); t {
	case querypb.Type_INT8, querypb.Type_INT16, querypb.Type_INT24, querypb.Type_INT32, querypb.Type_INT64:
		v, err := d.varint()
		if err != nil {
			return nil, err
		}
		switch t {
		case querypb.Type_INT8:
			return int8(v), nil
		case querypb.Type_INT16:
			return int16(v), nil
		case querypb.Type_INT24, querypb.Type_INT32:
			return int32(v), nil
		}
		return v, nil
	case querypb.Type_UINT8, querypb.Type_UINT16, querypb.Type_UINT24, querypb.Type_UINT32, querypb.Type_UINT64:
		v, err := d.uvarint()
		if err != nil {
			return nil, err
		}
		switch t {
		case querypb.Type_UINT8:
			return uint8(v), nil
		case querypb.Type_UINT16:
			return uint16(v), nil
		case querypb.Type_UINT24, querypb.Type_UINT32:
			return uint32(v), nil
		}
		return v, nil
	case querypb.Type_ENUM:
		v, err := d.uvarint()
		if err != nil {
			return nil, err
		}
		if v > math.MaxUint16 {
			return nil, errors.New("stored enum index out of range")
		}
		return uint16(v), nil
	case querypb.Type_FLOAT32:
		b, err := d.fixed(4)
		if err != nil {
			return nil, err
		}
		return math.Float32frombits(binary.LittleEndian.Uint32(b)), nil
	case querypb.Type_FLOAT64:
		b, err := d.fixed(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(b)), nil
	case querypb.Type_DATE, querypb.Type_DATETIME, querypb.Type_TIMESTAMP:
		b, err := d.fixed(8)
		if err != nil {
			return nil, err
		}
		return time.UnixMicro(int64(binary.LittleEndian.Uint64(b))).UTC(), nil
	case querypb.Type_TIME:
		v, err := d.varint()
		if err != nil {
			return nil, err
		}
		return types.Timespan(v), nil
	case querypb.Type_DECIMAL:
		dt, ok := typ.(sql.DecimalType)
		if !ok {
			return nil, fmt.Errorf("decimal column has type %T", typ)
		}
		return d.decimal(int32(dt.Scale()))
	case querypb.Type_JSON:
		s, err := d.string()
		if err != nil {
			return nil, err
		}
		v, _, err := types.JSON.Convert(context.Background(), s)
		return v, err
	case querypb.Type_BLOB, querypb.Type_VARBINARY, querypb.Type_BINARY:
		start, end, err := d.span()
		if err != nil {
			return nil, err
		}
		// Copied: callers may keep or modify the slice. make keeps an empty
		// value non-nil, distinct from NULL.
		b := make([]byte, end-start)
		copy(b, d.data[start:end])
		return b, nil
	default:
		return d.string()
	}
}

// skip passes over one cell without decoding it.
func (d *rowDecoder) skip(typ sql.Type) error {
	var err error
	switch typ.Type() {
	case querypb.Type_INT8, querypb.Type_INT16, querypb.Type_INT24, querypb.Type_INT32, querypb.Type_INT64, querypb.Type_TIME:
		_, err = d.varint()
	case querypb.Type_UINT8, querypb.Type_UINT16, querypb.Type_UINT24, querypb.Type_UINT32, querypb.Type_UINT64, querypb.Type_ENUM:
		_, err = d.uvarint()
	case querypb.Type_FLOAT32:
		_, err = d.fixed(4)
	case querypb.Type_FLOAT64, querypb.Type_DATE, querypb.Type_DATETIME, querypb.Type_TIMESTAMP:
		_, err = d.fixed(8)
	case querypb.Type_DECIMAL:
		var tag []byte
		if tag, err = d.fixed(1); err != nil {
			return err
		}
		switch tag[0] {
		case decimalSmall:
			_, err = d.varint()
		case decimalBig:
			if _, err = d.fixed(1); err == nil {
				_, _, err = d.span()
			}
		default:
			err = fmt.Errorf("unknown stored decimal form %d", tag[0])
		}
	default: // JSON, strings and binary are length-prefixed
		_, _, err = d.span()
	}
	return err
}

func (d *rowDecoder) decimal(scale int32) (*apd.Decimal, error) {
	tag, err := d.fixed(1)
	if err != nil {
		return nil, err
	}
	out := &apd.Decimal{Exponent: -scale}
	switch tag[0] {
	case decimalSmall:
		v, err := d.varint()
		if err != nil {
			return nil, err
		}
		if v < 0 {
			out.Negative = true
			out.Coeff.SetUint64(uint64(-v)) // uint64 conversion also covers MinInt64
		} else {
			out.Coeff.SetInt64(v)
		}
	case decimalBig:
		sign, err := d.fixed(1)
		if err != nil {
			return nil, err
		}
		start, end, err := d.span()
		if err != nil {
			return nil, err
		}
		out.Negative = sign[0] != 0
		out.Coeff.SetBytes(d.data[start:end])
	default:
		return nil, fmt.Errorf("unknown stored decimal form %d", tag[0])
	}
	return out, nil
}
