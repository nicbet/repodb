package engine

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
	"github.com/nicbet/repodb/common/prolly"
	"github.com/nicbet/repodb/common/storage"
)

type benchmarkTypedRecord struct {
	Version       int                   `json:"version"`
	Generation    uint64                `json:"generation"`
	SchemaChanges []benchmarkSchemaEdit `json:"schema_changes,omitempty"`
	RowChanges    []benchmarkTypedEdit  `json:"row_changes"`
}

type benchmarkSchemaEdit struct {
	Table  string `json:"table"`
	Schema []byte `json:"schema,omitempty"`
	Drop   bool   `json:"drop,omitempty"`
}

type benchmarkTypedEdit struct {
	Table  string `json:"table"`
	Key    []byte `json:"key"`
	Value  []byte `json:"value,omitempty"`
	Delete bool   `json:"delete,omitempty"`
}

// BenchmarkM43TypedEditJournal is a benchmark-only lower-bound experiment. It
// uses RepoDB's typed key/row encodings and publishes an in-memory read overlay
// only after the framed record is flushed. It deliberately excludes SQL parsing
// and is not a production persistence mode.
func BenchmarkM43TypedEditJournal(b *testing.B) {
	schema := benchmarkPrimaryKeySchema()
	crcTable := crc32.MakeTable(crc32.Castagnoli)
	for _, batch := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("batch=%d", batch), func(b *testing.B) {
			file, err := os.OpenFile(b.TempDir()+"/typed-journal", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				b.Fatal(err)
			}
			defer file.Close()
			overlay := make(map[string][]byte)
			latencies := make([]time.Duration, 0, b.N)
			var encodedBytes, encodeNanos, appendNanos, flushNanos uint64
			b.ReportAllocs()
			b.ResetTimer()
			for generation := range b.N {
				started := time.Now()
				record := benchmarkTypedRecord{Version: 1, Generation: uint64(generation + 1), RowChanges: make([]benchmarkTypedEdit, 0, batch)}
				for offset := range batch {
					row := sql.Row{int64(1 + (generation*batch+offset)%50_000), fmt.Sprintf("typed-%08d-%03d", generation, offset)}
					key, err := encodeKey(schema, row)
					if err != nil {
						b.Fatal(err)
					}
					value, err := encodeRow(schema.Schema, row)
					if err != nil {
						b.Fatal(err)
					}
					record.RowChanges = append(record.RowChanges, benchmarkTypedEdit{Table: "bench", Key: key, Value: value})
				}
				phase := time.Now()
				payload, err := json.Marshal(record)
				if err != nil {
					b.Fatal(err)
				}
				encodeNanos += uint64(time.Since(phase))
				encodedBytes += uint64(len(payload) + 12)
				header := make([]byte, 12)
				copy(header[:4], "RDBE")
				binary.BigEndian.PutUint32(header[4:8], uint32(len(payload)))
				binary.BigEndian.PutUint32(header[8:12], crc32.Checksum(payload, crcTable))
				phase = time.Now()
				if _, err := file.Write(append(header, payload...)); err != nil {
					b.Fatal(err)
				}
				appendNanos += uint64(time.Since(phase))
				phase = time.Now()
				if err := file.Sync(); err != nil {
					b.Fatal(err)
				}
				flushNanos += uint64(time.Since(phase))
				for _, edit := range record.RowChanges {
					overlay[string(edit.Key)] = append([]byte(nil), edit.Value...)
				}
				latencies = append(latencies, time.Since(started))
			}
			b.StopTimer()
			perOp := float64(b.N)
			b.ReportMetric(float64(encodedBytes)/perOp, "journal-bytes/op")
			b.ReportMetric(float64(encodeNanos)/perOp, "encode-ns/op")
			b.ReportMetric(float64(appendNanos)/perOp, "append-ns/op")
			b.ReportMetric(float64(flushNanos)/perOp, "flush-ns/op")
			reportRequestLatency(b, latencies)
			if len(overlay) == 0 {
				b.Fatal("typed overlay was not published")
			}
		})
	}
}

func BenchmarkM43TypedEditCheckpoint(b *testing.B) {
	ctx := context.Background()
	schema := benchmarkPrimaryKeySchema()
	for _, rows := range []int{1_000, 10_000, 50_000} {
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			store := storage.NewMemory()
			entries := make([]prolly.Entry, 0, rows)
			for id := 1; id <= rows; id++ {
				row := sql.Row{int64(id), "base"}
				key, _ := encodeKey(schema, row)
				value, _ := encodeRow(schema.Schema, row)
				entries = append(entries, prolly.Entry{Key: key, Value: value})
			}
			base, err := prolly.Build(ctx, store, entries, prolly.DefaultOptions)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := range b.N {
				edits := make([]prolly.Edit, 0, 100)
				for offset := range 100 {
					row := sql.Row{int64(1 + offset%rows), fmt.Sprintf("checkpoint-%08d", iteration)}
					key, _ := encodeKey(schema, row)
					value, _ := encodeRow(schema.Schema, row)
					edits = append(edits, prolly.Edit{Key: key, Value: value})
				}
				tree, err := prolly.Apply(ctx, store, base, edits)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := prolly.Reachable(ctx, store, tree.Root()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchmarkPrimaryKeySchema() sql.PrimaryKeySchema {
	return sql.PrimaryKeySchema{Schema: sql.Schema{
		&sql.Column{Name: "id", Type: types.Int64, Nullable: false, PrimaryKey: true},
		&sql.Column{Name: "value", Type: types.Text, Nullable: false},
	}, PkOrdinals: []int{0}}
}

func reportRequestLatency(b *testing.B, latencies []time.Duration) {
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	if len(latencies) == 0 {
		return
	}
	b.ReportMetric(float64(latencies[len(latencies)/2]), "request-p50-ns")
	b.ReportMetric(float64(latencies[(len(latencies)*95-1)/100]), "request-p95-ns")
	b.ReportMetric(float64(latencies[len(latencies)-1]), "request-max-ns")
}
