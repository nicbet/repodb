package prolly

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"testing"

	"github.com/nicbet/repodb/common/storage"
)

// Chunk sizes must not depend on the key format. FNV-64a's low bits follow
// the low bits of the key's last byte, which once made sequential integer
// keys cut every 64 entries regardless of the options.
func TestChunkSizesAreKeyShapeIndependent(t *testing.T) {
	ctx := context.Background()
	const n = 50_000
	rng := rand.New(rand.NewSource(1))
	shapes := map[string]func(i int) []byte{
		"sequential bigint": func(i int) []byte {
			return binary.BigEndian.AppendUint64(nil, uint64(i)^1<<63)
		},
		"string": func(i int) []byte {
			return append([]byte(fmt.Sprintf("user-%08d@example.com", i)), 0, 1)
		},
		"random": func(int) []byte {
			b := make([]byte, 16)
			rng.Read(b)
			return b
		},
	}
	averages := map[string]float64{}
	for name, key := range shapes {
		entries := make([]Entry, n)
		for i := range entries {
			entries[i] = Entry{Key: key(i), Value: []byte("v")}
		}
		store := storage.NewMemory()
		tree, err := Build(ctx, store, entries, DefaultOptions)
		if err != nil {
			t.Fatal(err)
		}
		leaves, err := leafLinks(ctx, store, tree.Root())
		if err != nil {
			t.Fatal(err)
		}
		capped := 0
		for _, leaf := range leaves[:len(leaves)-1] { // the last leaf ends with the input
			if leaf.Count == uint64(DefaultOptions.MaxChunkEntries) {
				capped++
			}
		}
		averages[name] = float64(n) / float64(len(leaves))
		if share := float64(capped) / float64(len(leaves)); share > 0.10 {
			t.Errorf("%s: %.0f%% of leaves end at the size cap", name, 100*share)
		}
		t.Logf("%s: %d leaves, %.1f entries on average, %d at the cap", name, len(leaves), averages[name], capped)
	}
	lo, hi := averages["random"], averages["random"]
	for _, avg := range averages {
		lo, hi = min(lo, avg), max(hi, avg)
	}
	if hi > 1.25*lo {
		t.Errorf("average leaf sizes vary by key shape: %v", averages)
	}
}
